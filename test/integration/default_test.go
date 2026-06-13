package integration

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/guofan/pio/internal/repo"
)

// TestDefaultUpstream_EgressBypassesProxy proves the built-in default upstream
// routes a client straight to the target via the daemon's own network: a user
// mapped to "default" reaches the echo target through the local HTTP proxy while
// the mock upstream sees ZERO requests (no proxy hop was taken).
func TestDefaultUpstream_EgressBypassesProxy(t *testing.T) {
	s := newScenario(t)
	ctx := context.Background()

	if err := repo.EnsureDefaultUpstream(ctx, s.db.DB); err != nil {
		t.Fatalf("EnsureDefaultUpstream: %v", err)
	}
	const defaultUser, defaultPwd = "duser", "dpw"
	if err := repo.InsertLocalUser(ctx, s.db.DB, defaultUser, defaultPwd, ""); err != nil {
		t.Fatalf("InsertLocalUser: %v", err)
	}
	defaultID := repo.DefaultUpstreamID
	if err := repo.UpdateLocalUserMapping(ctx, s.db.DB, defaultUser, &defaultID); err != nil {
		t.Fatalf("UpdateLocalUserMapping: %v", err)
	}
	// Re-hydrate so the freshly-seeded default upstream + new user are routable.
	if err := s.core.Hydrate(ctx); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	conn := dialAndConnect(t, s.proxyAddr, s.echo.Addr(), defaultUser, defaultPwd)
	defer conn.Close()

	const payload = "ping-default"
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

	// The clincher: default egress means the mock upstream was never dialed.
	if reqs := s.upstream.Requests(); len(reqs) != 0 {
		t.Fatalf("mock upstream saw %d requests; default egress must bypass it: %+v", len(reqs), reqs)
	}
}
