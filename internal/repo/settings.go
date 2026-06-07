package repo

import (
	"context"
	"database/sql"

	"github.com/guofan/pia/internal/crypto"
	"github.com/guofan/pia/internal/model"
)

// universalProxyPasswordAAD is the AAD for the settings.universal_proxy_password_enc
// column. It binds the ciphertext to this specific column so it can't be
// transplanted elsewhere and decrypted.
const universalProxyPasswordAAD = "settings.universal_proxy_password_enc"

// LoadSettings reads the single row from the settings table.
func LoadSettings(ctx context.Context, db *sql.DB) (model.Settings, error) {
	var s model.Settings
	var enabled int
	var subEnabled int
	err := db.QueryRowContext(ctx, `
		SELECT sync_interval_minutes, proxy_port, proxy_bind, api_port, proxy_enabled,
		       subscription_enabled, subscription_host
		  FROM settings WHERE id = 1`,
	).Scan(&s.SyncIntervalMinutes, &s.ProxyPort, &s.ProxyBind, &s.APIPort, &enabled,
		&subEnabled, &s.SubscriptionHost)
	s.ProxyEnabled = enabled != 0
	s.SubscriptionEnabled = subEnabled != 0
	return s, err
}

// UpdateSettings overwrites the single row's mutable fields.
func UpdateSettings(ctx context.Context, db *sql.DB, s model.Settings) error {
	enabled := 0
	if s.ProxyEnabled {
		enabled = 1
	}
	subEnabled := 0
	if s.SubscriptionEnabled {
		subEnabled = 1
	}
	_, err := db.ExecContext(ctx, `
		UPDATE settings
		   SET sync_interval_minutes = ?, proxy_port = ?, proxy_bind = ?, proxy_enabled = ?,
		       subscription_enabled = ?, subscription_host = ?
		 WHERE id = 1`,
		s.SyncIntervalMinutes, s.ProxyPort, s.ProxyBind, enabled,
		subEnabled, s.SubscriptionHost,
	)
	return err
}

// SetProxyEnabled persists only the proxy_enabled flag. Used by the
// explicit Start/Stop REST endpoints which don't touch port settings.
func SetProxyEnabled(ctx context.Context, db *sql.DB, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := db.ExecContext(ctx, `UPDATE settings SET proxy_enabled = ? WHERE id = 1`, v)
	return err
}

// LoadUniversalProxyPassword reads and decrypts the universal proxy password
// from the single settings row. Returns "" (and no error) when the feature is
// unset — i.e. the stored blob is zero-length. Used by the routing layer at
// hydrate/rebuild time so (display_name, universal_password) auth can resolve.
func LoadUniversalProxyPassword(ctx context.Context, db *sql.DB, masterKey []byte) (string, error) {
	var enc []byte
	err := db.QueryRowContext(ctx,
		`SELECT universal_proxy_password_enc FROM settings WHERE id = 1`,
	).Scan(&enc)
	if err != nil {
		return "", err
	}
	if len(enc) == 0 {
		return "", nil
	}
	pt, err := crypto.Decrypt(masterKey, enc, crypto.ColumnAAD(universalProxyPasswordAAD))
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

// HasUniversalProxyPassword reports whether a universal proxy password is
// configured, without decrypting it. Used by GET /settings so the UI can show
// a set/unset indicator without ever revealing the value.
func HasUniversalProxyPassword(ctx context.Context, db *sql.DB) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT length(universal_proxy_password_enc) FROM settings WHERE id = 1`,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetUniversalProxyPassword sets (or clears) the universal proxy password. An
// empty plaintext clears it (stores a zero-length blob = feature disabled);
// a non-empty plaintext is AES-256-GCM-encrypted with the column AAD. The
// caller is responsible for re-hydrating routing so the change takes effect.
func SetUniversalProxyPassword(ctx context.Context, db *sql.DB, masterKey []byte, plaintext string) error {
	enc := []byte{}
	if plaintext != "" {
		var err error
		enc, err = crypto.Encrypt(masterKey, []byte(plaintext), crypto.ColumnAAD(universalProxyPasswordAAD))
		if err != nil {
			return err
		}
	}
	_, err := db.ExecContext(ctx,
		`UPDATE settings SET universal_proxy_password_enc = ? WHERE id = 1`, enc,
	)
	return err
}

// SaveAPIPort writes the actual port the REST server bound to so the
// SwiftUI app can read it back via the api.port file.
func SaveAPIPort(ctx context.Context, db *sql.DB, port int) error {
	_, err := db.ExecContext(ctx, `UPDATE settings SET api_port = ? WHERE id = 1`, port)
	return err
}

// AuditLog inserts a row into audit_log. Best-effort; callers ignore errors.
func AuditLog(ctx context.Context, db *sql.DB, actor, action, detail string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO audit_log (at, actor, action, detail) VALUES (CURRENT_TIMESTAMP, ?, ?, ?)`,
		actor, action, detail,
	)
	return err
}

// ListAuditLog returns the last N audit rows ordered newest first.
func ListAuditLog(ctx context.Context, db *sql.DB, limit int) ([]model.AuditLogEntry, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, at, actor, action, detail FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.AuditLogEntry
	for rows.Next() {
		var e model.AuditLogEntry
		if err := rows.Scan(&e.ID, &e.At, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
