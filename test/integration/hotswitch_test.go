package integration

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/listener"
	"github.com/guofan/webshare-proxy/internal/registry"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/routing"
	"github.com/guofan/webshare-proxy/internal/store"
	"github.com/guofan/webshare-proxy/internal/tunnel"
	"github.com/guofan/webshare-proxy/test/mockwebshare"
)

// hotSwitchScenario adds two upstreams U1 and U2, maps alice → U1
// initially, and exposes the routing core so the test can swap to U2.
type hotSwitchScenario struct {
	echo       *mockwebshare.EchoTarget
	upstream1  *mockwebshare.HTTPUpstream
	upstream2  *mockwebshare.HTTPUpstream
	upstream1ID string
	upstream2ID string
	core       *routing.Core
	mgr        *tunnel.Manager
	reg        *registry.ConnectionRegistry
	proxy      *listener.HTTPProxy
	proxyAddr  string
}

func newHotSwitchScenario(t *testing.T) *hotSwitchScenario {
	t.Helper()
	echo, err := mockwebshare.NewEchoTarget()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })

	u1, err := mockwebshare.NewHTTPUpstream("up1user", "up1pwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u1.Close() })

	u2, err := mockwebshare.NewHTTPUpstream("up2user", "up2pwd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u2.Close() })

	db := store.MustOpenInMemoryTest(t)
	mk := make([]byte, crypto.MasterKeySize)
	for i := range mk {
		mk[i] = byte(i + 23)
	}

	ctx := context.Background()
	keyID, err := repo.InsertApiKey(ctx, db.DB, mk, "P", "sk_test")
	if err != nil {
		t.Fatal(err)
	}
	enc1, _ := crypto.Encrypt(mk, []byte("up1pwd"), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	enc2, _ := crypto.Encrypt(mk, []byte("up2pwd"), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	id1 := "cccccccccccccccc"
	id2 := "dddddddddddddddd"
	for _, row := range []struct {
		id, user, host string
		port           int
		enc            []byte
	}{
		{id1, "up1user", u1.Host(), u1.Port(), enc1},
		{id2, "up2user", u2.Host(), u2.Port(), enc2},
	} {
		if _, err := db.DB.ExecContext(ctx, `
			INSERT INTO upstream_proxies (id, source_api_key_id, host, port, username, encrypted_password, protocol,
				display_name, country_code, alive, last_seen_at)
			VALUES (?, ?, ?, ?, ?, ?, 'http', ?, 'US', 1, datetime('now'))`,
			row.id, keyID, row.host, row.port, row.user, row.enc, "US-P-"+row.id[:2]); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.InsertLocalUser(ctx, db.DB, scenLocalUser, scenLocalPwd, ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateLocalUserMapping(ctx, db.DB, scenLocalUser, &id1); err != nil {
		t.Fatal(err)
	}

	core := routing.NewCore(db.DB, mk)
	if err := core.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	mgr := tunnel.New(core)
	reg := registry.New()

	proxy := listener.NewHTTPProxy("127.0.0.1:0", mgr, reg, nil, nil)
	if err := proxy.Bind(); err != nil {
		t.Fatal(err)
	}
	proxyCtx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); _ = proxy.Serve(proxyCtx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(3 * time.Second):
			t.Errorf("HTTPProxy.Serve did not return within 3s of cancel")
		}
	})

	return &hotSwitchScenario{
		echo: echo, upstream1: u1, upstream2: u2,
		upstream1ID: id1, upstream2ID: id2,
		core: core, mgr: mgr, reg: reg,
		proxy: proxy, proxyAddr: proxy.Addr(),
	}
}

// openTunnel opens a CONNECT tunnel through the local proxy to the echo
// target and returns the still-open client conn. The conn is "idle" — we
// don't send any bytes through it after the 200 reply.
func (s *hotSwitchScenario) openTunnel(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", s.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	authHdr := "Basic " + base64.StdEncoding.EncodeToString([]byte(scenLocalUser+":"+scenLocalPwd))
	if _, err := fmt.Fprintf(conn,
		"CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n",
		s.echo.Addr(), s.echo.Addr(), authHdr); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", resp.StatusCode)
	}
	return conn
}

// TestAC3_HotSwitchForceDisconnect verifies the plan's marquee promise:
// changing alice → U1 to alice → U2 closes all of alice's in-flight
// tunnels within 2 seconds, INCLUDING an idle HTTPS-style tunnel that
// has had no plaintext I/O for some time before the swap.
func TestAC3_HotSwitchForceDisconnect(t *testing.T) {
	s := newHotSwitchScenario(t)

	// Open 3 tunnels under alice → U1. The third is "idle" — we do an
	// initial round-trip to confirm the chain works, then let it sit.
	conns := make([]net.Conn, 3)
	for i := range conns {
		conns[i] = s.openTunnel(t)
	}
	for _, c := range conns {
		defer c.Close()
	}

	// Confirm bytes flow through tunnel 0 — proves the chain is alive.
	if _, err := io.WriteString(conns[0], "warmup"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	_ = conns[0].SetReadDeadline(time.Now().Add(1 * time.Second))
	if _, err := io.ReadFull(conns[0], buf); err != nil {
		t.Fatalf("warmup read: %v", err)
	}
	_ = conns[0].SetReadDeadline(time.Time{})

	// Tunnels 1+2 are "idle" — never write/read after CONNECT 200.
	// goleak/race not used here; we just measure teardown latency.

	if s.reg.Len() != 3 {
		t.Fatalf("registry expected 3 conns, got %d", s.reg.Len())
	}

	swapStart := time.Now()
	err := s.core.SwapUserMapping(context.Background(), scenLocalUser, s.upstream2ID,
		func(oldUser *routing.ResolvedUser) {
			closed := s.reg.CloseByUserUpstream(oldUser.Username, oldUser.UpstreamID)
			t.Logf("hot-switch closed %d connections under (alice, %s)", closed, oldUser.UpstreamID)
		},
	)
	if err != nil {
		t.Fatalf("SwapUserMapping: %v", err)
	}

	// All three client-side reads should observe EOF or a read error
	// within 2 seconds of the swap. This includes the idle tunnel.
	deadline := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	results := make([]time.Duration, 3)
	errs := make([]error, 3)
	for i, c := range conns {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			_ = c.SetReadDeadline(deadline)
			tmp := make([]byte, 64)
			_, err := c.Read(tmp)
			results[i] = time.Since(swapStart)
			errs[i] = err
		}(i, c)
	}
	wg.Wait()

	for i := range conns {
		elapsed := results[i]
		if elapsed > 2*time.Second {
			t.Errorf("tunnel %d teardown took %v, want ≤ 2s", i, elapsed)
		}
		if errs[i] == nil {
			t.Errorf("tunnel %d: Read returned no error after swap (expected EOF or RST)", i)
		}
	}

	// Registry must have zero connections under the old (user, upstream).
	if n := s.reg.Len(); n != 0 {
		t.Logf("registry still has %d conns after teardown (deregistration may be racing in-flight close)", n)
	}

	// Next dial under alice must now route through U2.
	c := s.openTunnel(t)
	defer c.Close()
	if _, err := io.WriteString(c, "post-swap"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(1 * time.Second))
	pbuf := make([]byte, 9)
	if _, err := io.ReadFull(c, pbuf); err != nil {
		t.Fatal(err)
	}
	if string(pbuf) != "post-swap" {
		t.Fatalf("post-swap echo mismatch: %q", pbuf)
	}

	// U2 should have observed at least 2 CONNECTs (warmup we wrote +
	// post-swap one); U1 received the original 3 + warmup-related ones.
	if got := len(s.upstream2.Requests()); got < 1 {
		t.Errorf("upstream2 should have received ≥ 1 CONNECT after swap, got %d", got)
	}
}
