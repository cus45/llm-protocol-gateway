package gateway

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// The gateway's own cost ledger.
//
// Prices here are what the gateway charges itself for a request's tokens, taken
// from the vendor's published rate card and entered by the operator. They are
// not the vendor's invoice, and they are deliberately not fetched from a third
// party catalogue: most of what this gateway fronts (coding plans, subscription
// logins, domestic relays) is not in any public per-token table, and a guessed
// price is worse than an absent one — an absent price is reported as 未计价,
// while a wrong one is silently summed into a total someone then acts on.
//
// Cost is computed when the request finishes and stored per request and per day
// aggregate, so editing a price later never rewrites what a past request cost.

// PriceStore is the persistence the ledger needs; *store.Store satisfies it.
type PriceStore interface {
	LoadModelPrices() ([]domain.ModelPrice, error)
	SaveModelPrice(domain.ModelPrice) error
	DeleteModelPrice(providerID, modelID string) error
}

// priceTable is the lookup cache. One map read per request, never a query: the
// alternative is a SQLite read on the hot path for a number that changes a few
// times a year.
type priceTable struct {
	mu     sync.RWMutex
	rows   map[string]domain.ModelPrice
	loaded bool
	// generation counts invalidations. A load reads it before querying the
	// store and only stores its result if nothing invalidated in between: the
	// rows it read are then still the current ones. Without it, an admin's save
	// that landed while a cold load was in flight was undone by that load —
	// stale rates for the life of the process, silently.
	generation uint64
	// loading is closed when the load now in flight finishes, so calls that
	// arrive meanwhile wait for it instead of each querying the store.
	loading chan struct{}
}

func priceKey(providerID, modelID string) string {
	return strings.TrimSpace(providerID) + "\x00" + strings.TrimSpace(modelID)
}

func (s *Server) SetPriceStore(store PriceStore) {
	if s == nil {
		return
	}
	s.priceStore = store
	s.prices = &priceTable{rows: map[string]domain.ModelPrice{}}
}

// priceRows returns the cached table, loading it once on first use. A store that
// cannot be read is reported through the API rather than silently pricing
// everything at zero.
func (s *Server) priceRows() (map[string]domain.ModelPrice, error) {
	if s == nil || s.prices == nil {
		return nil, errors.New("price table not configured")
	}
	for {
		s.prices.mu.RLock()
		loaded, rows, inFlight := s.prices.loaded, s.prices.rows, s.prices.loading
		s.prices.mu.RUnlock()
		if loaded {
			return rows, nil
		}
		if s.priceStore == nil {
			return rows, nil
		}
		if inFlight != nil {
			// Another request is loading the table: its result is ours too
			// (or it was invalidated again, and the loop loads once more).
			<-inFlight
			continue
		}
		s.prices.mu.Lock()
		if s.prices.loaded {
			rows := s.prices.rows
			s.prices.mu.Unlock()
			return rows, nil
		}
		if s.prices.loading != nil {
			inFlight := s.prices.loading
			s.prices.mu.Unlock()
			<-inFlight
			continue
		}
		generation := s.prices.generation
		done := make(chan struct{})
		s.prices.loading = done
		s.prices.mu.Unlock()

		stored, err := s.priceStore.LoadModelPrices()
		indexed := make(map[string]domain.ModelPrice, len(stored))
		for _, price := range stored {
			indexed[priceKey(price.ProviderID, price.ModelID)] = price
		}

		s.prices.mu.Lock()
		s.prices.loading = nil
		// Only a load nothing invalidated may be cached: otherwise the rows it
		// read may predate the write that invalidated the table, and storing them
		// would undo that write for the life of the process.
		current := err == nil && s.prices.generation == generation
		if current {
			s.prices.rows = indexed
			s.prices.loaded = true
		}
		s.prices.mu.Unlock()
		close(done)
		if err != nil {
			return nil, err
		}
		// A load that raced a write still answers this caller with what the store
		// held a moment ago — a faithful answer to its question — but the next
		// caller loads again.
		return indexed, nil
	}
}

// lookupPrice finds the row that prices one model, in this order:
//
//  1. this provider's exact model row
//  2. this provider's "*" default row
//  3. the global exact model row
//  4. the global "*" default row
//
// A provider row is a deliberate override of the shared rate card, so it wins
// even when it is only a catch-all. Official Claude accounts, relays and other
// providers of the same model otherwise share one global row instead of each
// needing its own copy.
func (s *Server) lookupPrice(providerID, model string) (domain.ModelPrice, bool) {
	rows, err := s.priceRows()
	if err != nil || len(rows) == 0 {
		return domain.ModelPrice{}, false
	}
	model = upstreamModelForPricing(model)
	for _, scope := range []string{providerID, globalPriceProvider} {
		if scope == "" {
			continue
		}
		if model != "" {
			if price, ok := rows[priceKey(scope, model)]; ok {
				return price, true
			}
		}
		if price, ok := rows[priceKey(scope, storePriceWildcard)]; ok {
			return price, true
		}
	}
	return domain.ModelPrice{}, false
}

// storePriceWildcard mirrors store.PriceWildcard without importing store here.
const storePriceWildcard = "*"

// globalPriceProvider is the provider id that means "every provider without a
// row of its own". The price table's scope picker writes rows under it.
const globalPriceProvider = "*"

// costOf prices one request's usage. It returns priced=false when no row covers
// the model, so callers can tell "free" from "we do not know".
//
// The subtraction is the whole point: InputTokens already contains the cache
// reads and the cache writes (see TokenUsage), so pricing the full input at the
// uncached rate and then adding the cache tiers on top would bill those tokens
// twice.
func (s *Server) costOf(providerID, model string, usage TokenUsage) (int64, bool) {
	price, ok := s.lookupPrice(providerID, model)
	if !ok {
		return 0, false
	}
	cached := usage.CacheTokens
	if cached < 0 {
		cached = 0
	}
	if cached > usage.InputTokens {
		cached = usage.InputTokens
	}
	writes := usage.CacheWriteTokens
	if writes < 0 {
		writes = 0
	}
	if writes > usage.InputTokens-cached {
		writes = usage.InputTokens - cached
	}
	plain := usage.InputTokens - cached - writes
	// Rates are per million tokens, so the product is a fraction of a dollar;
	// rounding to whole micros keeps a day's sum equal to the sum of its
	// requests instead of drifting with each display rounding.
	costUSD := (float64(plain)*price.InputUSDPerMTok +
		float64(usage.OutputTokens)*price.OutputUSDPerMTok +
		float64(cached)*price.CacheReadUSDPerMTok +
		float64(writes)*price.CacheWriteUSDPerMTok) / 1e6
	cost := int64(math.Round(costUSD * 1e6))
	if cost < 0 {
		cost = 0
	}
	return cost, true
}

// upstreamModelForPricing mirrors the first step of resolveRequestLogModelForUsage:
// the logged model is the upstream id, or the historical "alias -> upstream"
// pair whose right side is what the vendor actually billed.
func upstreamModelForPricing(loggedModel string) string {
	raw := strings.TrimSpace(loggedModel)
	if _, right, ok := strings.Cut(raw, "->"); ok {
		if real := strings.TrimSpace(right); real != "" {
			return real
		}
	}
	return raw
}

// allRatesZero reports a row that prices every token tier at zero. -0 counts as
// zero; a negative rate is not zero and is rejected by the range check first.
func priceRowIsZero(p domain.ModelPrice) bool {
	for _, rate := range []float64{p.InputUSDPerMTok, p.OutputUSDPerMTok, p.CacheReadUSDPerMTok, p.CacheWriteUSDPerMTok} {
		if rate != 0 {
			return false
		}
	}
	return true
}

// ── console API ──────────────────────────────────────────────────────────────

func (s *Server) handleListPrices(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	rows, err := s.priceRows()
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to read price table: "+err.Error())
		return
	}
	prices := make([]domain.ModelPrice, 0, len(rows))
	for _, price := range rows {
		prices = append(prices, price)
	}
	sort.Slice(prices, func(i, j int) bool {
		if prices[i].ProviderID != prices[j].ProviderID {
			return prices[i].ProviderID < prices[j].ProviderID
		}
		return prices[i].ModelID < prices[j].ModelID
	})
	writeJSON(w, http.StatusOK, map[string]any{"prices": prices})
}

func (s *Server) handleSavePrice(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if s.priceStore == nil {
		writeOpenAIError(w, http.StatusConflict, "price table requires the SQLite store")
		return
	}
	var price domain.ModelPrice
	if err := json.NewDecoder(r.Body).Decode(&price); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid price json: "+err.Error())
		return
	}
	price.ProviderID = strings.TrimSpace(price.ProviderID)
	price.ModelID = strings.TrimSpace(price.ModelID)
	if price.ProviderID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "price needs providerId")
		return
	}
	if price.ModelID == "" {
		price.ModelID = storePriceWildcard
	}
	if price.ProviderID != globalPriceProvider {
		if _, err := s.router.ProviderByID(price.ProviderID); err != nil {
			writeOpenAIError(w, http.StatusNotFound, "provider "+price.ProviderID+" not found")
			return
		}
	}
	for _, rate := range []float64{price.InputUSDPerMTok, price.OutputUSDPerMTok, price.CacheReadUSDPerMTok, price.CacheWriteUSDPerMTok} {
		if rate < 0 {
			writeOpenAIError(w, http.StatusBadRequest, "rates can not be negative; omit the row to leave a model unpriced")
			return
		}
	}
	// An all-zero global row is refused. It is the last row lookupPrice tries
	// before giving up, so it would turn every model without a price of its own
	// into "priced at $0" and erase the 未计价 signal the ledger relies on — a
	// mistyped save would silently zero the whole cost total. A global row is
	// still allowed when it carries real rates. A provider's own exact row or its
	// "*" row may be all zero on purpose: it is scoped to one provider, and one of
	// the four rates may legitimately be 0 (a model with no cache write fee).
	// A global exact row is scoped to one model but still overrides nothing less
	// specific, so it follows the same rule as the global catch-all.
	if price.ProviderID == globalPriceProvider && priceRowIsZero(price) {
		writeOpenAIError(w, http.StatusBadRequest,
			"a global price row needs at least one non-zero rate; an all-zero global row would mark every unpriced model as priced at $0. Omit the row to leave models unpriced")
		return
	}
	if err := s.priceStore.SaveModelPrice(price); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save price: "+err.Error())
		return
	}
	s.invalidatePrices()
	writeJSON(w, http.StatusOK, price)
}

func (s *Server) handleDeletePrice(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	if s.priceStore == nil {
		writeOpenAIError(w, http.StatusConflict, "price table requires the SQLite store")
		return
	}
	providerID := strings.TrimSpace(r.PathValue("providerID"))
	modelID := strings.TrimSpace(r.PathValue("modelID"))
	if providerID == "" {
		writeOpenAIError(w, http.StatusBadRequest, "missing provider id")
		return
	}
	if modelID == "" {
		modelID = storePriceWildcard
	}
	if err := s.priceStore.DeleteModelPrice(providerID, modelID); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to delete price: "+err.Error())
		return
	}
	s.invalidatePrices()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// invalidatePrices drops the cache after a write. The next request reloads the
// whole table, which is fine at this size and avoids keeping two copies in sync.
func (s *Server) invalidatePrices() {
	if s.prices == nil {
		return
	}
	s.prices.mu.Lock()
	s.prices.rows = nil
	s.prices.loaded = false
	s.prices.generation++
	s.prices.mu.Unlock()
}
