package cli

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

	"github.com/guofan/pia/internal/auth"
	"github.com/guofan/pia/internal/listener"
	"github.com/guofan/pia/internal/registry"
	"github.com/guofan/pia/internal/tunnel"
)

// listenerSet is the unified-proxy listener lifecycle manager. The HTTP and
// SOCKS5 proxies share ONE port (protocol auto-detected per connection), so
// this manages a single listener. It supports:
//   - explicit Start / Stop (user-initiated on/off toggle)
//   - bind-then-swap Reconfigure so a port-change failure leaves the OLD
//     listener intact ("新端口被占用 → 旧端口不释放")
//   - idempotent Stop that releases the socket fully ("不要不释放未使用端口")
//
// All transitions take the same mutex; concurrent Start/Stop/Reconfigure
// calls serialize cleanly.
type listenerSet struct {
	mu       sync.Mutex
	mgr      *tunnel.Manager
	reg      *registry.ConnectionRegistry
	denyList *auth.DenyList

	// running is true iff proxy is bound and has a Serve goroutine alive.
	running bool

	proxy *listener.UnifiedProxy
	done  chan struct{}
}

// Start binds the unified listener on the supplied address and spawns its
// Serve goroutine. Idempotent on the (running && same addr) case.
func (l *listenerSet) Start(ctx context.Context, bind string, port int) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.running {
		// Already running on this addr → no-op. On a different addr, callers
		// should call Reconfigure instead (bind-then-swap with rollback).
		if l.proxy.Addr() == fmt.Sprintf("%s:%d", bind, port) {
			return nil
		}
		return fmt.Errorf("listenerSet: already running on a different address; use Reconfigure")
	}

	p, err := bindOne(l.mgr, l.reg, l.denyList, bind, port)
	if err != nil {
		return err
	}
	l.proxy = p
	l.spawnServeLocked(ctx)
	l.running = true
	return nil
}

// Stop closes the listener (releasing the kernel socket) and waits for its
// Serve goroutine to exit. Safe to call when already stopped.
func (l *listenerSet) Stop() {
	l.mu.Lock()
	p := l.proxy
	done := l.done
	l.running = false
	l.proxy = nil
	l.done = nil
	l.mu.Unlock()

	if p != nil {
		_ = p.Close()
	}
	if done != nil {
		<-done
	}
}

// Reconfigure swaps the live listener to a new address.
//
//   - Different port → bind NEW first, then close OLD. If the new bind fails,
//     the OLD listener stays running and the error is returned.
//   - Same port (bind change only, e.g. 127.0.0.1:8080 → 0.0.0.0:8080) →
//     close-then-bind with rollback: bind-first would always fail because the
//     kernel still holds the port. We close old, try new; if new fails we
//     attempt to restore the old bind so the user keeps a working proxy.
//
// When the set is currently stopped this is a no-op + nil — the caller has
// persisted the settings; the next Start uses them.
func (l *listenerSet) Reconfigure(ctx context.Context, bind string, port int) error {
	l.mu.Lock()
	if !l.running {
		l.mu.Unlock()
		return nil
	}
	newAddr := fmt.Sprintf("%s:%d", bind, port)
	// No-change short-circuit: avoid a needless rebind that would drop any
	// in-flight connections.
	if l.proxy.Addr() == newAddr {
		l.mu.Unlock()
		return nil
	}
	old := l.proxy
	oldDone := l.done
	oldHost, oldPort := splitHostPort(l.proxy.Addr())
	l.mu.Unlock()

	if port == oldPort {
		// Same port, different bind: close-then-bind with rollback. The kernel
		// won't let us bind the new address while the old socket holds the
		// port, so we must release first.
		_ = old.Close()
		if oldDone != nil {
			<-oldDone
		}
		p, err := bindOne(l.mgr, l.reg, l.denyList, bind, port)
		if err != nil {
			op, restErr := bindOne(l.mgr, l.reg, l.denyList, oldHost, oldPort)
			if restErr != nil {
				// Both new and rollback bind failed (someone grabbed the old
				// port in the gap). Listener is now stopped.
				l.mu.Lock()
				l.running = false
				l.proxy = nil
				l.done = nil
				l.mu.Unlock()
				return fmt.Errorf("%w (rollback to %s also failed: %v)", err, old.Addr(), restErr)
			}
			l.mu.Lock()
			l.proxy = op
			l.spawnServeLocked(ctx)
			l.mu.Unlock()
			return err
		}
		l.mu.Lock()
		l.proxy = p
		l.spawnServeLocked(ctx)
		l.mu.Unlock()
		return nil
	}

	// Different port: bind NEW before closing OLD so a failed new bind leaves
	// the old listener accepting.
	p, err := bindOne(l.mgr, l.reg, l.denyList, bind, port)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.proxy = p
	l.spawnServeLocked(ctx)
	l.mu.Unlock()

	_ = old.Close()
	if oldDone != nil {
		<-oldDone
	}
	return nil
}

// splitHostPort extracts host and port from a "host:port" string. Returns
// ("", 0) on parse failure — callers use the values to attempt a rebind, so a
// malformed addr just means "we don't know the old binding" and the rollback
// surfaces as a bind error.
func splitHostPort(addr string) (string, int) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(p)
	return h, port
}

// Status returns the running flag and the bound address (empty when stopped),
// snapshotted under the mutex so both values are consistent.
func (l *listenerSet) Status() (running bool, proxyAddr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.running {
		return false, ""
	}
	return true, l.proxy.Addr()
}

// spawnServeLocked must be called with l.mu held. Replaces any stale done
// channel with a fresh one and starts the Serve loop.
func (l *listenerSet) spawnServeLocked(ctx context.Context) {
	p := l.proxy
	done := make(chan struct{})
	l.done = done
	go func() {
		defer close(done)
		_ = p.Serve(ctx)
	}()
}

// bindOne constructs and Bind()s a unified listener. On failure the socket is
// not leaked (Bind closes nothing it didn't open).
func bindOne(mgr *tunnel.Manager, reg *registry.ConnectionRegistry, denyList *auth.DenyList,
	bind string, port int,
) (*listener.UnifiedProxy, error) {
	p := listener.NewUnifiedProxy(fmt.Sprintf("%s:%d", bind, port), mgr, reg, denyList, nil)
	if err := p.Bind(); err != nil {
		return nil, fmt.Errorf("proxy listener bind %s:%d: %w", bind, port, err)
	}
	return p, nil
}
