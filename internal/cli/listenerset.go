package cli

import (
	"context"
	"fmt"
	"sync"

	"github.com/guofan/pio/internal/auth"
	"github.com/guofan/pio/internal/listener"
	"github.com/guofan/pio/internal/registry"
	"github.com/guofan/pio/internal/tunnel"
)

// listenerSet is the unified-proxy listener lifecycle manager. The HTTP and
// SOCKS5 proxies share ONE port (protocol auto-detected per connection), so
// this manages a single listener. It supports:
//   - explicit Start / Stop (user-initiated on/off toggle)
//   - idempotent Stop that releases the socket fully ("不要不释放未使用端口")
//
// Bind/port changes are applied while stopped and take effect on the next
// Start (settings are not editable while running). All transitions take the
// same mutex; concurrent Start/Stop calls serialize cleanly.
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
		// Already running on this addr → no-op. To change the address, stop the
		// proxy first; the next Start binds the new settings.
		if l.proxy.Addr() == fmt.Sprintf("%s:%d", bind, port) {
			return nil
		}
		return fmt.Errorf("listenerSet: already running on a different address; stop the proxy first")
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
