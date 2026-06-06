package integration

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/guofan/webshare-proxy/internal/repo"
)

// TestDirectUpstream_EgressBypassesProxy proves the built-in direct upstream
// routes a client straight to the target via the daemon's own network: a user
// mapped to "direct" reaches the echo target through the local HTTP proxy while
// the mock upstream sees ZERO requests (no proxy hop was taken).
func TestDirectUpstream_EgressBypassesProxy(t *testing.T) {
	s := newScenario(t)
	ctx := context.Background()

	if err := repo.EnsureDirectUpstream(ctx, s.db.DB); err != nil {
		t.Fatalf("EnsureDirectUpstream: %v", err)
	}
	const directUser, directPwd = "duser", "dpw"
	if err := repo.InsertLocalUser(ctx, s.db.DB, directUser, directPwd, ""); err != nil {
		t.Fatalf("InsertLocalUser: %v", err)
	}
	directID := repo.DirectUpstreamID
	if err := repo.UpdateLocalUserMapping(ctx, s.db.DB, directUser, &directID); err != nil {
		t.Fatalf("UpdateLocalUserMapping: %v", err)
	}
	// Re-hydrate so the freshly-seeded direct upstream + new user are routable.
	if err := s.core.Hydrate(ctx); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	conn := dialAndConnect(t, s.proxyAddr, s.echo.Addr(), directUser, directPwd)
	defer conn.Close()

	const payload = "ping-direct"
	if _, err := io.WriteString(conn, payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("echo read: %v", err)
	}
	if string(buf) != payload {
		t.Fatalf("echo mismatch: got %q want %q", buf, payload)
	}

	// The clincher: direct egress means the mock upstream was never dialed.
	if reqs := s.upstream.Requests(); len(reqs) != 0 {
		t.Fatalf("mock upstream saw %d requests; direct egress must bypass it: %+v", len(reqs), reqs)
	}
}
