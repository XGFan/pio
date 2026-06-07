package integration

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/guofan/pia/internal/crypto"
	"github.com/guofan/pia/internal/listener"
	"github.com/guofan/pia/internal/repo"
	"github.com/guofan/pia/internal/routing"
	"github.com/guofan/pia/internal/store"
	"github.com/guofan/pia/internal/tunnel"
	"github.com/guofan/pia/test/mockwebshare"
)

// unifiedScenario wires the same plumbing as the HTTP/SOCKS5 scenarios but
// fronts it with a single UnifiedProxy that serves BOTH protocols on one port.
type unifiedScenario struct {
	echo      *mockwebshare.EchoTarget
	upstream  *mockwebshare.HTTPUpstream
	proxyAddr string
}

func newUnifiedScenario(t *testing.T) *unifiedScenario {
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
		mk[i] = byte(i + 23)
	}

	ctx := context.Background()
	keyID, err := repo.InsertApiKey(ctx, db.DB, mk, "Premium", "sk_test")
	if err != nil {
		t.Fatal(err)
	}
	encPwd, _ := crypto.Encrypt(mk, []byte(scenUpstreamPwd), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	upstreamID := "cccccccccccccccc"
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO upstream_proxies (id, source_api_key_id, host, port, username, encrypted_password, protocol,
			display_name, country_code, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, 'http', 'US-Premium-01', 'US', datetime('now'))`,
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

	proxy := listener.NewUnifiedProxy("127.0.0.1:0", mgr, nil, nil, nil)
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
			t.Errorf("UnifiedProxy.Serve did not return within 3s of cancel")
		}
	})

	return &unifiedScenario{echo: echo, upstream: up, proxyAddr: proxy.Addr()}
}

// TestUnified_BothProtocolsOnOnePort is the headline acceptance test: a single
// port serves an HTTP CONNECT client AND a SOCKS5 client, each round-tripping
// bytes through the same upstream to the echo target. Protocol is selected by
// the first byte the client sends (0x05 → SOCKS5, else HTTP).
func TestUnified_BothProtocolsOnOnePort(t *testing.T) {
	s := newUnifiedScenario(t)

	// --- HTTP proxy client on the unified port ---
	httpConn := dialAndConnect(t, s.proxyAddr, s.echo.Addr(), scenLocalUser, scenLocalPwd)
	defer httpConn.Close()
	roundTrip(t, httpConn, "ping-over-http")

	// --- SOCKS5 client on the SAME unified port ---
	socksConn := negotiateAndAuth(t, s.proxyAddr, scenLocalUser, scenLocalPwd)
	defer socksConn.Close()
	echoHost, echoPortStr, _ := net.SplitHostPort(s.echo.Addr())
	echoPort, _ := strconv.Atoi(echoPortStr)
	rep := sendRequest(t, socksConn, socks5CmdConnect, socks5ATypIPv4,
		net.ParseIP(echoHost).To4(), uint16(echoPort))
	if rep != 0x00 {
		t.Fatalf("SOCKS5 CONNECT reply = 0x%02x, want 0x00", rep)
	}
	roundTrip(t, socksConn, "ping-over-socks5")

	// Both clients reached the upstream through the one port.
	if got := len(s.upstream.Requests()); got != 2 {
		t.Fatalf("upstream saw %d requests, want 2 (one HTTP, one SOCKS5)", got)
	}
}

// TestUnified_UDPAssociateRejected confirms requirement 3: upstreams don't do
// UDP, so a SOCKS5 UDP ASSOCIATE over the unified port is refused with reply
// 0x07 (command not supported).
func TestUnified_UDPAssociateRejected(t *testing.T) {
	s := newUnifiedScenario(t)
	conn := negotiateAndAuth(t, s.proxyAddr, scenLocalUser, scenLocalPwd)
	defer conn.Close()

	rep := sendRequest(t, conn, socks5CmdUDP, socks5ATypIPv4, []byte{127, 0, 0, 1}, 80)
	if rep != 0x07 {
		t.Errorf("UDP ASSOCIATE over unified port = 0x%02x, want 0x07", rep)
	}
}

// TestUnified_HTTPBadPasswordRejected confirms auth still works through the
// sniffing front: a bad password on the HTTP path returns 407, not a route.
func TestUnified_HTTPBadPasswordRejected(t *testing.T) {
	s := newUnifiedScenario(t)
	conn, err := net.Dial("tcp", s.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	authHdr := "Basic " + base64.StdEncoding.EncodeToString([]byte(scenLocalUser+":wrong"))
	if _, err := io.WriteString(conn,
		"CONNECT "+s.echo.Addr()+" HTTP/1.1\r\nHost: "+s.echo.Addr()+"\r\nProxy-Authorization: "+authHdr+"\r\n\r\n",
	); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 12)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if string(buf) != "HTTP/1.1 407" {
		t.Fatalf("status = %q, want HTTP/1.1 407", buf)
	}
}

// TestUnified_SOCKS4NotTreatedAsSocks5 confirms the sniffer only treats 0x05
// as SOCKS5: a SOCKS4 request (first byte 0x04) is routed to the HTTP handler,
// which can't parse it and closes the connection — it must never receive a
// SOCKS5-style reply (which would start with 0x05).
func TestUnified_SOCKS4NotTreatedAsSocks5(t *testing.T) {
	s := newUnifiedScenario(t)
	conn, err := net.Dial("tcp", s.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// A minimal SOCKS4 CONNECT to 127.0.0.1:80 (VN=4, CD=1, port, ip, null).
	if _, err := conn.Write([]byte{0x04, 0x01, 0x00, 0x50, 127, 0, 0, 1, 0x00}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	// The HTTP handler rejects the unparseable request by closing (EOF). The
	// one thing that must NOT happen is a SOCKS5 reply (leading 0x05).
	if err == nil && n > 0 && buf[0] == socks5Ver {
		t.Fatalf("SOCKS4 first byte was handled as SOCKS5 (reply led with 0x05): %v", buf[:n])
	}
}

// roundTrip writes payload over an established tunnel and asserts the echo
// target returns the identical bytes.
func roundTrip(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
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
}
