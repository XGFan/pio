package tunnel_test

import (
	"context"
	"errors"
	"testing"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/routing"
	"github.com/guofan/webshare-proxy/internal/store"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// insertUpstreamRow inserts a webshare-style upstream with the given id,
// display name, and plaintext upstream password. Each row gets its own
// api_keys parent so the FK is satisfied.
func insertUpstreamRow(t *testing.T, db *store.DBHandle, mk []byte, id, displayName, pwd string) {
	t.Helper()
	ctx := context.Background()
	keyID, err := repo.InsertApiKey(ctx, db.DB, mk, "L-"+id, "sk_"+id)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := crypto.Encrypt(mk, []byte(pwd), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO upstream_proxies
			(id, source_api_key_id, host, port, username, encrypted_password, protocol,
			 display_name, country_code, last_seen_at)
		 VALUES (?, ?, '127.0.0.1', 9000, 'upU', ?, 'http', ?, 'US', datetime('now'))`,
		id, keyID, enc, displayName); err != nil {
		t.Fatal(err)
	}
}

// hydratedMgr builds a routing core over db and returns a tunnel.Manager.
func hydratedMgr(t *testing.T, db *store.DBHandle, mk []byte) *tunnel.Manager {
	t.Helper()
	core := routing.NewCore(db.DB, mk)
	if err := core.Hydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return tunnel.New(core)
}

func TestAcquireUniversal_RoutesByDisplayName(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	insertUpstreamRow(t, db, mk, "1111111111111111", "US-A-01", "pwA")
	insertUpstreamRow(t, db, mk, "2222222222222222", "US-B-01", "pwB")
	if err := repo.SetUniversalProxyPassword(context.Background(), db.DB, mk, "master"); err != nil {
		t.Fatal(err)
	}
	mgr := hydratedMgr(t, db, mk)

	// Display name "US-A-01" + universal password resolves to upstream A.
	up, gotPwd, cg, err := mgr.Acquire(context.Background(), "US-A-01", "master")
	if err != nil {
		t.Fatalf("Acquire US-A-01: %v", err)
	}
	if gotPwd != "pwA" {
		t.Errorf("upstream A password = %q want pwA", gotPwd)
	}
	if up == nil || cg == nil {
		t.Errorf("expected upstream + cancel group, got up=%v cg=%v", up, cg)
	}

	// A different display name selects the other upstream — proving routing
	// is by name, not a fixed target.
	_, gotPwdB, _, err := mgr.Acquire(context.Background(), "US-B-01", "master")
	if err != nil {
		t.Fatalf("Acquire US-B-01: %v", err)
	}
	if gotPwdB != "pwB" {
		t.Errorf("upstream B password = %q want pwB", gotPwdB)
	}
}

func TestAcquireUniversal_WrongPassword(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	insertUpstreamRow(t, db, mk, "1111111111111111", "US-A-01", "pwA")
	if err := repo.SetUniversalProxyPassword(context.Background(), db.DB, mk, "master"); err != nil {
		t.Fatal(err)
	}
	mgr := hydratedMgr(t, db, mk)

	_, _, _, err := mgr.Acquire(context.Background(), "US-A-01", "not-the-master")
	if !errors.Is(err, tunnel.ErrUnknownUser) {
		t.Fatalf("err = %v, want ErrUnknownUser", err)
	}
}

func TestAcquireUniversal_UnknownDisplayName(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	insertUpstreamRow(t, db, mk, "1111111111111111", "US-A-01", "pwA")
	if err := repo.SetUniversalProxyPassword(context.Background(), db.DB, mk, "master"); err != nil {
		t.Fatal(err)
	}
	mgr := hydratedMgr(t, db, mk)

	_, _, _, err := mgr.Acquire(context.Background(), "US-NOPE-99", "master")
	if !errors.Is(err, tunnel.ErrUnknownUser) {
		t.Fatalf("err = %v, want ErrUnknownUser", err)
	}
}

func TestAcquireUniversal_DisabledWhenUnset(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	insertUpstreamRow(t, db, mk, "1111111111111111", "US-A-01", "pwA")
	// No universal password configured → feature off.
	mgr := hydratedMgr(t, db, mk)

	_, _, _, err := mgr.Acquire(context.Background(), "US-A-01", "master")
	if !errors.Is(err, tunnel.ErrUnknownUser) {
		t.Fatalf("err = %v, want ErrUnknownUser (feature disabled)", err)
	}
}

func TestAcquireUniversal_AmbiguousDisplayNameRefused(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	// Two upstreams share a display name → ambiguous → not routable.
	insertUpstreamRow(t, db, mk, "1111111111111111", "DUP-01", "pwA")
	insertUpstreamRow(t, db, mk, "2222222222222222", "DUP-01", "pwB")
	if err := repo.SetUniversalProxyPassword(context.Background(), db.DB, mk, "master"); err != nil {
		t.Fatal(err)
	}
	mgr := hydratedMgr(t, db, mk)

	_, _, _, err := mgr.Acquire(context.Background(), "DUP-01", "master")
	if !errors.Is(err, tunnel.ErrUnknownUser) {
		t.Fatalf("err = %v, want ErrUnknownUser (ambiguous name refused)", err)
	}
}

// TestAcquireUniversal_PerUserPrecedence pins the precedence rule both ways:
// an exact (username, own-password) match always wins; the same username with
// the universal password instead routes by display name.
func TestAcquireUniversal_PerUserPrecedence(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	ctx := context.Background()

	// Upstream uA happens to have display name "alice"; upstream uB is what
	// the local user "alice" is mapped to.
	insertUpstreamRow(t, db, mk, "aaaaaaaaaaaaaaaa", "alice", "pwA")
	insertUpstreamRow(t, db, mk, "bbbbbbbbbbbbbbbb", "US-B-01", "pwB")
	if err := repo.InsertLocalUser(ctx, db.DB, "alice", "alicepw", ""); err != nil {
		t.Fatal(err)
	}
	upB := "bbbbbbbbbbbbbbbb"
	if err := repo.UpdateLocalUserMapping(ctx, db.DB, "alice", &upB); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetUniversalProxyPassword(ctx, db.DB, mk, "master"); err != nil {
		t.Fatal(err)
	}
	mgr := hydratedMgr(t, db, mk)

	// Exact per-user credential → the user's mapped upstream (uB).
	_, gotPwd, _, err := mgr.Acquire(ctx, "alice", "alicepw")
	if err != nil {
		t.Fatalf("Acquire alice/alicepw: %v", err)
	}
	if gotPwd != "pwB" {
		t.Errorf("per-user route password = %q want pwB", gotPwd)
	}

	// Same username, universal password → display-name route to uA.
	_, gotPwd2, _, err := mgr.Acquire(ctx, "alice", "master")
	if err != nil {
		t.Fatalf("Acquire alice/master: %v", err)
	}
	if gotPwd2 != "pwA" {
		t.Errorf("universal route password = %q want pwA", gotPwd2)
	}
}
