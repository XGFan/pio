package listener

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/guofan/webshare-proxy/internal/auth"
	"github.com/guofan/webshare-proxy/internal/registry"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// sniffDeadline bounds how long we wait for a freshly-accepted connection to
// send its first byte. A legitimate proxy client sends immediately (HTTP
// request line or SOCKS5 greeting), so this is deliberately short: an
// idle/half-open connection that never speaks must not pin a goroutine while
// we wait to learn its protocol. Kept tight to blunt slow-connection floods
// against an externally-exposed port.
const sniffDeadline = 10 * time.Second

// UnifiedProxy serves BOTH the HTTP forward proxy and SOCKS5 on a single
// port. For each accepted connection it reads one byte and dispatches:
//
//   - 0x05 → SOCKS5 (RFC 1928 greetings always start with VER=0x05)
//   - anything else → HTTP (proxy requests start with an ASCII method token)
//
// SOCKS4 (0x04) is not supported and falls through to the HTTP handler, which
// rejects it as a malformed request. The peeked byte is preserved and replayed
// to the chosen handler via prefixConn, so neither handler can tell it wasn't
// reading the raw socket.
//
// UnifiedProxy owns no protocol logic of its own: it reuses the existing
// HTTPProxy / SOCKS5Proxy per-connection handlers, constructed unbound purely
// for their handleConn methods.
type UnifiedProxy struct {
	bindAddr string
	httpH    *HTTPProxy
	socksH   *SOCKS5Proxy
	deny     *auth.DenyList
	log      *slog.Logger
	ln       net.Listener
}

// NewUnifiedProxy returns an unbound proxy. Call Bind then Serve. reg + deny
// may be nil; when deny is non-nil, banned client IPs are closed on accept
// before any byte is read.
func NewUnifiedProxy(bindAddr string, mgr *tunnel.Manager, reg *registry.ConnectionRegistry, deny *auth.DenyList, log *slog.Logger) *UnifiedProxy {
	if log == nil {
		log = slog.Default()
	}
	return &UnifiedProxy{
		bindAddr: bindAddr,
		httpH:    NewHTTPProxy(bindAddr, mgr, reg, deny, log),
		socksH:   NewSOCKS5Proxy(bindAddr, mgr, reg, deny, log),
		deny:     deny,
		log:      log,
	}
}

// Bind opens the TCP listener but does not start accepting.
func (p *UnifiedProxy) Bind() error {
	if p.ln != nil {
		return fmt.Errorf("unified proxy already bound to %s", p.ln.Addr())
	}
	ln, err := net.Listen("tcp", p.bindAddr)
	if err != nil {
		return fmt.Errorf("unified listener bind %s: %w", p.bindAddr, err)
	}
	p.ln = ln
	return nil
}

// Addr returns the bound address (or the configured bindAddr if not yet bound).
func (p *UnifiedProxy) Addr() string {
	if p.ln == nil {
		return p.bindAddr
	}
	return p.ln.Addr().String()
}

// Serve runs the accept loop until ctx is cancelled or the listener closes.
func (p *UnifiedProxy) Serve(ctx context.Context) error {
	if p.ln == nil {
		return fmt.Errorf("unified proxy: Serve called before Bind")
	}
	stopped := make(chan struct{})
	defer close(stopped)
	go func() {
		select {
		case <-ctx.Done():
			_ = p.ln.Close()
		case <-stopped:
		}
	}()

	var wg sync.WaitGroup
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			wg.Wait()
			return fmt.Errorf("accept: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.handleConn(ctx, conn)
		}()
	}
}

// Close stops accepting new connections; in-flight handlers continue.
func (p *UnifiedProxy) Close() error {
	if p.ln != nil {
		return p.ln.Close()
	}
	return nil
}

// handleConn applies the deny gate, sniffs the first byte, and dispatches to
// the HTTP or SOCKS5 per-connection handler with the byte replayed.
func (p *UnifiedProxy) handleConn(ctx context.Context, conn net.Conn) {
	// Deny-list gate runs BEFORE the first read so banned IPs consume no
	// parser time (matches the standalone listeners' contract).
	if p.deny != nil && p.deny.IsDenied(conn.RemoteAddr().String()) {
		_ = conn.Close()
		return
	}

	_ = conn.SetReadDeadline(time.Now().Add(sniffDeadline))
	first := make([]byte, 1)
	if _, err := io.ReadFull(conn, first); err != nil {
		_ = conn.Close()
		return
	}
	// Clear the sniff deadline; the protocol handlers manage their own
	// deadlines on the upstream conn, and tunnel.Bridge drives client-side
	// deadlines during teardown.
	_ = conn.SetReadDeadline(time.Time{})

	pc := newPrefixConn(conn, first)
	if first[0] == socksVer {
		p.socksH.handleConn(ctx, pc)
		return
	}
	p.httpH.handleConn(ctx, pc)
}

// prefixConn replays a small prefix (the sniffed byte) ahead of the live
// connection, so a handler reading from it sees the original byte stream. All
// methods except Read delegate to the embedded net.Conn, so deadlines, writes,
// and Close operate on the real socket — which is what tunnel.Bridge needs.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func newPrefixConn(c net.Conn, prefix []byte) *prefixConn {
	return &prefixConn{Conn: c, r: io.MultiReader(bytes.NewReader(prefix), c)}
}

func (c *prefixConn) Read(p []byte) (int, error) { return c.r.Read(p) }
