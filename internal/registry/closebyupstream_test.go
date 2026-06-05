package registry

import (
	"context"
	"testing"
)

// TestCloseByUpstream verifies the upstream-scoped sweep cancels every
// connection bridging through a given upstream — regardless of the username
// that opened it (including universal/display-name routes) — and leaves
// connections to other upstreams untouched.
//
// CloseByUpstream invokes each matched CancelFunc synchronously before it
// returns, so the connection contexts are already cancelled on return and we
// can assert on ctx.Err() without any goroutines or sleeps.
func TestCloseByUpstream(t *testing.T) {
	r := New()

	register := func(user, upstreamID string) context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		r.Register(&ActiveConnection{
			Username:   user,
			UpstreamID: upstreamID,
			CancelFunc: cancel,
		})
		return ctx
	}

	// Two conns to u1: one via a normal local user, one via a display-name
	// (universal) route whose username is a proxy display name. One to u2.
	ctxA := register("alice", "u1")
	ctxB := register("US-A-01", "u1")
	ctxC := register("bob", "u2")

	if n := r.CloseByUpstream("u1"); n != 2 {
		t.Fatalf("CloseByUpstream(u1) = %d, want 2", n)
	}
	if ctxA.Err() == nil {
		t.Error("conn a (u1, local user) was not cancelled")
	}
	if ctxB.Err() == nil {
		t.Error("conn b (u1, universal route) was not cancelled")
	}
	if ctxC.Err() != nil {
		t.Error("conn c (u2) was cancelled but should not have been")
	}

	if got := r.CloseByUpstream("nope"); got != 0 {
		t.Errorf("CloseByUpstream(nonexistent) = %d, want 0", got)
	}
}
