package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guofan/pia/internal/api"
	"github.com/guofan/pia/internal/auth"
	"github.com/guofan/pia/internal/crypto"
	"github.com/guofan/pia/internal/repo"
	"github.com/guofan/pia/internal/routing"
	"github.com/guofan/pia/internal/store"
)

func subKey() []byte {
	k := make([]byte, crypto.MasterKeySize)
	for i := range k {
		k[i] = byte(i + 5)
	}
	return k
}

func seedSubUpstream(t *testing.T, db *store.DBHandle, mk []byte, id, displayName string) {
	t.Helper()
	ctx := context.Background()
	keyID, err := repo.InsertApiKey(ctx, db.DB, mk, "L-"+id, "sk_"+id)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := crypto.Encrypt(mk, []byte("uppw"), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO upstream_proxies
			(id, source_api_key_id, host, port, username, encrypted_password, protocol,
			 display_name, country_code, last_seen_at)
		 VALUES (?, ?, '127.0.0.1', 9000, 'u', ?, 'http', ?, 'US', datetime('now'))`,
		id, keyID, enc, displayName); err != nil {
		t.Fatal(err)
	}
}

// buildSubServer seeds a subscription-enabled daemon with one routable proxy
// (US-A-01) and a duplicate-named pair (DUP) that is ambiguous — so only
// US-A-01 should appear in the subscription.
func buildSubServer(t *testing.T, enabled bool, universalPwd string) http.Handler {
	t.Helper()
	ctx := context.Background()
	db := store.MustOpenInMemoryTest(t)
	t.Cleanup(func() { _ = db.Close() })
	mk := subKey()

	st, err := repo.LoadSettings(ctx, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	st.SubscriptionEnabled = enabled
	st.SubscriptionHost = "proxy.example.com"
	st.ProxyPort = 8080
	if err := repo.UpdateSettings(ctx, db.DB, st); err != nil {
		t.Fatal(err)
	}
	if universalPwd != "" {
		if err := repo.SetUniversalProxyPassword(ctx, db.DB, mk, universalPwd); err != nil {
			t.Fatal(err)
		}
	}

	seedSubUpstream(t, db, mk, "aaaaaaaaaaaaaaaa", "US-A-01")
	seedSubUpstream(t, db, mk, "cccccccccccccccc", "DUP")
	seedSubUpstream(t, db, mk, "dddddddddddddddd", "DUP")

	core := routing.NewCore(db.DB, mk)
	if err := core.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	return api.New(api.Deps{DB: db.DB, MasterKey: mk, Core: core, DenyList: auth.New(nil)}).Handler()
}

// TestSubscription_BruteForceBanned confirms the public endpoint is protected
// by the shared per-IP deny-list: after 10 failed password attempts the IP is
// banned (403) instead of getting another 401 oracle.
func TestSubscription_BruteForceBanned(t *testing.T) {
	h := buildSubServer(t, true, "masterpw")
	const ip = "203.0.113.7:5555"

	for i := 0; i < 10; i++ {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/subscription?password=wrong", nil)
		req.RemoteAddr = ip
		h.ServeHTTP(rr, req)
		if rr.Code != 401 {
			t.Fatalf("attempt %d: status %d, want 401", i+1, rr.Code)
		}
	}
	// 11th request from the now-banned IP is refused before the compare.
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/subscription?password=wrong", nil)
	req.RemoteAddr = ip
	h.ServeHTTP(rr, req)
	if rr.Code != 403 {
		t.Fatalf("post-ban status = %d, want 403", rr.Code)
	}
	// Even the CORRECT password is refused while banned (the gate is first).
	rr2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/subscription?password=masterpw", nil)
	req2.RemoteAddr = ip
	h.ServeHTTP(rr2, req2)
	if rr2.Code != 403 {
		t.Fatalf("banned IP with correct password = %d, want 403", rr2.Code)
	}
}

func TestSubscription_RoutableProxiesOnly(t *testing.T) {
	h := buildSubServer(t, true, "masterpw")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/subscription?password=masterpw", nil))

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}
	body := rr.Body.String()

	want := "socks://US-A-01:masterpw@proxy.example.com:8080#US-A-01"
	if !strings.Contains(body, want) {
		t.Errorf("body missing routable line.\n got: %q\nwant substring: %q", body, want)
	}
	if strings.Contains(body, "DUP") {
		t.Error("ambiguous (duplicate display name) upstream leaked into subscription")
	}
	// Exactly one routable proxy → exactly one non-empty line.
	lines := strings.FieldsFunc(body, func(r rune) bool { return r == '\n' })
	if len(lines) != 1 {
		t.Errorf("got %d lines, want 1: %q", len(lines), lines)
	}
}

func TestSubscription_WrongPassword401(t *testing.T) {
	h := buildSubServer(t, true, "masterpw")
	for _, q := range []string{"/subscription?password=nope", "/subscription"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, q, nil))
		if rr.Code != 401 {
			t.Errorf("%s → status %d, want 401", q, rr.Code)
		}
	}
}

func TestSubscription_DisabledReturns404(t *testing.T) {
	h := buildSubServer(t, false, "masterpw") // subscription disabled
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/subscription?password=masterpw", nil))
	if rr.Code != 404 {
		t.Fatalf("disabled subscription → status %d, want 404", rr.Code)
	}
}

func TestSubscription_NoUniversalPasswordReturns404(t *testing.T) {
	h := buildSubServer(t, true, "") // enabled but no universal password
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/subscription?password=anything", nil))
	if rr.Code != 404 {
		t.Fatalf("no universal password → status %d, want 404", rr.Code)
	}
}

func TestSubscriptionURL_BuiltFromRequestHost(t *testing.T) {
	h := buildSubServer(t, true, "masterpw")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/subscription-url", nil)
	req.Host = "panel.example.com"
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var out struct {
		Enabled bool   `json:"enabled"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Enabled {
		t.Fatal("enabled = false, want true")
	}
	want := "http://panel.example.com/subscription?password=masterpw"
	if out.URL != want {
		t.Errorf("url = %q, want %q", out.URL, want)
	}
}

func TestSubscriptionURL_DisabledReturnsEmpty(t *testing.T) {
	h := buildSubServer(t, false, "masterpw")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/subscription-url", nil))
	var out struct {
		Enabled bool   `json:"enabled"`
		URL     string `json:"url"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out.Enabled || out.URL != "" {
		t.Errorf("disabled → {enabled:%v url:%q}, want {false, \"\"}", out.Enabled, out.URL)
	}
}
