package routing_test

import (
	"context"
	"sync"
	"testing"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/routing"
	"github.com/guofan/webshare-proxy/internal/store"
	"go.uber.org/goleak"
)

func mustMK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, crypto.MasterKeySize)
	for i := range k {
		k[i] = byte(i + 7)
	}
	return k
}

// seedRouting inserts one key, one upstream, and one local_user mapped to
// the upstream. Returns ids needed by the test.
func seedRouting(t *testing.T, db *store.DBHandle) (apiKeyID int64, upstreamID, username string) {
	t.Helper()
	ctx := context.Background()
	mk := mustMK(t)
	id, err := repo.InsertApiKey(ctx, db.DB, mk, "Premium", "sk_test")
	if err != nil {
		t.Fatal(err)
	}
	apiKeyID = id

	encPwd, err := crypto.Encrypt(mk, []byte("upstream-pw"), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	if err != nil {
		t.Fatal(err)
	}
	upstreamID = "abc1234567890def"
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO upstream_proxies
			(id, source_api_key_id, host, port, username, encrypted_password, protocol,
			 display_name, country_code, city_name, last_seen_at)
		VALUES (?, ?, '1.2.3.4', 8080, 'upU', ?, 'http', 'US-Premium-01', 'US', '', datetime('now'))`,
		upstreamID, apiKeyID, encPwd); err != nil {
		t.Fatal(err)
	}

	username = "alice"
	if err := repo.InsertLocalUser(ctx, db.DB, username, "alicepw", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateLocalUserMapping(ctx, db.DB, username, &upstreamID); err != nil {
		t.Fatal(err)
	}
	return apiKeyID, upstreamID, username
}

func TestHydrateBuildsResolvedUsers(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()

	_, upstreamID, username := seedRouting(t, db)

	core := routing.NewCore(db.DB, mustMK(t))
	if err := core.Hydrate(context.Background()); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	state := core.Snapshot()
	if state == nil {
		t.Fatal("Snapshot returned nil after Hydrate")
	}
	if len(state.Users) != 1 {
		t.Fatalf("Users len = %d want 1", len(state.Users))
	}
	u, ok := state.Users[username]
	if !ok {
		t.Fatalf("user %q missing", username)
	}
	if u.PasswordPlain != "alicepw" {
		t.Errorf("PasswordPlain = %q", u.PasswordPlain)
	}
	if u.UpstreamID != upstreamID {
		t.Errorf("UpstreamID = %q want %q", u.UpstreamID, upstreamID)
	}
	if u.Upstream == nil || u.Upstream.Host != "1.2.3.4" {
		t.Errorf("Upstream not resolved: %+v", u.Upstream)
	}
	if u.UpstreamPwd != "upstream-pw" {
		t.Errorf("UpstreamPwd not decrypted: %q", u.UpstreamPwd)
	}
	if u.Broken {
		t.Error("Broken should be false for a healthy mapping")
	}
	if u.CancelGroup == nil {
		t.Error("CancelGroup should be allocated")
	}
}

func TestHydrateMarksBrokenForMissingUpstream(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	ctx := context.Background()

	// Insert a user with no mapping at all.
	if err := repo.InsertLocalUser(ctx, db.DB, "lonely", "pw", ""); err != nil {
		t.Fatal(err)
	}
	core := routing.NewCore(db.DB, mustMK(t))
	if err := core.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	u := core.Snapshot().Users["lonely"]
	if u == nil || !u.Broken {
		t.Fatalf("unmapped user should be Broken; got %+v", u)
	}
}

func TestSwapIsObservable(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()

	core := routing.NewCore(db.DB, mustMK(t))
	if err := core.Hydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := core.Snapshot()

	// Build a synthetic next state and swap.
	next := &routing.RoutingState{Users: map[string]*routing.ResolvedUser{}, Version: first.Version + 1}
	core.Swap(next)

	got := core.Snapshot()
	if got.Version != first.Version+1 {
		t.Fatalf("Swap not observed: version still %d", got.Version)
	}
}

func TestCancelGroupCancelIsIdempotent(t *testing.T) {
	defer goleak.VerifyNone(t)
	cg := routing.NewCancelGroup()
	cg.Cancel()
	cg.Cancel() // must not panic
	select {
	case <-cg.Context().Done():
	default:
		t.Fatal("Context().Done() should fire after Cancel")
	}
}

func TestSnapshotIsRaceFree(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()

	core := routing.NewCore(db.DB, mustMK(t))
	if err := core.Hydrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 64 concurrent readers, 16 concurrent writers — must not race.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = core.Snapshot()
				}
			}
		}()
	}
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				select {
				case <-stop:
					return
				default:
					core.Swap(&routing.RoutingState{Users: map[string]*routing.ResolvedUser{}})
				}
			}
		}()
	}
	// Let them grind for a bit then stop.
	stopAfter(t, stop, 50)
	wg.Wait()
}

// stopAfter signals stop after the given ms via a small goroutine.
// Kept tiny so the test stays fast.
func stopAfter(t *testing.T, stop chan struct{}, ms int) {
	t.Helper()
	timer := newTimer(ms)
	go func() {
		<-timer
		close(stop)
	}()
}
