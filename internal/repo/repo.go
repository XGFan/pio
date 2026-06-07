// Package repo holds thin DAO helpers that wrap raw SQL queries used by
// more than one caller (Phase 1: sync service + CLI add-key helper;
// Phase 5+: REST handlers).
package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/guofan/pia/internal/crypto"
	"github.com/guofan/pia/internal/model"
)

// ErrNotFound is returned when a lookup by primary key finds no row.
var ErrNotFound = errors.New("repo: not found")

// AAD for the encrypted_key column. Bound to the column name so a
// ciphertext copied into upstream_proxies.encrypted_password can never be
// decrypted there.
const apiKeyAAD = "api_keys.encrypted_key"

// InsertApiKey encrypts apiKeyPlain with masterKey and inserts a row.
// Returns the auto-increment ID.
func InsertApiKey(ctx context.Context, db *sql.DB, masterKey []byte, label, apiKeyPlain string) (int64, error) {
	enc, err := crypto.Encrypt(masterKey, []byte(apiKeyPlain), crypto.ColumnAAD(apiKeyAAD))
	if err != nil {
		return 0, err
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO api_keys (label, encrypted_key, added_at, active) VALUES (?, ?, ?, 1)`,
		label, enc, time.Now().UTC(),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetApiKeyPlain returns the decrypted API key string and label for id.
// Returns ErrNotFound if the row does not exist.
func GetApiKeyPlain(ctx context.Context, db *sql.DB, masterKey []byte, id int64) (apiKey, label string, err error) {
	var enc []byte
	err = db.QueryRowContext(ctx,
		`SELECT encrypted_key, label FROM api_keys WHERE id = ?`, id,
	).Scan(&enc, &label)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}
	pt, err := crypto.Decrypt(masterKey, enc, crypto.ColumnAAD(apiKeyAAD))
	if err != nil {
		return "", "", err
	}
	return string(pt), label, nil
}

// MarkApiKeySyncSuccess updates LastSyncedAt and clears LastSyncError.
func MarkApiKeySyncSuccess(ctx context.Context, db ExecCtx, id int64, when time.Time) error {
	_, err := db.ExecContext(ctx,
		`UPDATE api_keys SET last_synced_at = ?, last_sync_error = '' WHERE id = ?`,
		when, id,
	)
	return err
}

// MarkApiKeySyncError sets LastSyncError. LastSyncedAt is intentionally
// untouched so the UI can still show the most recent successful sync.
func MarkApiKeySyncError(ctx context.Context, db ExecCtx, id int64, errMsg string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE api_keys SET last_sync_error = ? WHERE id = ?`, errMsg, id,
	)
	return err
}

// ListApiKeys returns every api_keys row ordered by ID ascending.
func ListApiKeys(ctx context.Context, db *sql.DB) ([]model.ApiKey, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, label, encrypted_key, added_at, last_synced_at, last_sync_error, active
		   FROM api_keys ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ApiKey
	for rows.Next() {
		var k model.ApiKey
		var synced sql.NullTime
		if err := rows.Scan(&k.ID, &k.Label, &k.EncryptedKey, &k.AddedAt, &synced, &k.LastSyncError, &k.Active); err != nil {
			return nil, err
		}
		if synced.Valid {
			t := synced.Time
			k.LastSyncedAt = &t
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ExecCtx is the subset of *sql.DB / *sql.Tx the sync helpers need; lets
// callers wrap the same DAO inside a larger transaction.
type ExecCtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
