// Package integration runs end-to-end scenarios that span multiple
// internal packages plus the mockwebshare test doubles.
//
// AC#2 (HTTP routing by user) lands here in Phase 2. Phase 3 adds AC#5
// (SOCKS5 parity) + AC#6 (SOCKS5 ATYP/CMD matrix). Phase 4 adds AC#3
// (hot-switch ≤ 2s). Phase 5 adds REST-driven scenarios.
package integration

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/listener"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/routing"
	"github.com/guofan/webshare-proxy/internal/store"
	"github.com/guofan/webshare-proxy/internal/tunnel"
	"github.com/guofan/webshare-proxy/test/mockwebshare"
)

// scenario bundles every piece of plumbing an end-to-end test needs:
//
//   - an EchoTarget that the proxy chain ultimately bridges to;
//   - a mock HTTPUpstream impersonating webshare's CONNECT endpoint;
//   - a seeded in-memory SQLite + hydrated routing.Core;
//   - a running listener.HTTPProxy.
//
// All resources are torn down via t.Cleanup so individual tests stay short.
type scenario struct {
	t          *testing.T
	echo       *mockwebshare.EchoTarget
	upstream   *mockwebshare.HTTPUpstream
	db         *store.DBHandle
	mk         []byte
	core       *routing.Core
	mgr        *tunnel.Manager
	proxy      *listener.HTTPProxy
	proxyAddr  string
	cancelProxy context.CancelFunc
}

const (
	scenUpstreamUser = "upU"
	scenUpstreamPwd  = "upP"
	scenLocalUser    = "alice"
	scenLocalPwd     = "alicepw"
)

func newScenario(t *testing.T) *scenario {
	t.Helper()
	echo, err := mockwebshare.NewEchoTarget()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })

	up, err := mockwebshare.NewHTTPUpstream(scenUpstreamUser, scenUpstreamPwd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = up.Close() })

	db := store.MustOpenInMemoryTest(t)
	mk := make([]byte, crypto.MasterKeySize)
	for i := range mk {
		mk[i] = byte(i + 11)
	}

	ctx := context.Background()
	keyID, err := repo.InsertApiKey(ctx, db.DB, mk, "Premium", "sk_test")
	if err != nil {
		t.Fatal(err)
	}
	encPwd, _ := crypto.Encrypt(mk, []byte(scenUpstreamPwd), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	upstreamID := "aaaaaaaaaaaaaaaa"
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO upstream_proxies
			(id, source_api_key_id, host, port, username, encrypted_password, protocol,
			 display_name, country_code, alive, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, 'http', 'US-Premium-01', 'US', 1, datetime('now'))`,
		upstreamID, keyID, up.Host(), up.Port(), scenUpstreamUser, encPwd); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertLocalUser(ctx, db.DB, scenLocalUser, scenLocalPwd, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateLocalUserMapping(ctx, db.DB, scenLocalUser, &upstreamID); err != nil {
		t.Fatal(err)
	}

	core := routing.NewCore(db.DB, mk)
	if err := core.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	mgr := tunnel.New(core)

	proxy := listener.NewHTTPProxy("127.0.0.1:0", mgr, nil, nil, nil)
	if err := proxy.Bind(); err != nil {
		t.Fatal(err)
	}
	proxyCtx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = proxy.Serve(proxyCtx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Errorf("HTTPProxy.Serve did not return within 3s of cancel")
		}
	})

	return &scenario{
		t:           t,
		echo:        echo,
		upstream:    up,
		db:          db,
		mk:          mk,
		core:        core,
		mgr:         mgr,
		proxy:       proxy,
		proxyAddr:   proxy.Addr(),
		cancelProxy: cancel,
	}
}

// dialAndConnect connects to the local HTTP proxy, sends CONNECT for the
// given target with (user, pass) creds, reads the response, and returns
// the still-open client conn on a 200.
func dialAndConnect(t *testing.T, proxyAddr, target, user, pass string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	authHdr := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		target, target, authHdr)
	if _, err := io.WriteString(conn, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = conn.Close()
		t.Fatalf("CONNECT got status %d, body=%q", resp.StatusCode, body)
	}
	return conn
}

func TestAC2_HTTPRoutingByUsername(t *testing.T) {
	s := newScenario(t)

	// Tunnel: client → localProxy → mockUpstream(CONNECT) → echo target.
	conn := dialAndConnect(t, s.proxyAddr, s.echo.Addr(), scenLocalUser, scenLocalPwd)
	defer conn.Close()

	// Bytes round-trip through the entire chain.
	const payload = "ping-acceptance"
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("echo mismatch: got %q want %q", buf, payload)
	}

	// The mock upstream must have seen exactly one CONNECT request and the
	// outbound Proxy-Authorization carried the UPSTREAM's credentials, not
	// the local user's — proving the listener rewrote auth.
	reqs := s.upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("mock upstream saw %d requests, want 1: %+v", len(reqs), reqs)
	}
	if reqs[0].Username != scenUpstreamUser {
		t.Errorf("mock upstream saw username %q, want %q", reqs[0].Username, scenUpstreamUser)
	}
	if reqs[0].Password != scenUpstreamPwd {
		t.Errorf("mock upstream password mismatch (test seam): got %q", reqs[0].Password)
	}
	if reqs[0].Target != s.echo.Addr() {
		t.Errorf("mock upstream target %q, want %q", reqs[0].Target, s.echo.Addr())
	}
}

// TestUniversalPassword_HTTPRoutingByDisplayName proves the full chain for
// the universal-password feature: with a universal password configured, a
// client authenticating as (upstream display name + universal password) —
// using NO dedicated local user — is routed to that upstream, and the
// listener rewrites auth to the upstream's own credentials.
func TestUniversalPassword_HTTPRoutingByDisplayName(t *testing.T) {
	s := newScenario(t)
	ctx := context.Background()

	const universalPwd = "master-secret"
	if err := repo.SetUniversalProxyPassword(ctx, s.db.DB, s.mk, universalPwd); err != nil {
		t.Fatal(err)
	}
	// Re-hydrate so the display-name index + universal password take effect.
	if err := s.core.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}

	// The seeded upstream's display_name is "US-Premium-01"; there is NO local
	// user with that name. Auth succeeds purely via display-name + universal.
	conn := dialAndConnect(t, s.proxyAddr, s.echo.Addr(), "US-Premium-01", universalPwd)
	defer conn.Close()

	const payload = "ping-universal"
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("echo mismatch: got %q want %q", buf, payload)
	}

	// The upstream must have seen the UPSTREAM's credentials, proving the
	// display-name route resolved to the right proxy and rewrote auth.
	reqs := s.upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("mock upstream saw %d requests, want 1", len(reqs))
	}
	if reqs[0].Username != scenUpstreamUser || reqs[0].Password != scenUpstreamPwd {
		t.Errorf("upstream creds = %q:%q, want %q:%q",
			reqs[0].Username, reqs[0].Password, scenUpstreamUser, scenUpstreamPwd)
	}
}

// TestUniversalPassword_HTTPWrongPasswordRejected confirms that a correct
// display name with the WRONG password (and no matching local user) is
// rejected with 407 rather than silently routing.
func TestUniversalPassword_HTTPWrongPasswordRejected(t *testing.T) {
	s := newScenario(t)
	ctx := context.Background()
	if err := repo.SetUniversalProxyPassword(ctx, s.db.DB, s.mk, "master-secret"); err != nil {
		t.Fatal(err)
	}
	if err := s.core.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}

	conn, err := net.Dial("tcp", s.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	authHdr := "Basic " + base64.StdEncoding.EncodeToString([]byte("US-Premium-01:wrong"))
	if _, err := fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		s.echo.Addr(), s.echo.Addr(), authHdr,
	); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d want 407", resp.StatusCode)
	}
}

func TestAC2_HTTPMissingAuthReturns407(t *testing.T) {
	s := newScenario(t)
	conn, err := net.Dial("tcp", s.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn,
		fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", s.echo.Addr(), s.echo.Addr()),
	); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d want 407", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Proxy-Authenticate"), "Basic") {
		t.Errorf("Proxy-Authenticate header = %q want Basic", resp.Header.Get("Proxy-Authenticate"))
	}
}

func TestAC2_HTTPBadPasswordReturns407(t *testing.T) {
	s := newScenario(t)
	conn, err := net.Dial("tcp", s.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	authHdr := "Basic " + base64.StdEncoding.EncodeToString([]byte(scenLocalUser+":wrong"))
	if _, err := fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		s.echo.Addr(), s.echo.Addr(), authHdr,
	); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Fatalf("status = %d want 407", resp.StatusCode)
	}
}
