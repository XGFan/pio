package sync

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/store"
	"github.com/guofan/webshare-proxy/internal/webshare"
)

// fakeFetcher returns canned data for ListProxies. Each test constructs one
// directly; the factory closure makes Service.SyncKey see the same fake
// regardless of which API-key plaintext it tries to dispatch on.
type fakeFetcher struct {
	proxies []webshare.Proxy
	err     error
	calls   int
}

func (f *fakeFetcher) ListProxies(ctx context.Context) ([]webshare.Proxy, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.proxies, nil
}

type fixture struct {
	t         *testing.T
	db        *sql.DB
	masterKey []byte
	keyID     int64
	fetcher   *fakeFetcher
	svc       *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := store.OpenInMemory(ctx)
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	mk := make([]byte, crypto.MasterKeySize)
	for i := range mk {
		mk[i] = byte(i + 1)
	}

	id, err := repo.InsertApiKey(ctx, db, mk, "US Premium", "sk_live_test_123")
	if err != nil {
		t.Fatalf("InsertApiKey: %v", err)
	}

	fake := &fakeFetcher{}
	svc := NewService(db, mk, func(apiKey string) Fetcher {
		// Sanity check that the decrypted key is what we put in.
		if apiKey != "sk_live_test_123" {
			t.Errorf("factory got wrong API key %q", apiKey)
		}
		return fake
	})

	return &fixture{t: t, db: db, masterKey: mk, keyID: id, fetcher: fake, svc: svc}
}

func mkProxy(host string, port int, user, pw, cc string) webshare.Proxy {
	return webshare.Proxy{
		ProxyAddress: host, Port: port, Username: user, Password: pw, CountryCode: cc,
	}
}

func TestStableIDIsDeterministic(t *testing.T) {
	got := StableID("1.1.1.1", 1080, "u")
	want := func() string {
		h := sha1.Sum([]byte("1.1.1.1:1080:u"))
		return hex.EncodeToString(h[:])[:16]
	}()
	if got != want {
		t.Fatalf("StableID = %q want %q", got, want)
	}
}

func TestFirstSyncInsertsAllUpstreams(t *testing.T) {
	fx := newFixture(t)
	fx.fetcher.proxies = []webshare.Proxy{
		mkProxy("1.1.1.1", 1080, "u1", "p1", "US"),
		mkProxy("2.2.2.2", 1080, "u2", "p2", "US"),
		mkProxy("3.3.3.3", 1080, "u3", "p3", "DE"),
	}
	if err := fx.svc.SyncKey(context.Background(), fx.keyID); err != nil {
		t.Fatalf("SyncKey: %v", err)
	}

	rows := dumpUpstreams(t, fx.db, fx.keyID)
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}

	// Verify country_code persisted, alive=true, encrypted_password decrypts back.
	for _, r := range rows {
		if !r.alive {
			t.Errorf("%s: alive=false on initial insert", r.id)
		}
		if r.countryCode != "US" && r.countryCode != "DE" {
			t.Errorf("%s: unexpected country %q", r.id, r.countryCode)
		}
		pw, err := crypto.Decrypt(fx.masterKey, r.encryptedPassword, crypto.ColumnAAD(upstreamPasswordAAD))
		if err != nil {
			t.Errorf("%s: decrypt password: %v", r.id, err)
		}
		if !strings.HasPrefix(string(pw), "p") {
			t.Errorf("%s: password round-trip failed: %q", r.id, pw)
		}
	}

	// ApiKey.LastSyncedAt set, LastSyncError cleared.
	keys, err := repo.ListApiKeys(context.Background(), fx.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].LastSyncedAt == nil {
		t.Fatalf("LastSyncedAt not set: %+v", keys)
	}
	if keys[0].LastSyncError != "" {
		t.Errorf("LastSyncError = %q want empty", keys[0].LastSyncError)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	fx := newFixture(t)
	fx.fetcher.proxies = []webshare.Proxy{
		mkProxy("1.1.1.1", 1080, "u1", "p1", "US"),
		mkProxy("2.2.2.2", 1080, "u2", "p2", "US"),
	}
	ctx := context.Background()
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}
	before := contentHashIgnoringTimes(t, fx.db, fx.keyID, fx.masterKey)
	beforeCount := rowCount(t, fx.db, fx.keyID)

	// Second sync, same fetch result.
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}
	after := contentHashIgnoringTimes(t, fx.db, fx.keyID, fx.masterKey)
	afterCount := rowCount(t, fx.db, fx.keyID)

	if beforeCount != afterCount {
		t.Fatalf("row count drifted: %d -> %d", beforeCount, afterCount)
	}
	if before != after {
		t.Fatalf("content (excluding timestamps) drifted across re-sync\nbefore=%s\nafter =%s", before, after)
	}
}

func TestSyncDeletesAbsentUpstreams(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetcher.proxies = []webshare.Proxy{
		mkProxy("1.1.1.1", 1080, "u1", "p1", "US"),
		mkProxy("2.2.2.2", 1080, "u2", "p2", "US"),
	}
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}

	// Drop the second proxy from the next fetch.
	fx.fetcher.proxies = []webshare.Proxy{
		mkProxy("1.1.1.1", 1080, "u1", "p1", "US"),
	}
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}

	// The rotated-out proxy is deleted, not kept as a dead row.
	rows := dumpUpstreams(t, fx.db, fx.keyID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row (the surviving proxy), got %d", len(rows))
	}
	if !rows[0].alive {
		t.Errorf("surviving proxy should be alive")
	}
	if rows[0].host != "1.1.1.1" {
		t.Errorf("wrong surviving proxy: host=%q want 1.1.1.1", rows[0].host)
	}
}

// TestSyncDeleteUnmapsLocalUser pins the documented side-effect: when a synced
// proxy a local user is mapped to gets rotated out and deleted, the FK
// (ON DELETE SET NULL) clears that user's mapping rather than the delete
// failing or leaving a dangling reference.
func TestSyncDeleteUnmapsLocalUser(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetcher.proxies = []webshare.Proxy{
		mkProxy("1.1.1.1", 1080, "u1", "p1", "US"),
		mkProxy("2.2.2.2", 1080, "u2", "p2", "US"),
	}
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}

	dropID := StableID("2.2.2.2", 1080, "u2")
	if err := repo.InsertLocalUser(ctx, fx.db, "alice", "pw", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateLocalUserMapping(ctx, fx.db, "alice", &dropID); err != nil {
		t.Fatal(err)
	}

	// Drop the mapped proxy from the next fetch → it gets deleted.
	fx.fetcher.proxies = []webshare.Proxy{mkProxy("1.1.1.1", 1080, "u1", "p1", "US")}
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}

	u, err := repo.GetLocalUser(ctx, fx.db, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.UpstreamProxyID != nil {
		t.Errorf("expected mapping cleared (NULL) after upstream deleted, got %q", *u.UpstreamProxyID)
	}
}

func TestDisplayNameSequence(t *testing.T) {
	fx := newFixture(t)
	fx.fetcher.proxies = []webshare.Proxy{
		mkProxy("1.1.1.1", 1080, "u1", "p", "US"),
		mkProxy("2.2.2.2", 1080, "u2", "p", "US"),
		mkProxy("3.3.3.3", 1080, "u3", "p", "DE"),
		mkProxy("4.4.4.4", 1080, "u4", "p", "DE"),
		mkProxy("5.5.5.5", 1080, "u5", "p", "DE"),
	}
	if err := fx.svc.SyncKey(context.Background(), fx.keyID); err != nil {
		t.Fatal(err)
	}
	names := map[string][]string{}
	for _, r := range dumpUpstreams(t, fx.db, fx.keyID) {
		names[r.countryCode] = append(names[r.countryCode], r.displayName)
	}
	for _, ns := range names {
		sort.Strings(ns)
	}

	// Label "US Premium" sanitizes to "USPremium".
	wantUS := []string{"USPremium-US-01", "USPremium-US-02"}
	wantDE := []string{"USPremium-DE-01", "USPremium-DE-02", "USPremium-DE-03"}
	if !equalSorted(names["US"], wantUS) {
		t.Errorf("US names = %v want %v", names["US"], wantUS)
	}
	if !equalSorted(names["DE"], wantDE) {
		t.Errorf("DE names = %v want %v", names["DE"], wantDE)
	}
}

func TestRenamedDisplayNamePreservedAcrossSync(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetcher.proxies = []webshare.Proxy{
		mkProxy("1.1.1.1", 1080, "u1", "p", "US"),
	}
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}

	// Simulate UI rename via direct UPDATE (the REST endpoint lands in Phase 5).
	id := StableID("1.1.1.1", 1080, "u1")
	if _, err := fx.db.ExecContext(ctx,
		`UPDATE upstream_proxies SET display_name = ? WHERE id = ?`, "MyHomeServer", id,
	); err != nil {
		t.Fatal(err)
	}

	// Re-sync; the rename must stick.
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := fx.db.QueryRow(`SELECT display_name FROM upstream_proxies WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "MyHomeServer" {
		t.Fatalf("renamed display_name lost: got %q", got)
	}
}

func TestSyncRecordsErrorOnFetchFailure(t *testing.T) {
	fx := newFixture(t)
	fx.fetcher.err = errors.New("network down")

	err := fx.svc.SyncKey(context.Background(), fx.keyID)
	if err == nil {
		t.Fatal("expected error from SyncKey")
	}

	keys, err := repo.ListApiKeys(context.Background(), fx.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !strings.Contains(keys[0].LastSyncError, "network down") {
		t.Fatalf("LastSyncError not recorded: %+v", keys)
	}
	if keys[0].LastSyncedAt != nil {
		t.Errorf("LastSyncedAt should be untouched on failure, got %v", keys[0].LastSyncedAt)
	}
}

func TestSanitizeLabel(t *testing.T) {
	tests := []struct{ in, out string }{
		{"US Premium", "USPremium"},
		{"prod-2025", "prod-2025"},
		{"!@#$%^", "key"},
		{"123456789012345", "123456789012"},
		{"a b c d e f g h", "abcdefgh"},
	}
	for _, tc := range tests {
		if got := sanitizeLabel(tc.in); got != tc.out {
			t.Errorf("sanitizeLabel(%q) = %q want %q", tc.in, got, tc.out)
		}
	}
}

func TestParseDisplayName(t *testing.T) {
	cc, lab, seq, ok := parseDisplayName("Premium-US-03")
	if !ok || cc != "US" || lab != "Premium" || seq != 3 {
		t.Fatalf("parse mismatch: cc=%q lab=%q seq=%d ok=%v", cc, lab, seq, ok)
	}
	// Three-digit seq must round-trip correctly so a fleet of 100+ proxies
	// in one country doesn't break the auto-form regex.
	cc, lab, seq, ok = parseDisplayName("Basic-DE-123")
	if !ok || cc != "DE" || lab != "Basic" || seq != 123 {
		t.Fatalf("three-digit seq parse mismatch: cc=%q lab=%q seq=%d ok=%v", cc, lab, seq, ok)
	}
	if _, _, _, ok := parseDisplayName("MyCustomName"); ok {
		t.Fatal("renamed name should not parse")
	}
	if _, _, _, ok := parseDisplayName("premium-us-03"); ok {
		t.Fatal("lowercase country must not parse")
	}
	// Legacy form (CC first) must NOT parse as new-canonical, but SHOULD
	// parse via parseLegacyDisplayName so sync can migrate it.
	if _, _, _, ok := parseDisplayName("US-Premium-03"); ok {
		t.Fatal("legacy form must not match canonical regex")
	}
	cc, lab, seq, ok = parseLegacyDisplayName("US-Premium-03")
	if !ok || cc != "US" || lab != "Premium" || seq != 3 {
		t.Fatalf("legacy parse mismatch: cc=%q lab=%q seq=%d ok=%v", cc, lab, seq, ok)
	}
}

// Verify that an existing row with the old "{CC}-{label}-{NN}" name is
// rewritten to the new "{label}-{CC}-{NN}" form on the next sync, while a
// user-renamed row is left alone.
func TestLegacyDisplayNameMigratedOnSync(t *testing.T) {
	fx := newFixture(t)
	ctx := context.Background()
	fx.fetcher.proxies = []webshare.Proxy{
		mkProxy("1.1.1.1", 1080, "u1", "p", "US"),
		mkProxy("2.2.2.2", 1080, "u2", "p", "DE"),
	}
	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}
	// Force the two stored rows back into legacy format.
	idUS := StableID("1.1.1.1", 1080, "u1")
	idDE := StableID("2.2.2.2", 1080, "u2")
	if _, err := fx.db.ExecContext(ctx,
		`UPDATE upstream_proxies SET display_name = ? WHERE id = ?`, "US-USPremium-01", idUS,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.db.ExecContext(ctx,
		`UPDATE upstream_proxies SET display_name = ?  WHERE id = ?`, "MyRenamedNode", idDE,
	); err != nil {
		t.Fatal(err)
	}

	if err := fx.svc.SyncKey(ctx, fx.keyID); err != nil {
		t.Fatal(err)
	}

	var got string
	if err := fx.db.QueryRow(`SELECT display_name FROM upstream_proxies WHERE id = ?`, idUS).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "USPremium-US-01" {
		t.Fatalf("legacy display_name not migrated: got %q want USPremium-US-01", got)
	}
	if err := fx.db.QueryRow(`SELECT display_name FROM upstream_proxies WHERE id = ?`, idDE).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "MyRenamedNode" {
		t.Fatalf("user-renamed name lost: got %q", got)
	}
}

// --- test helpers ---

type upstreamRow struct {
	id, displayName, countryCode string
	host                         string
	port                         int
	username                     string
	encryptedPassword            []byte
	alive                        bool
}

func dumpUpstreams(t *testing.T, db *sql.DB, keyID int64) []upstreamRow {
	t.Helper()
	rows, err := db.Query(
		`SELECT id, display_name, country_code, host, port, username, encrypted_password, alive
		   FROM upstream_proxies WHERE source_api_key_id = ?
		  ORDER BY id`, keyID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []upstreamRow
	for rows.Next() {
		var r upstreamRow
		if err := rows.Scan(&r.id, &r.displayName, &r.countryCode, &r.host, &r.port, &r.username, &r.encryptedPassword, &r.alive); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func rowCount(t *testing.T, db *sql.DB, keyID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM upstream_proxies WHERE source_api_key_id = ?`, keyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// contentHashIgnoringTimes returns a stable digest of every row's
// observable state, so we can check that two syncs of the same fetch
// produced the same content even though last_seen_at and the per-row
// AES-GCM nonce change on every encrypt.
//
// "Idempotent" here means "same plaintext-equivalent state visible to a
// reader", NOT "no UPDATE issued at all" — the sync UPDATE intentionally
// rewrites encrypted_password with a fresh nonce, which is harmless. To
// detect real content drift, the helper decrypts the password column
// instead of comparing ciphertext lengths.
func contentHashIgnoringTimes(t *testing.T, db *sql.DB, keyID int64, masterKey []byte) string {
	t.Helper()
	rows := dumpUpstreams(t, db, keyID)
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		pw, err := crypto.Decrypt(masterKey, r.encryptedPassword, crypto.ColumnAAD(upstreamPasswordAAD))
		if err != nil {
			t.Fatalf("decrypt %s: %v", r.id, err)
		}
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%s|%d|%s|pw=%s|alive=%v",
			r.id, r.displayName, r.countryCode, r.host, r.port, r.username, pw, r.alive,
		))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}

func equalSorted(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
