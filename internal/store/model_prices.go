package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// model_prices holds the gateway's own idea of what a model costs, in US dollars
// per million tokens. It lives in its own table rather than in a column of
// `models` because a price row can exist for a model the provider listing never
// returned, and because saving a price must not have to rewrite a provider's
// whole model list (which "获取模型" does).

func ensureModelPricesTable(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS model_prices (
		provider_id TEXT NOT NULL,
		model_id TEXT NOT NULL,
		input_usd_per_mtok REAL NOT NULL DEFAULT 0,
		output_usd_per_mtok REAL NOT NULL DEFAULT 0,
		cache_read_usd_per_mtok REAL NOT NULL DEFAULT 0,
		cache_write_usd_per_mtok REAL NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (provider_id, model_id)
	)`)
	return err
}

// PriceWildcard is the model id meaning "every model of this provider that has
// no row of its own".
const PriceWildcard = "*"

func (s *Store) LoadModelPrices() ([]domain.ModelPrice, error) {
	rows, err := s.reader().Query(`SELECT provider_id, model_id, input_usd_per_mtok, output_usd_per_mtok,
		cache_read_usd_per_mtok, cache_write_usd_per_mtok, updated_at
		FROM model_prices ORDER BY provider_id, model_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	prices := []domain.ModelPrice{}
	for rows.Next() {
		var price domain.ModelPrice
		if err := rows.Scan(&price.ProviderID, &price.ModelID, &price.InputUSDPerMTok, &price.OutputUSDPerMTok,
			&price.CacheReadUSDPerMTok, &price.CacheWriteUSDPerMTok, &price.UpdatedAt); err != nil {
			return nil, err
		}
		prices = append(prices, price)
	}
	return prices, rows.Err()
}

// SaveModelPrice upserts one row. Rates are rounded to 1e-8 USD/token so a
// typo like 0.0000000000001 cannot make a model look free-but-infinite.
func (s *Store) SaveModelPrice(price domain.ModelPrice) error {
	providerID := strings.TrimSpace(price.ProviderID)
	modelID := strings.TrimSpace(price.ModelID)
	if providerID == "" || modelID == "" {
		return fmt.Errorf("model price needs both providerId and modelId")
	}
	_, err := s.db.Exec(`INSERT INTO model_prices
		(provider_id, model_id, input_usd_per_mtok, output_usd_per_mtok, cache_read_usd_per_mtok, cache_write_usd_per_mtok, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id, model_id) DO UPDATE SET
			input_usd_per_mtok = excluded.input_usd_per_mtok,
			output_usd_per_mtok = excluded.output_usd_per_mtok,
			cache_read_usd_per_mtok = excluded.cache_read_usd_per_mtok,
			cache_write_usd_per_mtok = excluded.cache_write_usd_per_mtok,
			updated_at = excluded.updated_at`,
		providerID, modelID, roundRate(price.InputUSDPerMTok), roundRate(price.OutputUSDPerMTok),
		roundRate(price.CacheReadUSDPerMTok), roundRate(price.CacheWriteUSDPerMTok),
		time.Now().UTC().Format(time.RFC3339))
	return err
}

func (s *Store) DeleteModelPrice(providerID, modelID string) error {
	_, err := s.db.Exec(`DELETE FROM model_prices WHERE provider_id = ? AND model_id = ?`,
		strings.TrimSpace(providerID), strings.TrimSpace(modelID))
	return err
}

// pruneModelPrices forgets prices whose provider is gone. Save() rewrites the
// provider table wholesale, so it is the one place that can tell "deleted" from
// "not loaded"; a stale row must not survive to price a future provider that
// happens to reuse the id.
func pruneModelPrices(tx *sql.Tx, providers []domain.Provider) error {
	keep := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		if id := strings.TrimSpace(provider.ID); id != "" {
			keep[id] = struct{}{}
		}
	}
	rows, err := tx.Query(`SELECT DISTINCT provider_id FROM model_prices`)
	if err != nil {
		return err
	}
	stale := []string{}
	for rows.Next() {
		var providerID string
		if err := rows.Scan(&providerID); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := keep[providerID]; !ok {
			stale = append(stale, providerID)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, providerID := range stale {
		if _, err := tx.Exec(`DELETE FROM model_prices WHERE provider_id = ?`, providerID); err != nil {
			return err
		}
	}
	return nil
}

func roundRate(value float64) float64 {
	if value <= 0 {
		return 0
	}
	return float64(int64(value*1e8+0.5)) / 1e8
}

// boolToInt stores Go bools in the INTEGER NOT NULL DEFAULT 0 columns SQLite
// has no boolean type for.
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
