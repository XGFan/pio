package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubAPIHandler stands in for api.Server.Handler(). Returns 200 + body so
// tests can prove the request actually reached the inner handler.
func stubAPIHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func newTestServer(t *testing.T, password string) *Server {
	t.Helper()
	srv, err := New(Options{
		Bind:       "127.0.0.1:0",
		Password:   password,
		APIHandler: stubAPIHandler(`[]`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

// loginAndCookie posts /web/api/login with the given password and returns
// the session cookie, asserting a 200 response.
func loginAndCookie(t *testing.T, h http.Handler, password string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	req := httptest.NewRequest("POST", "/web/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d, body=%q", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			return c
		}
	}
	t.Fatal("login: no session cookie set")
	return nil
}

func TestRequireAuthAPI_NoCookie401(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	req := httptest.NewRequest("GET", "/api/v1/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestRequireAuthAPI_GoodCookie200(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	cookie := loginAndCookie(t, h, "secret")

	req := httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "[]" {
		t.Fatalf("expected inner handler body, got %q", rec.Body.String())
	}
}

func TestRequireAuthAPI_StaleCookie401(t *testing.T) {
	srv := newTestServer(t, "secret")
	// Force the cookie to validate as stale by issuing the session under a
	// clock the test controls, then advancing past the TTL.
	clock := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	srv.sessions.now = func() time.Time { return clock }

	tok, _ := srv.sessions.Issue()
	h := srv.handler()

	// Travel past TTL.
	srv.sessions.now = func() time.Time { return clock.Add(48 * time.Hour) }

	req := httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on stale cookie, got %d", rec.Code)
	}
}

func TestLogin_BadPassword401(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	body, _ := json.Marshal(map[string]string{"password": "wrong"})
	req := httptest.NewRequest("POST", "/web/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 on bad password, got %d", rec.Code)
	}
	// And no cookie set.
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			t.Fatal("session cookie set on bad password")
		}
	}
}

func TestLogout_RevokesSession(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	cookie := loginAndCookie(t, h, "secret")

	// Authed call works before logout.
	req := httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-logout: expected 200, got %d", rec.Code)
	}

	// Logout.
	req = httptest.NewRequest("POST", "/web/api/logout", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d", rec.Code)
	}

	// Same cookie should now be rejected.
	req = httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-logout: expected 401, got %d", rec.Code)
	}
}

func TestRootRedirect_Unauth(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func TestRootRedirect_Authed(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()
	cookie := loginAndCookie(t, h, "secret")

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/app" {
		t.Fatalf("expected redirect to /app, got %q", loc)
	}
}

func TestAppRoute_RequiresAuth(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	req := httptest.NewRequest("GET", "/app", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}

	cookie := loginAndCookie(t, h, "secret")
	req = httptest.NewRequest("GET", "/app", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authed /app: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "PIO") {
		t.Fatal("authed /app did not serve index.html")
	}
}

func TestLoginAndAssets_UnauthOK(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	for _, path := range []string{"/login", "/assets/style.css", "/assets/app.js"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200, got %d", path, rec.Code)
		}
	}
}

func TestFaviconServedAndLinked(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	// Favicon must be reachable without auth — the login page references it.
	req := httptest.NewRequest("GET", "/assets/favicon.png", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/assets/favicon.png: expected 200, got %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "image/png") {
		t.Errorf("/assets/favicon.png: expected image/png, got %q", ct)
	}

	// login.html links the favicon (unauth).
	req = httptest.NewRequest("GET", "/login", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `href="/assets/favicon.png"`) {
		t.Error("login.html does not link favicon")
	}

	// index.html (served at /app behind auth) links the favicon too.
	cookie := loginAndCookie(t, h, "secret")
	req = httptest.NewRequest("GET", "/app", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `href="/assets/favicon.png"`) {
		t.Error("index.html does not link favicon")
	}
}

func TestNew_ValidatesOptions(t *testing.T) {
	cases := []struct {
		name string
		opts Options
	}{
		{"missing bind", Options{Password: "x", APIHandler: stubAPIHandler("")}},
		{"missing password", Options{Bind: "127.0.0.1:0", APIHandler: stubAPIHandler("")}},
		{"missing handler", Options{Bind: "127.0.0.1:0", Password: "x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.opts); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// --- forward-auth mode ------------------------------------------------------

func newForwardAuthServer(t *testing.T) *Server {
	t.Helper()
	srv, err := New(Options{
		Bind:                "127.0.0.1:0",
		AuthMode:            AuthModeForwardAuth,
		APIHandler:          stubAPIHandler(`[]`),
		SubscriptionHandler: stubAPIHandler(`sub`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestForwardAuth_HeaderGrantsAPI(t *testing.T) {
	srv := newForwardAuthServer(t)
	h := srv.handler()

	// No header → 401.
	req := httptest.NewRequest("GET", "/api/v1/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no header: expected 401, got %d", rec.Code)
	}

	// With trusted header → 200, reaches inner handler.
	req = httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.Header.Set("Remote-Email", "alice@example.com")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with header: expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "[]" {
		t.Fatalf("expected inner handler body, got %q", rec.Body.String())
	}
}

func TestForwardAuth_EmptyHeaderRejected(t *testing.T) {
	srv := newForwardAuthServer(t)
	h := srv.handler()

	// Present but empty header is not proof of identity.
	req := httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.Header.Set("Remote-Email", "")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("empty header: expected 401, got %d", rec.Code)
	}
}

func TestForwardAuth_SubscriptionStaysPublic(t *testing.T) {
	srv := newForwardAuthServer(t)
	h := srv.handler()

	// /subscription must remain reachable without the auth header.
	req := httptest.NewRequest("GET", "/subscription", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("subscription: expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "sub" {
		t.Fatalf("subscription: expected handler body, got %q", rec.Body.String())
	}
}

func TestForwardAuth_LoginRedirectsToApp(t *testing.T) {
	srv := newForwardAuthServer(t)
	h := srv.handler()

	// /login is a dead end in forward-auth mode — it must redirect to /app
	// rather than serve the (unusable) password form. True with or without the
	// trusted header; here without it, /app then 401s via its own gate.
	req := httptest.NewRequest("GET", "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/app" {
		t.Fatalf("expected redirect to /app, got %q", loc)
	}
}

func TestLoginPage_PasswordMode(t *testing.T) {
	srv := newTestServer(t, "secret")
	h := srv.handler()

	// Unauthenticated: serve the form (200, login.html body).
	req := httptest.NewRequest("GET", "/login", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unauth /login: expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "login-form") {
		t.Fatal("unauth /login did not serve login.html")
	}

	// Already authenticated: skip the form, redirect to /app.
	cookie := loginAndCookie(t, h, "secret")
	req = httptest.NewRequest("GET", "/login", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("authed /login: expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/app" {
		t.Fatalf("authed /login: expected redirect to /app, got %q", loc)
	}
}

func TestForwardAuth_RootRedirectsToApp(t *testing.T) {
	srv := newForwardAuthServer(t)
	h := srv.handler()

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Remote-Email", "alice@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/app" {
		t.Fatalf("expected redirect to /app, got %q", loc)
	}

	// Without the header the HTML deny is a 401 (no local login page to use).
	req = httptest.NewRequest("GET", "/", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth root: expected 401, got %d", rec.Code)
	}
}

func TestForwardAuth_LoginDisabled(t *testing.T) {
	srv := newForwardAuthServer(t)
	h := srv.handler()

	// Posting an empty password must NOT mint a session (guards the
	// empty-vs-empty constant-time match when no password is configured).
	body, _ := json.Marshal(map[string]string{"password": ""})
	req := httptest.NewRequest("POST", "/web/api/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// 404 proves the mode guard fired (before any decode/compare), not an
	// incidental error path.
	if rec.Code != http.StatusNotFound {
		t.Fatalf("login must be disabled (404) in forward-auth mode, got %d", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == cookieName {
			t.Fatal("session cookie set in forward-auth mode")
		}
	}
}

// TestPasswordMode_IgnoresTrustedHeader locks the mode branch: in password mode
// the forward-auth identity header must NOT grant access — only a valid cookie.
func TestPasswordMode_IgnoresTrustedHeader(t *testing.T) {
	srv := newTestServer(t, "secret") // default AuthModePassword
	h := srv.handler()

	req := httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.Header.Set("Remote-Email", "attacker@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("password mode must ignore the trusted header: expected 401, got %d", rec.Code)
	}
}

func TestForwardAuth_SessionStatusReflectsHeader(t *testing.T) {
	srv := newForwardAuthServer(t)
	h := srv.handler()

	req := httptest.NewRequest("GET", "/web/api/session", nil)
	req.Header.Set("Remote-Email", "alice@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var got map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got["authenticated"] {
		t.Fatal("expected authenticated=true with trusted header")
	}
}

func TestForwardAuth_CustomHeader(t *testing.T) {
	srv, err := New(Options{
		Bind:          "127.0.0.1:0",
		AuthMode:      AuthModeForwardAuth,
		TrustedHeader: "Remote-User",
		APIHandler:    stubAPIHandler(`[]`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h := srv.handler()

	// Default header is now ignored; only the configured one counts.
	req := httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.Header.Set("Remote-Email", "alice@example.com")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong header: expected 401, got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/v1/keys", nil)
	req.Header.Set("Remote-User", "alice")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configured header: expected 200, got %d", rec.Code)
	}
}

func TestNew_ForwardAuthDefaultsHeaderNoPassword(t *testing.T) {
	srv, err := New(Options{
		Bind:       "127.0.0.1:0",
		AuthMode:   AuthModeForwardAuth,
		APIHandler: stubAPIHandler(""),
	})
	if err != nil {
		t.Fatalf("forward-auth without password should be valid: %v", err)
	}
	if srv.opts.TrustedHeader != DefaultTrustedHeader {
		t.Fatalf("expected default trusted header %q, got %q", DefaultTrustedHeader, srv.opts.TrustedHeader)
	}
}

func TestNew_InvalidAuthMode(t *testing.T) {
	_, err := New(Options{
		Bind:       "127.0.0.1:0",
		AuthMode:   "saml",
		APIHandler: stubAPIHandler(""),
	})
	if err == nil {
		t.Fatal("expected error for invalid auth mode")
	}
}

func TestIsLoopbackBind(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"127.0.0.1:9091", true},
		{"localhost:9091", true},
		{"[::1]:9091", true},
		{"0.0.0.0:9091", false},
		{"192.168.1.5:9091", false},
		{"127.0.0.1", true},
		{"", true},
	}
	for _, tc := range cases {
		if got := IsLoopbackBind(tc.in); got != tc.want {
			t.Errorf("IsLoopbackBind(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
