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
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/guofan/webshare-proxy/internal/model"
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

// DialHTTPUpstream opens a TCP connection to upstream.Host:upstream.Port,
// sends an HTTP CONNECT for target, awaits the 200 response, and returns
// the established net.Conn ready for byte-stream bridging.
//
// target is passed verbatim — never resolved locally. This is what keeps
// the geo-routing promise.
func (m *Manager) DialHTTPUpstream(ctx context.Context, upstream *model.UpstreamProxy, upstreamPassword, target string) (net.Conn, error) {
	addr := net.JoinHostPort(upstream.Host, fmt.Sprintf("%d", upstream.Port))
	conn, err := m.dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("%w: dial %s: %v", ErrUpstreamDial, addr, err)
	}
	// Bound the CONNECT round-trip so a hung upstream doesn't pin our goroutine.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(m.dialTimeout))
	}

	auth := upstream.Username + ":" + upstreamPassword
	authHeader := "Basic " + base64.StdEncoding.EncodeToString([]byte(auth))

	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: target},
		Host:   target,
		Header: http.Header{"Proxy-Authorization": {authHeader}},
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
	// Clear the deadline now that the handshake is done; bridge() will set
	// SetDeadline(now) on cancel to force a teardown within ~1 TCP RTT.
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
