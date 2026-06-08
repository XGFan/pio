package integration

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/guofan/pio/internal/crypto"
	"github.com/guofan/pio/internal/listener"
	"github.com/guofan/pio/internal/repo"
	"github.com/guofan/pio/internal/routing"
	"github.com/guofan/pio/internal/store"
	"github.com/guofan/pio/internal/tunnel"
	"github.com/guofan/pio/test/mockwebshare"
)

// socksScenario is the SOCKS5 equivalent of `scenario`. We could collapse
// the two into a parameterized helper, but at two listeners that's mostly
// noise — duplicating ~30 lines keeps each variant easy to read.
type socksScenario struct {
	t         *testing.T
	echo      *mockwebshare.EchoTarget
	upstream  *mockwebshare.HTTPUpstream
	db        *store.DBHandle
	core      *routing.Core
	mgr       *tunnel.Manager
	proxy     *listener.SOCKS5Proxy
	proxyAddr string
}

func newSocksScenario(t *testing.T) *socksScenario {
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
		mk[i] = byte(i + 19)
	}

	ctx := context.Background()
	keyID, err := repo.InsertApiKey(ctx, db.DB, mk, "Premium", "sk_test")
	if err != nil {
		t.Fatal(err)
	}
	encPwd, _ := crypto.Encrypt(mk, []byte(scenUpstreamPwd), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	upstreamID := "bbbbbbbbbbbbbbbb"
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

	proxy := listener.NewSOCKS5Proxy("127.0.0.1:0", mgr, nil, nil, nil)
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
			t.Errorf("SOCKS5Proxy.Serve did not return within 3s of cancel")
		}
	})

	return &socksScenario{
		t: t, echo: echo, upstream: up, db: db, core: core, mgr: mgr,
		proxy: proxy, proxyAddr: proxy.Addr(),
	}
}

// socks5 framing helpers --------------------------------------------------

const (
	socks5Ver       = 0x05
	socks5MethodUP  = 0x02
	socks5CmdConnect = 0x01
	socks5CmdBind    = 0x02
	socks5CmdUDP     = 0x03
	socks5ATypIPv4   = 0x01
	socks5ATypDomain = 0x03
	socks5ATypIPv6   = 0x04
)

// negotiateAndAuth opens a SOCKS5 conn, completes method negotiation and
// RFC 1929 auth with (user, pass), and returns the still-open conn at the
// point where the client is ready to send the request frame.
func negotiateAndAuth(t *testing.T, addr, user, pass string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	// Hello: [VER, NMETHODS=1, METHOD=UP]
	if _, err := conn.Write([]byte{socks5Ver, 1, socks5MethodUP}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatal(err)
	}
	if resp[0] != socks5Ver || resp[1] != socks5MethodUP {
		_ = conn.Close()
		t.Fatalf("hello reply = %v want [5,2]", resp)
	}
	// RFC 1929: [VER=1, ULEN, UNAME, PLEN, PASSWD]
	pkt := []byte{0x01, byte(len(user))}
	pkt = append(pkt, []byte(user)...)
	pkt = append(pkt, byte(len(pass)))
	pkt = append(pkt, []byte(pass)...)
	if _, err := conn.Write(pkt); err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 2)
	if _, err := io.ReadFull(conn, auth); err != nil {
		t.Fatal(err)
	}
	if auth[0] != 0x01 || auth[1] != 0x00 {
		_ = conn.Close()
		t.Fatalf("auth reply = %v want [1,0]", auth)
	}
	return conn
}

// sendRequest writes a SOCKS5 request frame and returns the reply byte.
// On a 0x00 success the caller can read from conn afterwards.
func sendRequest(t *testing.T, conn net.Conn, cmd, atyp byte, addrBytes []byte, port uint16) byte {
	t.Helper()
	pkt := []byte{socks5Ver, cmd, 0x00, atyp}
	pkt = append(pkt, addrBytes...)
	portBuf := make([]byte, 2)
	binary.BigEndian.PutUint16(portBuf, port)
	pkt = append(pkt, portBuf...)
	if _, err := conn.Write(pkt); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 10) // VER+REP+RSV+ATYP+IPv4(4)+PORT(2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if reply[0] != socks5Ver {
		t.Errorf("reply ver = 0x%02x want 0x05", reply[0])
	}
	return reply[1]
}

// sendBadRequest is like sendRequest but does not try to read a 10-byte
// reply; useful when the server returns < 10 bytes on early failure.
func sendBadRequestExpectShort(t *testing.T, conn net.Conn, raw []byte) byte {
	t.Helper()
	if _, err := conn.Write(raw); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, 2)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn, hdr); err != nil {
		if errors.Is(err, io.EOF) {
			return 0xff // server closed
		}
		t.Fatalf("read short reply: %v", err)
	}
	return hdr[1]
}

// Tests -------------------------------------------------------------------

func TestAC5_SOCKS5RoutingByUsername(t *testing.T) {
	s := newSocksScenario(t)
	conn := negotiateAndAuth(t, s.proxyAddr, scenLocalUser, scenLocalPwd)
	defer conn.Close()

	echoHost, echoPortStr, _ := net.SplitHostPort(s.echo.Addr())
	echoPort, _ := strconv.Atoi(echoPortStr)
	rep := sendRequest(t, conn, socks5CmdConnect, socks5ATypIPv4,
		net.ParseIP(echoHost).To4(), uint16(echoPort))
	if rep != 0x00 {
		t.Fatalf("CONNECT reply = 0x%02x, want success (0x00)", rep)
	}

	const payload = "socks-echo-payload"
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("echo mismatch: %q vs %q", buf, payload)
	}

	reqs := s.upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d want 1", len(reqs))
	}
	if reqs[0].Username != scenUpstreamUser {
		t.Errorf("upstream username = %q want %q", reqs[0].Username, scenUpstreamUser)
	}
}

func TestAC6_SOCKS5_DomainATYPForwardedVerbatim(t *testing.T) {
	s := newSocksScenario(t)
	conn := negotiateAndAuth(t, s.proxyAddr, scenLocalUser, scenLocalPwd)
	defer conn.Close()

	// Use a domain that won't resolve to anything useful — the mock
	// upstream will fail its own dial, but we only care that the
	// recorded target is "echo.invalid:443" (string), proving the
	// listener did NOT resolve the name locally.
	const domain = "echo.invalid"
	addrBytes := []byte{byte(len(domain))}
	addrBytes = append(addrBytes, []byte(domain)...)
	rep := sendRequest(t, conn, socks5CmdConnect, socks5ATypDomain, addrBytes, 443)
	// The upstream's own dial will fail, so the listener reports general
	// failure to the SOCKS client — that's fine for this AC; the matrix
	// assertion is about what the upstream OBSERVED, not whether the
	// chain succeeded end-to-end.
	if rep != 0x01 {
		t.Logf("expected SOCKS5 reply 0x01 (upstream dial fail), got 0x%02x", rep)
	}

	// Give the mock upstream a moment to record the request.
	for range 20 {
		if len(s.upstream.Requests()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	reqs := s.upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d want 1; reqs=%+v", len(reqs), reqs)
	}
	want := domain + ":443"
	if reqs[0].Target != want {
		t.Errorf("upstream target = %q want %q (domain MUST forward verbatim, no DNS)", reqs[0].Target, want)
	}
}

func TestAC6_SOCKS5_IPv6ATYPForwardedAsBracketedHost(t *testing.T) {
	s := newSocksScenario(t)
	conn := negotiateAndAuth(t, s.proxyAddr, scenLocalUser, scenLocalPwd)
	defer conn.Close()

	v6 := net.ParseIP("2001:db8::1").To16()
	rep := sendRequest(t, conn, socks5CmdConnect, socks5ATypIPv6, v6, 8443)
	// Upstream dial will fail (no route to 2001:db8::1); reply 0x01.
	if rep != 0x01 {
		t.Logf("got reply 0x%02x", rep)
	}
	for range 20 {
		if len(s.upstream.Requests()) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	reqs := s.upstream.Requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d want 1", len(reqs))
	}
	if reqs[0].Target != "[2001:db8::1]:8443" {
		t.Errorf("upstream target = %q want [2001:db8::1]:8443 (RFC 7230 §5.4 bracket form)", reqs[0].Target)
	}
}

func TestAC6_SOCKS5_BINDRejectedWith07(t *testing.T) {
	s := newSocksScenario(t)
	conn := negotiateAndAuth(t, s.proxyAddr, scenLocalUser, scenLocalPwd)
	defer conn.Close()

	rep := sendRequest(t, conn, socks5CmdBind, socks5ATypIPv4, []byte{127, 0, 0, 1}, 80)
	if rep != 0x07 {
		t.Errorf("BIND reply = 0x%02x want 0x07 (command not supported)", rep)
	}
}

func TestAC6_SOCKS5_UDPAssociateRejectedWith07(t *testing.T) {
	s := newSocksScenario(t)
	conn := negotiateAndAuth(t, s.proxyAddr, scenLocalUser, scenLocalPwd)
	defer conn.Close()

	rep := sendRequest(t, conn, socks5CmdUDP, socks5ATypIPv4, []byte{127, 0, 0, 1}, 80)
	if rep != 0x07 {
		t.Errorf("UDP ASSOCIATE reply = 0x%02x want 0x07", rep)
	}
}

func TestAC6_SOCKS5_UnknownATYPRejectedWith08(t *testing.T) {
	s := newSocksScenario(t)
	conn := negotiateAndAuth(t, s.proxyAddr, scenLocalUser, scenLocalPwd)
	defer conn.Close()

	// Send a request with ATYP=0xFE (reserved/unknown). The server's
	// readRequest cannot consume the rest of the frame safely, so it
	// emits a single short reply (we read its REP byte directly).
	rep := sendBadRequestExpectShort(t, conn, []byte{socks5Ver, socks5CmdConnect, 0x00, 0xfe, 0x00, 0x00})
	// We accept either 0x08 (correct per matrix) OR 0xff if server closed
	// before sending — but per US-011 the spec is 0x08.
	if rep != 0x08 {
		t.Errorf("unknown-ATYP reply = 0x%02x want 0x08 (address type not supported)", rep)
	}
}

func TestAC5_SOCKS5_BadPasswordFails(t *testing.T) {
	s := newSocksScenario(t)
	conn, err := net.Dial("tcp", s.proxyAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte{socks5Ver, 1, socks5MethodUP}); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatal(err)
	}
	// Send a wrong password.
	pkt := []byte{0x01, byte(len(scenLocalUser))}
	pkt = append(pkt, []byte(scenLocalUser)...)
	const badPwd = "definitely-wrong"
	pkt = append(pkt, byte(len(badPwd)))
	pkt = append(pkt, []byte(badPwd)...)
	if _, err := conn.Write(pkt); err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 2)
	if _, err := io.ReadFull(conn, auth); err != nil {
		t.Fatal(err)
	}
	if auth[1] == 0x00 {
		t.Error("bad password accepted by SOCKS5 listener")
	}
}
