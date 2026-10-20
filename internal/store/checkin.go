package store

import (
	"database/sql"
	"strings"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

// ensureCheckinTable keeps the latest daily check-in per provider.
//
// One row per provider, replaced on every answer: the console only ever asks
// "is today's in, and what did it give", and the vendor decides what a day is,
// so a history table would hold nothing the user could read as a trend.
func ensureCheckinTable(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS checkins (
		provider_id TEXT PRIMARY KEY,
		account_key TEXT NOT NULL DEFAULT '',
		day TEXT NOT NULL DEFAULT '',
		at TEXT NOT NULL DEFAULT '',
		outcome TEXT NOT NULL DEFAULT '',
		credit REAL NOT NULL DEFAULT 0,
		streak INTEGER NOT NULL DEFAULT 0,
		msg TEXT NOT NULL DEFAULT '',
		valid_until TEXT NOT NULL DEFAULT '',
		offline INTEGER NOT NULL DEFAULT 0,
		tries INTEGER NOT NULL DEFAULT 0
	)`)
	return err
}

// SaveCheckin replaces the record for one provider.
func (s *Store) SaveCheckin(record domain.CheckinRecord) error {
	offline := 0
	if record.Offline {
		offline = 1
	}
	_, err := s.db.Exec(`INSERT INTO checkins
		(provider_id, account_key, day, at, outcome, credit, streak, msg, valid_until, offline, tries)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_id) DO UPDATE SET
			account_key = excluded.account_key, day = excluded.day, at = excluded.at,
			outcome = excluded.outcome, credit = excluded.credit, streak = excluded.streak,
			msg = excluded.msg, valid_until = excluded.valid_until,
			offline = excluded.offline, tries = excluded.tries`,
		record.ProviderID, record.AccountKey, record.Day, record.At, record.Outcome,
		record.Credit, record.Streak, record.Message, record.Until, offline, record.Tries)
	return err
}

const checkinSelectColumns = `provider_id, account_key, day, at, outcome, credit, streak, msg, valid_until, offline, tries`

func scanCheckin(rows *sql.Rows) (domain.CheckinRecord, error) {
	var record domain.CheckinRecord
	var offline int
	if err := rows.Scan(&record.ProviderID, &record.AccountKey, &record.Day, &record.At, &record.Outcome,
		&record.Credit, &record.Streak, &record.Message, &record.Until, &offline, &record.Tries); err != nil {
		return domain.CheckinRecord{}, err
	}
	record.Offline = offline != 0
	return record, nil
}

// ListCheckins returns the latest record of every provider that has one.
func (s *Store) ListCheckins() ([]domain.CheckinRecord, error) {
	rows, err := s.reader().Query(`SELECT ` + checkinSelectColumns + ` FROM checkins ORDER BY provider_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []domain.CheckinRecord{}
	for rows.Next() {
		record, err := scanCheckin(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// CheckinByProvider returns the latest record of one provider; found is false
// when it has never been checked in.
func (s *Store) CheckinByProvider(providerID string) (domain.CheckinRecord, bool, error) {
	rows, err := s.reader().Query(`SELECT `+checkinSelectColumns+` FROM checkins WHERE provider_id = ?`, strings.TrimSpace(providerID))
	if err != nil {
		return domain.CheckinRecord{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return domain.CheckinRecord{}, false, rows.Err()
	}
	record, err := scanCheckin(rows)
	if err != nil {
		return domain.CheckinRecord{}, false, err
	}
	return record, true, nil
}

// pruneCheckins drops the records of providers that no longer exist. SaveState
// rewrites the whole providers table, so a provider deleted while the gateway
// ran would otherwise leave its check-in row behind forever.
func pruneCheckins(tx *sql.Tx) error {
	_, err := tx.Exec(`DELETE FROM checkins WHERE provider_id NOT IN (SELECT id FROM providers)`)
	return err
}
