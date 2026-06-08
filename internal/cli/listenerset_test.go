package cli

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/guofan/pio/internal/auth"
	"github.com/guofan/pio/internal/registry"
	"github.com/guofan/pio/internal/routing"
	"github.com/guofan/pio/internal/tunnel"
)

// TestReconfigure_SameBindNoOp documents the no-op short-circuit and is the
// baseline for the same-port-bind-change regression test below.
func TestReconfigure_SameBindNoOp(t *testing.T) {
	t.Parallel()
	port := pickFreePort(t)

	l, cleanup := newRunningSet(t, "127.0.0.1", port)
	defer cleanup()

	if err := l.Reconfigure(context.Background(), "127.0.0.1", port); err != nil {
		t.Fatalf("Reconfigure no-op: %v", err)
	}
	running, addr := l.Status()
	if !running || portOf(addr) != port {
		t.Fatalf("status after no-op: running=%v addr=%s", running, addr)
	}
}

// TestReconfigure_SameBindAddressChange covers the user-reported bug: keeping
// the same port but switching the bind address (127.0.0.1 → 0.0.0.0) must
// succeed. The old bind-then-close path always failed here because 0.0.0.0:N
// can't coexist with 127.0.0.1:N.
func TestReconfigure_SameBindAddressChange(t *testing.T) {
	t.Parallel()
	port := pickFreePort(t)

	l, cleanup := newRunningSet(t, "127.0.0.1", port)
	defer cleanup()

	if err := l.Reconfigure(context.Background(), "0.0.0.0", port); err != nil {
		t.Fatalf("Reconfigure 127.0.0.1 → 0.0.0.0 (same port): %v", err)
	}
	running, addr := l.Status()
	if !running {
		t.Fatalf("expected listener running after bind swap, got running=false")
	}
	// Go's "tcp" listener for 0.0.0.0 reports [::] in dual-stack mode on most
	// platforms — assert on port only.
	if portOf(addr) != port {
		t.Fatalf("port drifted after bind swap: addr=%s want port=%d", addr, port)
	}
}

// TestReconfigure_DifferentPort verifies the bind-then-close happy path: a
// clean move to a brand-new port leaves the listener running on the new port.
func TestReconfigure_DifferentPort(t *testing.T) {
	t.Parallel()
	port := pickFreePort(t)

	l, cleanup := newRunningSet(t, "127.0.0.1", port)
	defer cleanup()

	newPort := pickFreePort(t)
	if newPort == port {
		newPort = pickFreePort(t)
	}
	if err := l.Reconfigure(context.Background(), "127.0.0.1", newPort); err != nil {
		t.Fatalf("Reconfigure to new port: %v", err)
	}
	running, addr := l.Status()
	if !running || portOf(addr) != newPort {
		t.Fatalf("status after port change: running=%v addr=%s want port=%d", running, addr, newPort)
	}
}

// TestReconfigure_SamePortRollback verifies that when the new bind fails on
// the close-then-bind path, the old listener is restored rather than left
// stopped. Triggers the conflict by squatting on the target wildcard bind so
// the same-port path's new bind cannot succeed.
func TestReconfigure_SamePortRollback(t *testing.T) {
	t.Parallel()
	port := pickFreePort(t)

	l, cleanup := newRunningSet(t, "127.0.0.1", port)
	defer cleanup()

	// Squat a wildcard listener so that reconfiguring to 0.0.0.0:squatPort
	// fails the new bind regardless of platform (dual-stack vs IPv4-only).
	squatPort, squatCleanup := squatTCP(t, "0.0.0.0")
	defer squatCleanup()

	err := l.Reconfigure(context.Background(), "0.0.0.0", squatPort)
	if err == nil {
		t.Fatal("expected Reconfigure error when new port is squatted")
	}
	running, addr := l.Status()
	if !running {
		t.Fatalf("expected old listener restored after rollback, got running=false (err=%v)", err)
	}
	if portOf(addr) != port {
		t.Fatalf("rollback did not restore original port: addr=%s want %d", addr, port)
	}
}

// --- helpers ---

func newRunningSet(t *testing.T, bind string, port int) (*listenerSet, func()) {
	t.Helper()
	core := routing.NewCore(nil, nil)
	mgr := tunnel.New(core)
	reg := registry.New()
	deny := auth.New(nil)
	l := &listenerSet{mgr: mgr, reg: reg, denyList: deny}

	ctx, cancel := context.WithCancel(context.Background())
	if err := l.Start(ctx, bind, port); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	cleanup := func() {
		l.Stop()
		cancel()
		// Brief wait so the kernel releases the socket before the next subtest
		// binds to similar ports.
		time.Sleep(10 * time.Millisecond)
	}
	return l, cleanup
}

func pickFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen :0: %v", err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	n, _ := strconv.Atoi(p)
	return n
}

// squatTCP binds a fresh listener and returns its port + a cleanup. Used to
// force a "new port already in use" condition for rollback tests.
func squatTCP(t *testing.T, host string) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatalf("squat listen: %v", err)
	}
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return n, func() { _ = ln.Close() }
}

// portOf parses "host:port" (or "[::]:port") and returns the port. Returns 0
// on malformed input so tests fail loudly instead of silently passing.
func portOf(addr string) int {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}
