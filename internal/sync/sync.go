// Package sync implements the webshare proxy-list sync workflow. One
// SyncKey call corresponds to one API key: fetch the full upstream list,
// reconcile against the local SQLite cache, and update ApiKey.LastSyncedAt.
//
// Multi-key concurrency is Phase 6. Phase 1 is single-key, on-demand.
package sync

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/webshare"
)

const upstreamPasswordAAD = "upstream_proxies.encrypted_password"

// Fetcher is the seam between the sync service and webshare.Client. The
// real implementation is the webshare package; tests supply a fake.
type Fetcher interface {
	ListProxies(ctx context.Context) ([]webshare.Proxy, error)
}

// FetcherFactory builds a Fetcher from a decrypted API key. Production
// uses DefaultFetcherFactory; tests inject a fake.
type FetcherFactory func(apiKey string) Fetcher

// DefaultFetcherFactory returns a real webshare client backed by the
// process-wide http.DefaultClient.
func DefaultFetcherFactory(apiKey string) Fetcher {
	return webshare.New(apiKey, nil)
}

// Service owns one *sql.DB and the master key. It is safe for concurrent
// SyncKey calls on different IDs; concurrent calls on the same ID will
// serialize through SQLite locking but the design assumes a single-flight
// per key (Phase 6 adds a per-key mutex).
type Service struct {
	db         *sql.DB
	masterKey  []byte
	newFetcher FetcherFactory
	// now is injectable for deterministic tests.
	now func() time.Time
}

// NewService wires a sync service.
func NewService(db *sql.DB, masterKey []byte, factory FetcherFactory) *Service {
	if factory == nil {
		factory = DefaultFetcherFactory
	}
	return &Service{db: db, masterKey: masterKey, newFetcher: factory, now: time.Now}
}

// StableID returns the 16-hex-char prefix of sha1("host:port:username"),
// the canonical primary key for an upstream proxy. Stable across syncs
// and across rotation of webshare's own internal IDs.
func StableID(host string, port int, username string) string {
	sum := sha1.Sum(fmt.Appendf(nil, "%s:%d:%s", host, port, username))
	return hex.EncodeToString(sum[:])[:16]
}

// SyncKey reconciles upstream_proxies for the given API key with the live
// webshare response. On success, ApiKey.LastSyncedAt is set and
// LastSyncError cleared. On error, LastSyncError is set and LastSyncedAt
// is left untouched (so the UI can still show the most recent good sync).
func (s *Service) SyncKey(ctx context.Context, keyID int64) error {
	plain, label, err := repo.GetApiKeyPlain(ctx, s.db, s.masterKey, keyID)
	if err != nil {
		return fmt.Errorf("load api key %d: %w", keyID, err)
	}

	fetcher := s.newFetcher(plain)
	proxies, fetchErr := fetcher.ListProxies(ctx)
	if fetchErr != nil {
		// Record the error on a best-effort basis; surface the original.
		_ = repo.MarkApiKeySyncError(ctx, s.db, keyID, fetchErr.Error())
		return fmt.Errorf("fetch proxies for key %d: %w", keyID, fetchErr)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := loadExistingUpstreams(ctx, tx, keyID)
	if err != nil {
		return err
	}

	sanitized := sanitizeLabel(label)
	seqMap := buildSeqMap(existing, sanitized)

	now := s.now().UTC()
	seen := make(map[string]struct{}, len(proxies))
	for _, p := range proxies {
		id := StableID(p.ProxyAddress, p.Port, p.Username)
		seen[id] = struct{}{}

		encPwd, err := crypto.Encrypt(s.masterKey, []byte(p.Password), crypto.ColumnAAD(upstreamPasswordAAD))
		if err != nil {
			return fmt.Errorf("encrypt password: %w", err)
		}

		if existingRow, ok := existing[id]; ok {
			// Existing row: refresh mutable metadata, mark alive, leave
			// display_name untouched so user renames stick — UNLESS the
			// stored name still uses the legacy "{CC}-{label}-{NN}" form,
			// in which case rewrite it to the canonical "{label}-{CC}-{NN}".
			newDN := ""
			if _, _, legacySeq, isLegacy := parseLegacyDisplayName(existingRow.displayName); isLegacy {
				newDN = formatDisplayName(p.CountryCode, sanitized, legacySeq)
			}
			if newDN != "" {
				if _, err := tx.ExecContext(ctx, `
					UPDATE upstream_proxies
					   SET host=?, port=?, username=?, encrypted_password=?,
					       country_code=?, city_name=?, display_name=?,
					       alive=1, last_seen_at=?
					 WHERE id=?
				`, p.ProxyAddress, p.Port, p.Username, encPwd,
					p.CountryCode, p.CityName, newDN, now, id); err != nil {
					return fmt.Errorf("update upstream %s: %w", id, err)
				}
			} else {
				if _, err := tx.ExecContext(ctx, `
					UPDATE upstream_proxies
					   SET host=?, port=?, username=?, encrypted_password=?,
					       country_code=?, city_name=?, alive=1, last_seen_at=?
					 WHERE id=?
				`, p.ProxyAddress, p.Port, p.Username, encPwd,
					p.CountryCode, p.CityName, now, id); err != nil {
					return fmt.Errorf("update upstream %s: %w", id, err)
				}
			}
			continue
		}

		// New row: allocate next seq for this country, generate DisplayName.
		seqMap[p.CountryCode]++
		dn := formatDisplayName(p.CountryCode, sanitized, seqMap[p.CountryCode])
		// Set `source` explicitly rather than relying on the column DEFAULT —
		// the load path (loadExistingUpstreams) and the stale-marker UPDATE
		// both filter on source='webshare', so the value here is part of the
		// load/store invariant, not a cosmetic write.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO upstream_proxies
				(id, source, source_api_key_id, host, port, username, encrypted_password,
				 protocol, display_name, country_code, city_name, alive, last_seen_at)
			VALUES (?, 'webshare', ?, ?, ?, ?, ?, 'http', ?, ?, ?, 1, ?)
		`, id, keyID, p.ProxyAddress, p.Port, p.Username, encPwd,
			dn, p.CountryCode, p.CityName, now); err != nil {
			return fmt.Errorf("insert upstream %s: %w", id, err)
		}
	}

	// Mark anything we used to know about this key but no longer see as not-alive.
	// Guard on source='webshare' so a future bug that mis-tags a manual row
	// with a source_api_key_id never corrupts the user's manual entries.
	for id := range existing {
		if _, ok := seen[id]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE upstream_proxies SET alive=0 WHERE id=? AND source='webshare'`, id,
		); err != nil {
			return fmt.Errorf("mark stale %s: %w", id, err)
		}
	}

	if err := repo.MarkApiKeySyncSuccess(ctx, tx, keyID, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

type existingUpstream struct {
	id          string
	displayName string
	countryCode string
}

func loadExistingUpstreams(ctx context.Context, tx *sql.Tx, keyID int64) (map[string]existingUpstream, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, display_name, country_code FROM upstream_proxies
		  WHERE source_api_key_id = ? AND source = 'webshare'`,
		keyID,
	)
	if err != nil {
		return nil, fmt.Errorf("load existing upstreams: %w", err)
	}
	defer rows.Close()
	out := make(map[string]existingUpstream)
	for rows.Next() {
		var e existingUpstream
		if err := rows.Scan(&e.id, &e.displayName, &e.countryCode); err != nil {
			return nil, err
		}
		out[e.id] = e
	}
	return out, rows.Err()
}

// buildSeqMap scans existing auto-form display_names and records, per
// country code, the highest seq number already in use. New upstream
// allocations start at that+1 within the same (country, key) bucket.
// Renamed rows (those that don't match the auto-form regex) are ignored
// here — they can leave seq "holes" but we never reuse a slot, so the
// auto-form uniqueness invariant holds.
func buildSeqMap(existing map[string]existingUpstream, sanitizedLabel string) map[string]int {
	out := map[string]int{}
	for _, e := range existing {
		cc, lab, n, ok := parseDisplayName(e.displayName)
		if !ok || cc != e.countryCode || lab != sanitizedLabel {
			continue
		}
		if n > out[cc] {
			out[cc] = n
		}
	}
	return out
}

