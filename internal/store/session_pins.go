package store

import (
	"database/sql"
	"strings"
)

// ensureSessionPinsTable keeps conversation→provider pins across restarts.
//
// The pins are the gateway's promise that a conversation keeps the upstream
// account that already has its prompt cached. Keeping them in memory only meant
// every deploy or restart sent every live conversation to whatever account the
// scheduler picked next, and that account had to bill the whole prompt again as
// fresh input tokens. One row per pin, expired lazily on read.
func ensureSessionPinsTable(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS session_pins (
		session_key TEXT PRIMARY KEY,
		provider_id TEXT NOT NULL,
		expires_at TEXT NOT NULL
	)`)
	return err
}

// SaveSessionPins writes the given pins, replacing any existing row and dropping
// pins whose expiry has passed.
func (s *Store) SaveSessionPins(pins []SessionPin) error {
	if len(pins) == 0 {
		_, err := s.db.Exec(`DELETE FROM session_pins WHERE expires_at <= ?`, nowRFC3339())
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := nowRFC3339()
	if _, err := tx.Exec(`DELETE FROM session_pins WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	statement, err := tx.Prepare(`INSERT INTO session_pins (session_key, provider_id, expires_at)
		VALUES (?, ?, ?)
		ON CONFLICT(session_key) DO UPDATE SET provider_id = excluded.provider_id, expires_at = excluded.expires_at`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, pin := range pins {
		if strings.TrimSpace(pin.SessionKey) == "" || strings.TrimSpace(pin.ProviderID) == "" {
			continue
		}
		if pin.ExpiresAt <= now {
			continue // RFC3339 UTC strings order like their instants
		}
		if _, err := statement.Exec(pin.SessionKey, pin.ProviderID, pin.ExpiresAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSessionPins removes pins by key (a forgotten conversation) or, with an
// empty key, every pin whose expiry has passed.
func (s *Store) DeleteSessionPins(sessionKeys []string) error {
	if len(sessionKeys) == 0 {
		_, err := s.db.Exec(`DELETE FROM session_pins WHERE expires_at <= ?`, nowRFC3339())
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.Prepare(`DELETE FROM session_pins WHERE session_key = ?`)
	if err != nil {
		return err
	}
	defer statement.Close()
	for _, key := range sessionKeys {
		if _, err := statement.Exec(key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListSessionPins returns every pin that has not expired.
func (s *Store) ListSessionPins() ([]SessionPin, error) {
	rows, err := s.reader().Query(`SELECT session_key, provider_id, expires_at FROM session_pins WHERE expires_at > ?`, nowRFC3339())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]SessionPin, 0)
	for rows.Next() {
		var pin SessionPin
		if err := rows.Scan(&pin.SessionKey, &pin.ProviderID, &pin.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, pin)
	}
	return out, rows.Err()
}

// SessionPin is one conversation's provider pin as the store keeps it.
type SessionPin struct {
	SessionKey string
	ProviderID string
	ExpiresAt  string // RFC3339
}
