package gateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/monitor"
)

type rtkStatsDailyPoint struct {
	Date             string `json:"date"`
	Commands         int64  `json:"commands"`
	OriginalTokens   int64  `json:"originalTokens"`
	CompressedTokens int64  `json:"compressedTokens"`
	SavedTokens      int64  `json:"savedTokens"`
}

type rtkProviderStats struct {
	ProviderID       string  `json:"providerId"`
	Commands         int64   `json:"commands"`
	OriginalTokens   int64   `json:"originalTokens"`
	CompressedTokens int64   `json:"compressedTokens"`
	SavedTokens      int64   `json:"savedTokens"`
	SavedPercent     float64 `json:"savedPercent"`
}

type rtkStatsResponse struct {
	Available        bool                 `json:"available"`
	Version          string               `json:"version,omitempty"`
	Days             int                  `json:"days"`
	From             string               `json:"from,omitempty"`
	Commands         int64                `json:"commands"`
	OriginalBytes    int64                `json:"originalBytes"`
	CompressedBytes  int64                `json:"compressedBytes"`
	OriginalTokens   int64                `json:"originalTokens"`
	CompressedTokens int64                `json:"compressedTokens"`
	SavedTokens      int64                `json:"savedTokens"`
	SavedPercent     float64              `json:"savedPercent"`
	Daily            []rtkStatsDailyPoint `json:"daily"`
	ByProvider       []rtkProviderStats   `json:"byProvider,omitempty"`
}

// handleRTKStats returns gateway-side compression savings. Unlike rtk gain,
// these numbers are measured on the exact payloads this gateway forwarded, so
// they cover every route/key hitting an RTK-enabled provider.
func (s *Server) handleRTKStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}

	days := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		if raw == "all" {
			days = 0
		} else if parsed, err := strconv.Atoi(raw); err == nil {
			days = parsed
		}
	}
	if days < 0 {
		days = 0
	}
	if days > 3650 {
		days = 3650
	}

	now := time.Now().Local()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	endExclusive := today.AddDate(0, 0, 1)
	var from time.Time
	if days > 0 {
		from = today.AddDate(0, 0, -(days - 1))
	}

	available, version := false, ""
	if s.rtk != nil {
		available, version = s.rtk.status()
	}
	response := rtkStatsResponse{
		Available: available,
		Version:   version,
		Days:      days,
		Daily:     []rtkStatsDailyPoint{},
	}

	rows := []monitor.RTKDayStats{}
	if s.rtkStatsStore != nil {
		loaded, err := s.rtkStatsStore.LoadRTKStats(from, endExclusive)
		if err != nil {
			s.logs.AddApp("warn", "load rtk stats failed", err.Error())
		} else {
			rows = loaded
		}
	}

	byDay := make(map[string]*rtkStatsDailyPoint)
	byProvider := make(map[string]*rtkProviderStats)
	for _, row := range rows {
		response.Commands += row.Commands
		response.OriginalBytes += row.OriginalBytes
		response.CompressedBytes += row.CompressedBytes
		response.OriginalTokens += row.OriginalTokens
		response.CompressedTokens += row.CompressedTokens

		point, ok := byDay[row.Date]
		if !ok {
			point = &rtkStatsDailyPoint{Date: row.Date}
			byDay[row.Date] = point
		}
		point.Commands += row.Commands
		point.OriginalTokens += row.OriginalTokens
		point.CompressedTokens += row.CompressedTokens

		provider, ok := byProvider[row.ProviderID]
		if !ok {
			provider = &rtkProviderStats{ProviderID: row.ProviderID}
			byProvider[row.ProviderID] = provider
		}
		provider.Commands += row.Commands
		provider.OriginalTokens += row.OriginalTokens
		provider.CompressedTokens += row.CompressedTokens
	}
	response.SavedTokens = response.OriginalTokens - response.CompressedTokens
	if response.SavedTokens < 0 {
		response.SavedTokens = 0
	}
	if response.OriginalTokens > 0 {
		response.SavedPercent = float64(response.SavedTokens) / float64(response.OriginalTokens) * 100
	}

	chartStart := today
	if days > 0 {
		chartStart = from
		response.From = from.Format("2006-01-02")
	} else if len(rows) > 0 {
		if earliest, err := time.ParseInLocation("2006-01-02", rows[0].Date, now.Location()); err == nil {
			chartStart = earliest
		}
		// "全部" keeps the totals over the whole history but caps the chart at
		// one year so a years-old install does not render thousands of bars.
		if earliest := today.AddDate(0, 0, -365); chartStart.Before(earliest) {
			chartStart = earliest
		}
	}
	for day := chartStart; !day.After(today); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		point := rtkStatsDailyPoint{Date: key}
		if stored, ok := byDay[key]; ok {
			point = *stored
		}
		point.SavedTokens = point.OriginalTokens - point.CompressedTokens
		if point.SavedTokens < 0 {
			point.SavedTokens = 0
		}
		response.Daily = append(response.Daily, point)
	}

	for _, provider := range byProvider {
		provider.SavedTokens = provider.OriginalTokens - provider.CompressedTokens
		if provider.SavedTokens < 0 {
			provider.SavedTokens = 0
		}
		if provider.OriginalTokens > 0 {
			provider.SavedPercent = float64(provider.SavedTokens) / float64(provider.OriginalTokens) * 100
		}
		response.ByProvider = append(response.ByProvider, *provider)
	}
	sort.Slice(response.ByProvider, func(i, j int) bool {
		if response.ByProvider[i].SavedTokens == response.ByProvider[j].SavedTokens {
			return response.ByProvider[i].ProviderID < response.ByProvider[j].ProviderID
		}
		return response.ByProvider[i].SavedTokens > response.ByProvider[j].SavedTokens
	})

	writeJSON(w, http.StatusOK, response)
}

// recordRTKStats persists one request's compression savings right before the
// compressed payload is forwarded. The write is a single synchronous UPSERT,
// so the added latency is negligible compared with the rtk spawn that just
// produced these numbers.
func (s *Server) recordRTKStats(providerID string, stats rtkCompressionStats) {
	if s.rtkStatsStore == nil || stats.Commands == 0 {
		return
	}
	delta := monitor.RTKPersistDelta{
		Day:              time.Now().Local().Format("2006-01-02"),
		ProviderID:       providerID,
		Commands:         stats.Commands,
		OriginalBytes:    stats.OriginalBytes,
		CompressedBytes:  stats.CompressedBytes,
		OriginalTokens:   stats.OriginalTokens,
		CompressedTokens: stats.CompressedTokens,
	}
	if err := s.rtkStatsStore.ApplyRTKDelta(delta); err != nil {
		s.logs.AddApp("warn", "persist rtk savings failed", err.Error())
	}
}

// handleSetProviderRTK is the admin-only provider switch for tool-result
// compression. It is a dedicated endpoint (like /enabled) so the partial
// provider update path cannot accidentally clear the flag.
func (s *Server) handleSetProviderRTK(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	var payload struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Enabled == nil {
		writeOpenAIError(w, http.StatusBadRequest, "body must be {\"enabled\": true|false}")
		return
	}
	if *payload.Enabled {
		if available, _ := s.rtk.status(); !available {
			writeOpenAIError(w, http.StatusConflict, "rtk CLI not found on the gateway host; install rtk or set GATEWAY_RTK_PATH, then retry")
			return
		}
	}
	updated, err := s.router.SetProviderRTKCompress(r.PathValue("id"), *payload.Enabled)
	if err != nil {
		writeOpenAIError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.saveState(); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "failed to save configuration: "+err.Error())
		return
	}
	if *payload.Enabled {
		s.logs.AddApp("info", "provider rtk compression enabled", updated.ID)
	} else {
		s.logs.AddApp("info", "provider rtk compression disabled", updated.ID)
	}
	writeJSON(w, http.StatusOK, redactProviderForClient(updated))
}
