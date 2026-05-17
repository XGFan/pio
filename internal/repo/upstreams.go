package repo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/model"
)

// upstreamPasswordAAD is the AAD for the upstream_proxies.encrypted_password
// column. Duplicated as a const in the sync package; centralizing is a
// Phase 5 refactor target.
const upstreamPasswordAAD = "upstream_proxies.encrypted_password"

// ResolvedUpstream is an UpstreamProxy with its password decrypted ready
// for runtime use. Returned from routing-hydration paths only.
type ResolvedUpstream struct {
	model.UpstreamProxy
	Password string // decrypted plaintext upstream proxy password
}

// ListAllResolvedUpstreams reads every upstream_proxies row, decrypts the
// password column, and returns a map keyed by stable ID. Used by
// routing.Core.Hydrate at boot.
func ListAllResolvedUpstreams(ctx context.Context, db *sql.DB, masterKey []byte) (map[string]*ResolvedUpstream, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, source_api_key_id, host, port, username, encrypted_password, protocol,
		        display_name, country_code, city_name, alive, recently_failing,
		        recent_failure_count, recent_failure_since, last_seen_at
		   FROM upstream_proxies`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*ResolvedUpstream)
	for rows.Next() {
		u := &ResolvedUpstream{}
		var enc []byte
		var since sql.NullTime
		if err := rows.Scan(&u.ID, &u.SourceApiKeyID, &u.Host, &u.Port, &u.Username, &enc, &u.Protocol,
			&u.DisplayName, &u.CountryCode, &u.CityName, &u.Alive, &u.RecentlyFailing,
			&u.RecentFailureCount, &since, &u.LastSeenAt); err != nil {
			return nil, err
		}
		if since.Valid {
			t := since.Time
			u.RecentFailureSince = &t
		}
		pt, err := crypto.Decrypt(masterKey, enc, crypto.ColumnAAD(upstreamPasswordAAD))
		if err != nil {
			return nil, err
		}
		u.Password = string(pt)
		out[u.ID] = u
	}
	return out, rows.Err()
}

// GetUpstream returns one row by stable ID (no password decrypt).
func GetUpstream(ctx context.Context, db *sql.DB, id string) (*model.UpstreamProxy, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, source_api_key_id, host, port, username, encrypted_password, protocol,
		        display_name, country_code, city_name, alive, recently_failing,
		        recent_failure_count, recent_failure_since, last_seen_at
		   FROM upstream_proxies WHERE id = ?`, id,
	)
	u := &model.UpstreamProxy{}
	var enc []byte
	var since sql.NullTime
	err := row.Scan(&u.ID, &u.SourceApiKeyID, &u.Host, &u.Port, &u.Username, &enc, &u.Protocol,
		&u.DisplayName, &u.CountryCode, &u.CityName, &u.Alive, &u.RecentlyFailing,
		&u.RecentFailureCount, &since, &u.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.EncryptedPassword = enc
	if since.Valid {
		t := since.Time
		u.RecentFailureSince = &t
	}
	return u, nil
}

// ListUpstreams returns every row, ordered by country then display_name.
// Caller decides whether to surface alive=false / broken rows. Passwords
// remain encrypted in the returned slice (UI never needs them).
func ListUpstreams(ctx context.Context, db *sql.DB) ([]model.UpstreamProxy, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, source_api_key_id, host, port, username, encrypted_password, protocol,
		        display_name, country_code, city_name, alive, recently_failing,
		        recent_failure_count, recent_failure_since, last_seen_at
		   FROM upstream_proxies ORDER BY country_code, display_name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.UpstreamProxy
	for rows.Next() {
		var u model.UpstreamProxy
		var since sql.NullTime
		if err := rows.Scan(&u.ID, &u.SourceApiKeyID, &u.Host, &u.Port, &u.Username, &u.EncryptedPassword, &u.Protocol,
			&u.DisplayName, &u.CountryCode, &u.CityName, &u.Alive, &u.RecentlyFailing,
			&u.RecentFailureCount, &since, &u.LastSeenAt); err != nil {
			return nil, err
		}
		if since.Valid {
			t := since.Time
			u.RecentFailureSince = &t
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUpstreamDisplayName changes only the display_name; auto-sync
// preserves user-edited names per US-006.
func UpdateUpstreamDisplayName(ctx context.Context, db ExecCtx, id, displayName string) error {
	res, err := db.ExecContext(ctx,
		`UPDATE upstream_proxies SET display_name = ? WHERE id = ?`,
		displayName, id,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// HasReferencingUsersForKey reports whether any local_users.upstream_proxy_id
// points at an upstream_proxies row owned by keyID. Used by the API-key
// delete handler to return 409 instead of cascading.
func HasReferencingUsersForKey(ctx context.Context, db *sql.DB, keyID int64) (bool, []ReferencingUser, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lu.username, lu.upstream_proxy_id, up.display_name, up.country_code
		  FROM local_users lu
		  JOIN upstream_proxies up ON up.id = lu.upstream_proxy_id
		 WHERE up.source_api_key_id = ?`,
		keyID,
	)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	var out []ReferencingUser
	for rows.Next() {
		var r ReferencingUser
		if err := rows.Scan(&r.Username, &r.UpstreamProxyID, &r.DisplayName, &r.CountryCode); err != nil {
			return false, nil, err
		}
		out = append(out, r)
	}
	return len(out) > 0, out, rows.Err()
}

// ReferencingUser is one row in the 409 body when a key is in use.
type ReferencingUser struct {
	Username        string `json:"username"`
	UpstreamProxyID string `json:"upstream_proxy_id"`
	DisplayName     string `json:"display_name"`
	CountryCode     string `json:"country_code"`
}

// DeleteApiKey removes an ApiKey row and all upstreams owned by it.
// Returns ErrKeyInUse with the conflict list when any local_user still
// references one of this key's upstreams.
func DeleteApiKey(ctx context.Context, db *sql.DB, id int64) error {
	inUse, refs, err := HasReferencingUsersForKey(ctx, db, id)
	if err != nil {
		return err
	}
	if inUse {
		return &ErrKeyInUse{ReferencingUsers: refs}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM upstream_proxies WHERE source_api_key_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit_log (at, actor, action, detail) VALUES (?, 'cli', 'key_delete', ?)`,
		time.Now().UTC(), "{}"); err != nil {
		return err
	}
	return tx.Commit()
}

// ErrKeyInUse is returned by DeleteApiKey when other rows reference the key.
type ErrKeyInUse struct {
	ReferencingUsers []ReferencingUser
}

func (e *ErrKeyInUse) Error() string {
	return "api key is in use by local users; unmap them first"
}
