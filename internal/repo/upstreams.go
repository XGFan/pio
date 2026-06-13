package repo

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/guofan/pio/internal/crypto"
	"github.com/guofan/pio/internal/model"
)

// upstreamPasswordAAD is the AAD for the upstream_proxies.encrypted_password
// column. Duplicated as a const in the sync package; centralizing is a
// later refactor target.
const upstreamPasswordAAD = "upstream_proxies.encrypted_password"

// SourceWebshare / SourceManual / SourceDefault are the canonical values of
// upstream_proxies.source. SourceDefault tags the single built-in "default"
// upstream whose egress is the daemon's own host network (no upstream hop).
// (It was historically named "direct"; renamed in migration 0011.)
const (
	SourceWebshare = "webshare"
	SourceManual   = "manual"
	SourceDefault  = "default"
)

// DefaultUpstreamID is the reserved, stable primary key of the built-in default
// upstream. It is also its display name, so it is routable by name through the
// universal-password path and appears in the subscription list. It is offered
// as a mapping target in the admin UI alongside synced/manual upstreams.
//
// SECURITY (intentional, product-confirmed): because it egresses from the
// daemon's own host, any holder of the universal proxy password can reach the
// host's network through it — including loopback, RFC1918 internals, and cloud
// metadata (169.254.169.254). This wider blast radius vs. a remote upstream is
// an accepted trade-off: default is a built-in by-name pattern meant to use the
// app's own network as the exit. Restrict the universal password's distribution
// accordingly.
const DefaultUpstreamID = "default"

// ProtocolHTTP / HTTPS / SOCKS5 enumerate the values upstream_proxies.protocol
// is allowed to take. The CHECK constraint in migration 0004 enforces these.
const (
	ProtocolHTTP   = "http"
	ProtocolHTTPS  = "https"
	ProtocolSOCKS5 = "socks5"
)

// ResolvedUpstream is an UpstreamProxy with its password decrypted ready
// for runtime use. Returned from routing-hydration paths only.
type ResolvedUpstream struct {
	model.UpstreamProxy
	Password string // decrypted plaintext upstream proxy password
}

const upstreamSelectCols = `id, source, source_api_key_id, manual_name, host, port, username,
	encrypted_password, protocol, display_name, country_code, city_name,
	recently_failing, recent_failure_count, recent_failure_since, last_seen_at,
	last_latency_ms, last_latency_at`

// scanUpstream fills u from a row that selected upstreamSelectCols.
func scanUpstream(row interface {
	Scan(dest ...any) error
}, u *model.UpstreamProxy) (encPwd []byte, err error) {
	var srcKey sql.NullInt64
	var since sql.NullTime
	var latencyMS sql.NullInt64
	var latencyAt sql.NullTime
	err = row.Scan(
		&u.ID, &u.Source, &srcKey, &u.ManualName, &u.Host, &u.Port, &u.Username,
		&encPwd, &u.Protocol, &u.DisplayName, &u.CountryCode, &u.CityName,
		&u.RecentlyFailing, &u.RecentFailureCount, &since, &u.LastSeenAt,
		&latencyMS, &latencyAt,
	)
	if err != nil {
		return nil, err
	}
	if srcKey.Valid {
		v := srcKey.Int64
		u.SourceApiKeyID = &v
	}
	if since.Valid {
		t := since.Time
		u.RecentFailureSince = &t
	}
	if latencyMS.Valid {
		v := int(latencyMS.Int64)
		u.LastLatencyMS = &v
	}
	if latencyAt.Valid {
		t := latencyAt.Time
		u.LastLatencyAt = &t
	}
	return encPwd, nil
}

// UpdateUpstreamLatency records the result of a latency probe: ms >= 0 for a
// measured latency, ms = -1 for a failed probe. at is the probe time.
func UpdateUpstreamLatency(ctx context.Context, db ExecCtx, id string, ms int, at time.Time) error {
	_, err := db.ExecContext(ctx,
		`UPDATE upstream_proxies SET last_latency_ms = ?, last_latency_at = ? WHERE id = ?`,
		ms, at, id,
	)
	return err
}

// ListAllResolvedUpstreams reads every upstream_proxies row, decrypts the
// password column, and returns a map keyed by stable ID. Used by
// routing.Core.Hydrate at boot. Includes both webshare and manual rows;
// the caller treats them uniformly.
func ListAllResolvedUpstreams(ctx context.Context, db *sql.DB, masterKey []byte) (map[string]*ResolvedUpstream, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+upstreamSelectCols+` FROM upstream_proxies`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]*ResolvedUpstream)
	for rows.Next() {
		u := &ResolvedUpstream{}
		enc, err := scanUpstream(rows, &u.UpstreamProxy)
		if err != nil {
			return nil, err
		}
		// An empty ciphertext means "no password" (manual proxies may omit
		// upstream auth entirely). Skip decrypt in that case.
		if len(enc) > 0 {
			pt, err := crypto.Decrypt(masterKey, enc, crypto.ColumnAAD(upstreamPasswordAAD))
			if err != nil {
				return nil, fmt.Errorf("decrypt upstream %s: %w", u.ID, err)
			}
			u.Password = string(pt)
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

// EnsureDefaultUpstream idempotently seeds the built-in "default" upstream
// (id=DefaultUpstreamID, source='default'). Called at daemon boot before routing
// hydration so the row always exists — clients can map to it, it is routable by
// its display name, and local_users.upstream_proxy_id='default' satisfies the FK.
//
// The row carries no host/port/credentials: the data plane (tunnel.DialUpstream)
// dispatches on source=='default' and dials the target straight out of the host
// network. protocol is set to a valid filler ('http') only to satisfy the column
// CHECK; it is never consulted for default rows. encrypted_password is an empty
// blob (no upstream auth). ON CONFLICT keeps the call self-healing and a no-op
// once seeded, so it is safe to run on every boot.
func EnsureDefaultUpstream(ctx context.Context, db ExecCtx) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO upstream_proxies
			(id, source, source_api_key_id, manual_name, host, port, username,
			 encrypted_password, protocol, display_name, country_code, city_name,
			 last_seen_at)
		VALUES (?, 'default', NULL, '', '', 0, '', X'', 'http', ?, '', '', ?)
		ON CONFLICT(id) DO NOTHING
	`, DefaultUpstreamID, DefaultUpstreamID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("ensure default upstream: %w", err)
	}
	return nil
}

// GetUpstream returns one row by stable ID (no password decrypt).
func GetUpstream(ctx context.Context, db *sql.DB, id string) (*model.UpstreamProxy, error) {
	row := db.QueryRowContext(ctx, `SELECT `+upstreamSelectCols+` FROM upstream_proxies WHERE id = ?`, id)
	u := &model.UpstreamProxy{}
	enc, err := scanUpstream(row, u)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.EncryptedPassword = enc
	return u, nil
}

// ListUpstreams returns every row, ordered so that:
//   - default/manual rows come before webshare rows (source ASC, since
//     'default' < 'manual' < 'webshare')
//   - within each source bucket, rows order by country_code then display_name
//
// Manual rows are inserted with country_code='' so in practice they all
// land in a single block at the top of the response — that's the UI
// behavior we want (user-curated entries above the noisy webshare list).
//
// The built-in 'default' row is returned here too and is surfaced by the admin
// listing (api.listUpstreams) so it can be picked as a user→upstream mapping
// target; it sorts to the very top.
func ListUpstreams(ctx context.Context, db *sql.DB) ([]model.UpstreamProxy, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+upstreamSelectCols+` FROM upstream_proxies ORDER BY source ASC, country_code, display_name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.UpstreamProxy
	for rows.Next() {
		var u model.UpstreamProxy
		enc, err := scanUpstream(rows, &u)
		if err != nil {
			return nil, err
		}
		u.EncryptedPassword = enc
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListManualProxies returns just the source='manual' rows, ordered by
// display name. Used by the dedicated /api/v1/manual-proxies endpoint.
func ListManualProxies(ctx context.Context, db *sql.DB) ([]model.UpstreamProxy, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT `+upstreamSelectCols+` FROM upstream_proxies WHERE source = 'manual' ORDER BY manual_name`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.UpstreamProxy
	for rows.Next() {
		var u model.UpstreamProxy
		enc, err := scanUpstream(rows, &u)
		if err != nil {
			return nil, err
		}
		u.EncryptedPassword = enc
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUpstreamDisplayName changes only the display_name; auto-sync
// preserves user-edited names for webshare rows.
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

// HasReferencingUsersForUpstream returns the local users that map to the
// given upstream id. Used by manual-proxy delete to return 409.
func HasReferencingUsersForUpstream(ctx context.Context, db *sql.DB, upstreamID string) ([]ReferencingUser, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lu.username, lu.upstream_proxy_id, up.display_name, up.country_code
		  FROM local_users lu
		  JOIN upstream_proxies up ON up.id = lu.upstream_proxy_id
		 WHERE up.id = ?`,
		upstreamID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReferencingUser
	for rows.Next() {
		var r ReferencingUser
		if err := rows.Scan(&r.Username, &r.UpstreamProxyID, &r.DisplayName, &r.CountryCode); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReferencingUser is one row in the 409 body when a key or upstream is in use.
type ReferencingUser struct {
	Username        string `json:"username"`
	UpstreamProxyID string `json:"upstream_proxy_id"`
	DisplayName     string `json:"display_name"`
	CountryCode     string `json:"country_code"`
}

// DeleteApiKey removes an ApiKey row and all upstreams owned by it.
// Returns ErrKeyInUse with the conflict list when any local_user still
// references one of this key's upstreams. Only deletes upstreams with
// source='webshare' so manual rows survive even if they share an api_keys.id
// (they shouldn't — manual rows have source_api_key_id=NULL — but be explicit).
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
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM upstream_proxies WHERE source_api_key_id = ? AND source = 'webshare'`, id,
	); err != nil {
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

// ErrUpstreamInUse is returned by DeleteManualProxy when local users
// still map to the upstream.
type ErrUpstreamInUse struct {
	ReferencingUsers []ReferencingUser
}

func (e *ErrUpstreamInUse) Error() string {
	return "upstream is in use by local users; unmap them first"
}

// ErrManualNameInUse is returned by Insert/UpdateManualProxy when the
// supplied manual_name collides with another manual row.
var ErrManualNameInUse = errors.New("repo: manual proxy name already in use")

// ErrInvalidManualProxy is returned for required-field violations.
var ErrInvalidManualProxy = errors.New("repo: invalid manual proxy input")

// ErrUpstreamNotManual is returned by UpdateManualProxy/DeleteManualProxy
// when the targeted id resolves to a non-manual row (e.g., a webshare
// upstream). Lets the API layer turn this into a 404 rather than a 500.
var ErrUpstreamNotManual = errors.New("repo: upstream is not manual")

// ManualProxyInput is the field bag both Insert and Update consume.
// For Update, an empty Password means "leave existing password unchanged".
type ManualProxyInput struct {
	Name     string
	Host     string
	Port     int
	Protocol string // ProtocolHTTP / HTTPS / SOCKS5
	Username string
	Password string
}

// validate enforces the field-shape rules the DB schema can't.
func (in ManualProxyInput) validate() error {
	if strings.TrimSpace(in.Name) == "" {
		return fmt.Errorf("%w: name required", ErrInvalidManualProxy)
	}
	if strings.TrimSpace(in.Host) == "" {
		return fmt.Errorf("%w: host required", ErrInvalidManualProxy)
	}
	if in.Port <= 0 || in.Port > 65535 {
		return fmt.Errorf("%w: port out of range", ErrInvalidManualProxy)
	}
	switch in.Protocol {
	case ProtocolHTTP, ProtocolHTTPS, ProtocolSOCKS5:
	default:
		return fmt.Errorf("%w: protocol must be http|https|socks5", ErrInvalidManualProxy)
	}
	// Password without a username is a tunnel-side foot-gun: the SOCKS5
	// sub-negotiation would send ULEN=0 (RFC 1929 nominally allows it but
	// many servers reject as malformed) and HTTP CONNECT's Basic auth
	// becomes ":pwd" which most upstreams treat as auth-failure. Reject
	// at the boundary so neither downstream path sees the broken shape.
	if in.Password != "" && in.Username == "" {
		return fmt.Errorf("%w: password requires a username", ErrInvalidManualProxy)
	}
	return nil
}

// newManualID returns a fresh 16-hex random identifier. crypto/rand never
// errors in practice; the surrounding fallback is belt-and-braces.
func newManualID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// InsertManualProxy creates a new manual upstream and returns its ID.
// Empty Password → empty ciphertext (no upstream auth); non-empty is
// encrypted with the column AAD. UNIQUE collision on manual_name returns
// ErrManualNameInUse.
func InsertManualProxy(ctx context.Context, db *sql.DB, masterKey []byte, in ManualProxyInput) (string, error) {
	if err := in.validate(); err != nil {
		return "", err
	}
	id, err := newManualID()
	if err != nil {
		return "", fmt.Errorf("gen manual id: %w", err)
	}
	// The encrypted_password column is BLOB NOT NULL. For manual proxies
	// without auth we still store a zero-length blob (sqlite distinguishes
	// X'' from NULL, and X'' is what we want here).
	enc := []byte{}
	if in.Password != "" {
		enc, err = crypto.Encrypt(masterKey, []byte(in.Password), crypto.ColumnAAD(upstreamPasswordAAD))
		if err != nil {
			return "", fmt.Errorf("encrypt password: %w", err)
		}
	}
	now := time.Now().UTC()
	_, err = db.ExecContext(ctx, `
		INSERT INTO upstream_proxies
			(id, source, source_api_key_id, manual_name, host, port, username,
			 encrypted_password, protocol, display_name, country_code, city_name,
			 last_seen_at)
		VALUES (?, 'manual', NULL, ?, ?, ?, ?, ?, ?, ?, '', '', ?)
	`, id, in.Name, in.Host, in.Port, in.Username, enc, in.Protocol, in.Name, now)
	if err != nil {
		if isUniqueViolation(err) {
			return "", ErrManualNameInUse
		}
		return "", err
	}
	return id, nil
}

// UpdateManualProxy rewrites every field on a source='manual' row. An
// empty Password means "keep existing"; the caller fills any field it
// wants to preserve from the current row first.
func UpdateManualProxy(ctx context.Context, db *sql.DB, masterKey []byte, id string, in ManualProxyInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	// Fetch the current row to (a) confirm it's manual, (b) preserve the
	// password ciphertext when the caller passes the empty sentinel.
	cur, err := GetUpstream(ctx, db, id)
	if err != nil {
		return err
	}
	if cur.Source != SourceManual {
		return ErrUpstreamNotManual
	}

	enc := cur.EncryptedPassword
	if enc == nil {
		// Defensive — scan can yield nil for zero-length BLOBs depending on
		// driver behavior. encrypted_password is NOT NULL, so coerce to X''.
		enc = []byte{}
	}
	if in.Password != "" {
		enc, err = crypto.Encrypt(masterKey, []byte(in.Password), crypto.ColumnAAD(upstreamPasswordAAD))
		if err != nil {
			return fmt.Errorf("encrypt password: %w", err)
		}
	}

	_, err = db.ExecContext(ctx, `
		UPDATE upstream_proxies
		   SET manual_name = ?, host = ?, port = ?, username = ?,
		       encrypted_password = ?, protocol = ?, display_name = ?
		 WHERE id = ? AND source = 'manual'
	`, in.Name, in.Host, in.Port, in.Username, enc, in.Protocol, in.Name, id)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrManualNameInUse
		}
		return err
	}
	return nil
}

// DeleteManualProxy removes a source='manual' row. Returns ErrUpstreamInUse
// with the referencing-user list when local_users still map to the row.
func DeleteManualProxy(ctx context.Context, db *sql.DB, id string) error {
	cur, err := GetUpstream(ctx, db, id)
	if err != nil {
		return err
	}
	if cur.Source != SourceManual {
		return ErrUpstreamNotManual
	}
	refs, err := HasReferencingUsersForUpstream(ctx, db, id)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &ErrUpstreamInUse{ReferencingUsers: refs}
	}
	res, err := db.ExecContext(ctx,
		`DELETE FROM upstream_proxies WHERE id = ? AND source = 'manual'`, id,
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

// isUniqueViolation checks whether the SQLite driver's error string
// indicates a UNIQUE constraint failure ON the manual_name index. We
// additionally require the manual_name fragment so adding a second
// UNIQUE index on upstream_proxies in the future doesn't quietly start
// surfacing as ErrManualNameInUse.
//
// modernc.org/sqlite returns errors as plain strings on this code path
// (no typed sqlite.Error from BindContext+Exec); the substring match is
// the contract.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "UNIQUE constraint failed") &&
		!strings.Contains(msg, "constraint failed: UNIQUE") {
		return false
	}
	return strings.Contains(msg, "manual_name")
}
