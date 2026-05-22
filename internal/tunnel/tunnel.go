// Package tunnel resolves a (username, password) pair to an upstream proxy,
// dials the upstream's CONNECT endpoint, and bridges the duplex stream.
//
// Phase 2 wires Acquire + DialHTTPUpstream + Bridge against a routing.Core
// that was hydrated at boot. Hot-switch teardown (Phase 4) re-uses the
// per-mapping CancelGroup carried in ResolvedUser.
package tunnel

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guofan/webshare-proxy/internal/model"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/routing"
)

// Sentinel errors returned by Acquire. HTTP listener maps these to 407/502;
// SOCKS5 maps to RFC1929 0x01 / SOCKS reply 0x01.
var (
	ErrUnknownUser    = errors.New("tunnel: unknown user")
	ErrBadPassword    = errors.New("tunnel: bad password")
	ErrBrokenMapping  = errors.New("tunnel: mapping broken")
	ErrUpstreamStale  = errors.New("tunnel: upstream stale (alive=false)")
	ErrUpstreamDial   = errors.New("tunnel: upstream dial failed")
	ErrUpstreamAuth   = errors.New("tunnel: upstream rejected our credentials")
)

// Manager is the tunnel facade the listeners call into.
type Manager struct {
	core        *routing.Core
	dialer      *net.Dialer
	dialTimeout time.Duration
}

// New wires a Manager.
func New(core *routing.Core) *Manager {
	return &Manager{
		core:        core,
		dialer:      &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second},
		dialTimeout: 10 * time.Second,
	}
}

// Acquire resolves (username, password) against the current routing
// snapshot. Returns the upstream proxy, its decrypted password, and the
// per-mapping CancelGroup the caller derives its connection context from.
//
// Password comparison is constant-time. The snapshot is captured once at
// the start of the function via routing.Core.Snapshot(); no lock is held
// during the rest of the call.
func (m *Manager) Acquire(_ context.Context, username, password string) (*model.UpstreamProxy, string, *routing.CancelGroup, error) {
	state := m.core.Snapshot()
	if state == nil {
		return nil, "", nil, errors.New("tunnel: routing state not hydrated")
	}
	u, ok := state.Users[username]
	if !ok {
		return nil, "", nil, ErrUnknownUser
	}
	// Constant-time compare prevents leaking password length via timing.
	if subtle.ConstantTimeCompare([]byte(u.PasswordPlain), []byte(password)) != 1 {
		return nil, "", nil, ErrBadPassword
	}
	if u.Broken || u.Upstream == nil {
		return nil, "", nil, ErrBrokenMapping
	}
	if !u.Upstream.Alive {
		return nil, "", nil, ErrUpstreamStale
	}
	return u.Upstream, u.UpstreamPwd, u.CancelGroup, nil
}

// DialUpstream opens a connection to the upstream proxy and tunnels a
// CONNECT to target through it, dispatching on upstream.Protocol. The
// returned net.Conn is ready for byte-stream bridging.
//
// target is passed verbatim — never resolved locally. This is what keeps
// the geo-routing promise across both webshare and manual upstreams.
func (m *Manager) DialUpstream(ctx context.Context, upstream *model.UpstreamProxy, upstreamPassword, target string) (net.Conn, error) {
	switch upstream.Protocol {
	case repo.ProtocolHTTPS:
		return m.dialViaHTTPSConnect(ctx, upstream, upstreamPassword, target)
	case repo.ProtocolSOCKS5:
		return m.dialViaSOCKS5(ctx, upstream, upstreamPassword, target)
	case repo.ProtocolHTTP, "":
		return m.dialViaHTTPConnect(ctx, upstream, upstreamPassword, target)
	default:
		return nil, fmt.Errorf("%w: unsupported upstream protocol %q", ErrUpstreamDial, upstream.Protocol)
	}
}

// DialHTTPUpstream is preserved for callers and tests that predate the
// per-protocol dispatch. It is a thin wrapper that always takes the
// http-CONNECT path, matching the historical behavior of every upstream
// before manual proxies existed.
func (m *Manager) DialHTTPUpstream(ctx context.Context, upstream *model.UpstreamProxy, upstreamPassword, target string) (net.Conn, error) {
	return m.dialViaHTTPConnect(ctx, upstream, upstreamPassword, target)
}

// dialViaHTTPConnect opens a TCP connection to upstream.Host:upstream.Port,
// sends an HTTP CONNECT for target, and awaits the 200 response.
func (m *Manager) dialViaHTTPConnect(ctx context.Context, upstream *model.UpstreamProxy, upstreamPassword, target string) (net.Conn, error) {
	addr := net.JoinHostPort(upstream.Host, strconv.Itoa(upstream.Port))
	conn, err := m.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %v", ErrUpstreamDial, addr, err)
	}
	return m.finishHTTPConnect(ctx, conn, upstream, upstreamPassword, target)
}

// dialViaHTTPSConnect TLS-wraps the TCP dial (so the upstream sees a TLS
// proxy connection, e.g. Cloudflare-fronted proxies) then performs an
// identical HTTP CONNECT handshake.
func (m *Manager) dialViaHTTPSConnect(ctx context.Context, upstream *model.UpstreamProxy, upstreamPassword, target string) (net.Conn, error) {
	addr := net.JoinHostPort(upstream.Host, strconv.Itoa(upstream.Port))
	rawConn, err := m.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %v", ErrUpstreamDial, addr, err)
	}
	// SECURITY: do NOT set InsecureSkipVerify. ServerName drives both SNI
	// and cert-chain validation; an upstream that can't present a cert
	// matching upstream.Host should fail the dial closed.
	tlsConn := tls.Client(rawConn, &tls.Config{ServerName: upstream.Host})
	if dl, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(dl)
	} else {
		_ = rawConn.SetDeadline(time.Now().Add(m.dialTimeout))
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		// Close via tlsConn so the wrapper's state machine is torn down
		// cleanly; tlsConn.Close also closes the underlying rawConn.
		_ = tlsConn.Close()
		return nil, fmt.Errorf("%w: tls handshake %s: %v", ErrUpstreamDial, addr, err)
	}
	return m.finishHTTPConnect(ctx, tlsConn, upstream, upstreamPassword, target)
}

// finishHTTPConnect drives the CONNECT request/response over an already-
// established net.Conn (plain TCP or TLS) and returns the conn ready for
// duplex copying.
func (m *Manager) finishHTTPConnect(ctx context.Context, conn net.Conn, upstream *model.UpstreamProxy, upstreamPassword, target string) (net.Conn, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(m.dialTimeout))
	}

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: http.Header{},
	}
	// Only attach Proxy-Authorization if the upstream has a username.
	// A bare password (no user) is invalid Basic auth — the upstream
	// sees ":pwd" and treats it as a missing user. The API layer rejects
	// that shape; this matches the policy as defense in depth.
	if upstream.Username != "" {
		auth := upstream.Username + ":" + upstreamPassword
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: write CONNECT: %v", ErrUpstreamDial, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: read CONNECT response: %v", ErrUpstreamDial, err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusProxyAuthRequired {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: upstream returned 407", ErrUpstreamAuth)
	}
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: upstream returned status %d", ErrUpstreamDial, resp.StatusCode)
	}
	// Clear the deadline now that the handshake is done; Bridge will set
	// SetDeadline(now) on cancel to force a teardown within ~1 TCP RTT.
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// dialViaSOCKS5 implements the RFC 1928 client side: METHOD negotiate
// (offer USERNAME/PASSWORD = 0x02 and NO-AUTH = 0x00), optional RFC 1929
// sub-negotiate, then CONNECT with ATYP=domain so the upstream resolves.
// target must be "host:port" (verbatim from the listener).
func (m *Manager) dialViaSOCKS5(ctx context.Context, upstream *model.UpstreamProxy, upstreamPassword, target string) (net.Conn, error) {
	addr := net.JoinHostPort(upstream.Host, strconv.Itoa(upstream.Port))
	conn, err := m.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %v", ErrUpstreamDial, addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(m.dialTimeout))
	}

	// Same rule as HTTP CONNECT: a SOCKS5 USERNAME/PASSWORD sub-negotiate
	// requires a non-empty username; an empty ULEN may be rejected by
	// real-world servers as malformed.
	hasAuth := upstream.Username != ""

	// Method negotiate: VER=5, NMETHODS=1, METHODS=[0x02 or 0x00].
	method := byte(0x00) // NO AUTH
	if hasAuth {
		method = byte(0x02) // USERNAME/PASSWORD
	}
	if _, err := conn.Write([]byte{0x05, 0x01, method}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 greet: %v", ErrUpstreamDial, err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 greet read: %v", ErrUpstreamDial, err)
	}
	if resp[0] != 0x05 {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 bad version %#x", ErrUpstreamDial, resp[0])
	}
	if resp[1] == 0xFF {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 server rejected offered auth methods", ErrUpstreamAuth)
	}
	if resp[1] != method {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 method mismatch (got %#x want %#x)", ErrUpstreamDial, resp[1], method)
	}

	if hasAuth {
		// RFC 1929: VER=1, ULEN, UNAME, PLEN, PASSWD. Validate lengths
		// BEFORE we use them in the cap arithmetic so the reader sees
		// the bound check first.
		if len(upstream.Username) > 255 || len(upstreamPassword) > 255 {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: socks5 creds too long", ErrUpstreamDial)
		}
		buf := make([]byte, 0, 3+len(upstream.Username)+len(upstreamPassword))
		buf = append(buf, 0x01, byte(len(upstream.Username)))
		buf = append(buf, []byte(upstream.Username)...)
		buf = append(buf, byte(len(upstreamPassword)))
		buf = append(buf, []byte(upstreamPassword)...)
		if _, err := conn.Write(buf); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: socks5 auth write: %v", ErrUpstreamDial, err)
		}
		ar := make([]byte, 2)
		if _, err := io.ReadFull(conn, ar); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: socks5 auth read: %v", ErrUpstreamDial, err)
		}
		if ar[1] != 0x00 {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: socks5 server denied credentials", ErrUpstreamAuth)
		}
	}

	// CONNECT request: VER=5, CMD=1, RSV=0, ATYP=domain, LEN, NAME, PORT.
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 target %q: %v", ErrUpstreamDial, target, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 bad port %q", ErrUpstreamDial, portStr)
	}
	// Validate host length BEFORE we use it for the cap arithmetic.
	if len(host) > 255 {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 host too long", ErrUpstreamDial)
	}
	req := make([]byte, 0, 7+len(host))
	req = append(req, 0x05, 0x01, 0x00, 0x03, byte(len(host)))
	req = append(req, []byte(host)...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	req = append(req, portBytes[:]...)
	if _, err := conn.Write(req); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 connect write: %v", ErrUpstreamDial, err)
	}

	// Reply: VER, REP, RSV, ATYP, BND.ADDR, BND.PORT — variable length.
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 reply head: %v", ErrUpstreamDial, err)
	}
	if head[0] != 0x05 {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 reply ver %#x", ErrUpstreamDial, head[0])
	}
	if head[1] != 0x00 {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 connect failed (rep=%#x)", ErrUpstreamDial, head[1])
	}
	// Consume BND.ADDR per ATYP, then 2-byte port — we don't use these
	// values but must drain them so the bridge starts at a clean offset.
	switch head[3] {
	case 0x01:
		if _, err := io.ReadFull(conn, make([]byte, 4+2)); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: socks5 reply ipv4: %v", ErrUpstreamDial, err)
		}
	case 0x04:
		if _, err := io.ReadFull(conn, make([]byte, 16+2)); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: socks5 reply ipv6: %v", ErrUpstreamDial, err)
		}
	case 0x03:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(conn, lb); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: socks5 reply domain len: %v", ErrUpstreamDial, err)
		}
		if _, err := io.ReadFull(conn, make([]byte, int(lb[0])+2)); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%w: socks5 reply domain: %v", ErrUpstreamDial, err)
		}
	default:
		_ = conn.Close()
		return nil, fmt.Errorf("%w: socks5 reply atyp %#x", ErrUpstreamDial, head[3])
	}

	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// Bridge copies bytes in both directions between clientConn and
// upstreamConn until either side closes or ctx is cancelled. The two
// canonical end-of-stream paths are:
//
//   - External cancellation (Phase 4 hot-switch): ctx.Done() fires, the
//     watcher sets a past deadline on BOTH sockets, both io.Copy goroutines
//     unblock within ~1 TCP RTT.
//   - One direction completes first (the usual "client closed its half"
//     case): the completing goroutine cancels a derived bridge context,
//     which trips the same deadline path on the OTHER socket so its
//     blocked Read returns immediately instead of hanging forever.
//
// Both sockets are Close()'d via defer regardless of how Bridge returns,
// so FDs do not leak. The caller is expected to register the connection
// in a registry (Phase 4) before calling and to deregister after return.
func Bridge(ctx context.Context, clientConn, upstreamConn net.Conn) (bytesIn, bytesOut int64, err error) {
	defer clientConn.Close()
	defer upstreamConn.Close()

	// bridgeCtx is cancelled either by the caller's ctx OR by whichever
	// io.Copy direction returns first. The watcher goroutine reacts to
	// bridgeCtx.Done() by setting a past deadline on both sockets, which
	// is the only reliable way to interrupt a blocked TCP Read.
	bridgeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-bridgeCtx.Done():
			deadline := time.Now()
			_ = clientConn.SetDeadline(deadline)
			_ = upstreamConn.SetDeadline(deadline)
		case <-done:
		}
	}()

	var (
		in  atomic.Int64
		out atomic.Int64
		wg  sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel() // first direction to finish wakes the other
		n, _ := io.Copy(upstreamConn, clientConn)
		in.Store(n)
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		n, _ := io.Copy(clientConn, upstreamConn)
		out.Store(n)
	}()
	wg.Wait()
	return in.Load(), out.Load(), ctx.Err()
}
