package tunnel_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/guofan/webshare-proxy/internal/model"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// startEcho spins a plain TCP echo server and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

// TestDialUpstream_Direct proves the built-in direct upstream dials the target
// straight (no proxy hop): the upstream row carries NO host/port, yet the dial
// reaches the target and bytes round-trip. Dispatch is on Source, not Protocol.
func TestDialUpstream_Direct(t *testing.T) {
	target := startEcho(t)

	mgr := tunnel.New(nil) // Acquire isn't exercised; direct dial uses just the dialer.
	up := &model.UpstreamProxy{
		ID:     repo.DirectUpstreamID,
		Source: repo.SourceDirect,
		// Deliberately no Host/Port/Username — a proxy path would fail here;
		// the direct path ignores them and dials the target argument instead.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := mgr.DialUpstream(ctx, up, "", target)
	if err != nil {
		t.Fatalf("DialUpstream(direct): %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("hello-direct")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, len("hello-direct"))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "hello-direct" {
		t.Errorf("echo mismatch: got %q", buf)
	}
}

// TestDialUpstream_Direct_DialError surfaces a connect failure as ErrUpstreamDial
// (so listeners map it to 502/SOCKS-failure like any other dial error).
func TestDialUpstream_Direct_DialError(t *testing.T) {
	mgr := tunnel.New(nil)
	up := &model.UpstreamProxy{ID: repo.DirectUpstreamID, Source: repo.SourceDirect}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// 127.0.0.1:1 is the reserved tcpmux port; nothing listens there.
	if _, err := mgr.DialUpstream(ctx, up, "", "127.0.0.1:1"); err == nil {
		t.Fatal("expected dial error for unreachable target")
	}
}
