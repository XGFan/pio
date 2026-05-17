package repo

import (
	"context"
	"database/sql"

	"github.com/guofan/webshare-proxy/internal/model"
)

// LoadSettings reads the single row from the settings table.
func LoadSettings(ctx context.Context, db *sql.DB) (model.Settings, error) {
	var s model.Settings
	var enabled int
	err := db.QueryRowContext(ctx, `
		SELECT sync_interval_minutes, http_listener_port, http_listener_bind,
		       socks5_listener_port, socks5_listener_bind, api_port, proxy_enabled
		  FROM settings WHERE id = 1`,
	).Scan(&s.SyncIntervalMinutes, &s.HTTPListenerPort, &s.HTTPListenerBind,
		&s.SOCKS5ListenerPort, &s.SOCKS5ListenerBind, &s.APIPort, &enabled)
	s.ProxyEnabled = enabled != 0
	return s, err
}

// UpdateSettings overwrites the single row's mutable fields.
func UpdateSettings(ctx context.Context, db *sql.DB, s model.Settings) error {
	enabled := 0
	if s.ProxyEnabled {
		enabled = 1
	}
	_, err := db.ExecContext(ctx, `
		UPDATE settings
		   SET sync_interval_minutes = ?, http_listener_port = ?, http_listener_bind = ?,
		       socks5_listener_port = ?, socks5_listener_bind = ?, proxy_enabled = ?
		 WHERE id = 1`,
		s.SyncIntervalMinutes, s.HTTPListenerPort, s.HTTPListenerBind,
		s.SOCKS5ListenerPort, s.SOCKS5ListenerBind, enabled,
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
