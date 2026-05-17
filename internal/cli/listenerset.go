package cli

import (
	"context"
	"fmt"
	"sync"

	"github.com/guofan/webshare-proxy/internal/auth"
	"github.com/guofan/webshare-proxy/internal/listener"
	"github.com/guofan/webshare-proxy/internal/registry"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// listenerSet is the HTTP+SOCKS5 listener lifecycle manager. It supports:
//   - explicit Start / Stop (req #1 user-initiated toggle)
//   - bind-then-swap Reconfigure so a port-change failure leaves the OLD
//     listener intact (req #3 "新端口被占用 → 旧端口不释放")
//   - idempotent Stop that releases sockets fully (req #4 "不要不释放未使用端口")
//
// All transitions take the same mutex; concurrent Start/Stop/Reconfigure
// calls serialize cleanly.
type listenerSet struct {
	mu       sync.Mutex
	mgr      *tunnel.Manager
	reg      *registry.ConnectionRegistry
	denyList *auth.DenyList

	// running is true iff httpProxy and socksProxy are bound and have a
	// Serve goroutine alive.
	running bool

	httpProxy  *listener.HTTPProxy
	socksProxy *listener.SOCKS5Proxy

	httpDone  chan struct{}
	socksDone chan struct{}
}

// Start binds both listeners on the supplied addresses and spawns their
// Serve goroutines. Idempotent on the (running && same addrs) case. If
// either bind fails, neither port is left bound.
func (l *listenerSet) Start(ctx context.Context, httpBind string, httpPort int, socksBind string, socksPort int) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.running {
		// Already running on these addrs → no-op. On different addrs,
		// callers should call Reconfigure instead (which handles the
		// bind-then-swap path with rollback).
		if l.httpProxy.Addr() == fmt.Sprintf("%s:%d", httpBind, httpPort) &&
			l.socksProxy.Addr() == fmt.Sprintf("%s:%d", socksBind, socksPort) {
			return nil
		}
		return fmt.Errorf("listenerSet: already running on different addresses; use Reconfigure")
	}

	hp, sp, err := bindPair(l.mgr, l.reg, l.denyList, httpBind, httpPort, socksBind, socksPort)
	if err != nil {
		return err
	}
	l.httpProxy = hp
	l.socksProxy = sp
	l.spawnServeLocked(ctx)
	l.running = true
	return nil
}

// Stop closes the listeners (releasing the kernel sockets) and waits for
// their Serve goroutines to exit. Safe to call when already stopped.
func (l *listenerSet) Stop() {
	l.mu.Lock()
	hp := l.httpProxy
	sp := l.socksProxy
	httpDone := l.httpDone
	socksDone := l.socksDone
	l.running = false
	l.httpProxy = nil
	l.socksProxy = nil
	l.httpDone = nil
	l.socksDone = nil
	l.mu.Unlock()

	if hp != nil {
		_ = hp.Close()
	}
	if sp != nil {
		_ = sp.Close()
	}
	if httpDone != nil {
		<-httpDone
	}
	if socksDone != nil {
		<-socksDone
	}
}

// Reconfigure attempts to swap the live listeners to the new addresses. It
// binds the NEW pair first, then closes the old. If the new bind fails the
// OLD listeners stay running and the error is returned — the kernel never
// sees the old ports released, so a port-change to an occupied port is a
// safe no-op from the user's perspective (req #3).
//
// When the set is currently stopped this is a no-op + nil — settings are
// persisted by the caller; the next Start uses them.
func (l *listenerSet) Reconfigure(ctx context.Context, httpBind string, httpPort int, socksBind string, socksPort int) error {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	// No-change short-circuit: avoid a needless rebind cycle that would
	// drop any in-flight connections.
	if l.httpProxy.Addr() == fmt.Sprintf("%s:%d", httpBind, httpPort) &&
		l.socksProxy.Addr() == fmt.Sprintf("%s:%d", socksBind, socksPort) {
		l.mu.Unlock()
		return nil
	}
	oldHTTP := l.httpProxy
	oldSocks := l.socksProxy
	oldHTTPDone := l.httpDone
	oldSocksDone := l.socksDone
	l.mu.Unlock()

	// Bind new BEFORE closing old. bindPair takes no instance locks so we
	// don't deadlock; on failure neither new port is left bound.
	hp, sp, err := bindPair(l.mgr, l.reg, l.denyList, httpBind, httpPort, socksBind, socksPort)
	if err != nil {
		return err
	}

	// Bind succeeded — swap in new, close old. Lock around the pointer
	// swap so Status()/HTTPAddr() callers never see a half-state.
	l.mu.Lock()
	l.httpProxy = hp
	l.socksProxy = sp
	l.spawnServeLocked(ctx)
	l.mu.Unlock()

	_ = oldHTTP.Close()
	_ = oldSocks.Close()
	if oldHTTPDone != nil {
		<-oldHTTPDone
	}
	if oldSocksDone != nil {
		<-oldSocksDone
	}
	return nil
}

// Status returns the current running flag and the bound addresses (empty
// strings when stopped). Snapshots under the mutex so all three values are
// consistent.
func (l *listenerSet) Status() (running bool, httpAddr, socksAddr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return false, "", ""
	}
	return true, l.httpProxy.Addr(), l.socksProxy.Addr()
}

// spawnServeLocked must be called with l.mu held. Replaces any stale
// httpDone/socksDone with fresh channels and starts the Serve loops.
func (l *listenerSet) spawnServeLocked(ctx context.Context) {
	hp := l.httpProxy
	sp := l.socksProxy
	httpDone := make(chan struct{})
	socksDone := make(chan struct{})
	l.httpDone = httpDone
	l.socksDone = socksDone
	go func() {
		defer close(httpDone)
		_ = hp.Serve(ctx)
	}()
	go func() {
		defer close(socksDone)
		_ = sp.Serve(ctx)
	}()
}

// bindPair constructs and Bind()s an HTTP+SOCKS5 listener pair. On any
// failure both new sockets are closed before the error is returned, so
// no port is leaked.
func bindPair(mgr *tunnel.Manager, reg *registry.ConnectionRegistry, denyList *auth.DenyList,
	httpBind string, httpPort int, socksBind string, socksPort int,
) (*listener.HTTPProxy, *listener.SOCKS5Proxy, error) {
	hp := listener.NewHTTPProxy(fmt.Sprintf("%s:%d", httpBind, httpPort), mgr, reg, denyList, nil)
	if err := hp.Bind(); err != nil {
		return nil, nil, fmt.Errorf("http listener bind %s:%d: %w", httpBind, httpPort, err)
	}
	sp := listener.NewSOCKS5Proxy(fmt.Sprintf("%s:%d", socksBind, socksPort), mgr, reg, denyList, nil)
	if err := sp.Bind(); err != nil {
		_ = hp.Close()
		return nil, nil, fmt.Errorf("socks5 listener bind %s:%d: %w", socksBind, socksPort, err)
	}
	return hp, sp, nil
}
