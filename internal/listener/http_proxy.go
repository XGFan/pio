// Package listener implements the local HTTP and SOCKS5 forward proxies
// that clients connect to. Both translate client requests into HTTP
// CONNECT calls against the webshare upstream selected by routing.
//
// Phase 2 ships the HTTP listener; Phase 3 adds SOCKS5. Hot-switch
// (Phase 4) does not require listener-side changes — cancellation
// propagates through tunnel.Bridge via the CancelGroup context.
package listener

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/guofan/pia/internal/auth"
	"github.com/guofan/pia/internal/model"
	"github.com/guofan/pia/internal/registry"
	"github.com/guofan/pia/internal/tunnel"
)

// HTTPProxy is the local-side HTTP forward proxy.
//
// Lifecycle:
//   p := NewHTTPProxy(":0", mgr, reg, log)
//   if err := p.Bind(); err != nil { ... }   // socket exists now
//   addr := p.Addr()                          // safe to read after Bind
//   go p.Serve(ctx)                           // accept loop until ctx cancel
//
// reg may be nil; when non-nil, every accepted+authenticated connection
// is Registered before Bridge starts and Deregistered after it returns,
// so Phase 4's hot-switch can force-close them via the registry.
type HTTPProxy struct {
	bindAddr string
	mgr      *tunnel.Manager
	reg      *registry.ConnectionRegistry
	deny     *auth.DenyList
	log      *slog.Logger
	ln       net.Listener
}

// NewHTTPProxy returns an unbound proxy. Call Bind then Serve. deny may be
// nil; when non-nil, 10 auth failures within 60s from one client IP lead
// to a 5-min 403 ban (per US-019).
func NewHTTPProxy(bindAddr string, mgr *tunnel.Manager, reg *registry.ConnectionRegistry, deny *auth.DenyList, log *slog.Logger) *HTTPProxy {
	if log == nil {
		log = slog.Default()
	}
	return &HTTPProxy{bindAddr: bindAddr, mgr: mgr, reg: reg, deny: deny, log: log}
}

// Bind opens the TCP listener but does not start accepting. Safe to call
// once; subsequent calls error out.
func (p *HTTPProxy) Bind() error {
	if p.ln != nil {
		return fmt.Errorf("http proxy already bound to %s", p.ln.Addr())
	}
	ln, err := net.Listen("tcp", p.bindAddr)
	if err != nil {
		return fmt.Errorf("http listener bind %s: %w", p.bindAddr, err)
	}
	p.ln = ln
	return nil
}

// Addr returns the bound address. Returns the configured bindAddr until
// Bind succeeds.
func (p *HTTPProxy) Addr() string {
	if p.ln == nil {
		return p.bindAddr
	}
	return p.ln.Addr().String()
}

// Serve runs the accept loop until ctx is cancelled or the listener is
// closed. Returns nil for normal shutdown.
func (p *HTTPProxy) Serve(ctx context.Context) error {
	if p.ln == nil {
		return fmt.Errorf("http proxy: Serve called before Bind")
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
func (p *HTTPProxy) Close() error {
	if p.ln != nil {
		return p.ln.Close()
	}
	return nil
}

// handleConn reads the first request, authenticates, then dispatches.
func (p *HTTPProxy) handleConn(ctx context.Context, conn net.Conn) {
	// Deny-list gate runs BEFORE reading the request body — banned IPs
	// don't get to consume any parser time.
	if p.deny != nil && p.deny.IsDenied(conn.RemoteAddr().String()) {
		writeStatus(conn, 403, "rate limited")
		return
	}

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		_ = conn.Close()
		return
	}

	username, password, ok := parseProxyAuth(req.Header.Get("Proxy-Authorization"))
	if !ok {
		write407(conn, "missing or malformed Proxy-Authorization")
		return
	}

	upstream, upstreamPwd, cg, err := p.mgr.Acquire(ctx, username, password)
	if err != nil {
		// Record auth failures so the deny-list can ban abusive clients.
		if p.deny != nil && (errors.Is(err, tunnel.ErrUnknownUser) || errors.Is(err, tunnel.ErrBadPassword)) {
			p.deny.RecordFailure(conn.RemoteAddr().String())
		}
		p.handleAuthError(conn, err)
		return
	}

	// Derive a per-connection context from the mapping's CancelGroup so a
	// hot-switch (Phase 4) tears this conn down via Bridge's deadline trip.
	connCtx, cancelConn := context.WithCancel(cg.Context())
	defer cancelConn()

	// Register with the connection registry so the registry can wake this
	// goroutine via cancelConn during a mapping swap or force-disconnect.
	var connID uint64
	if p.reg != nil {
		target := req.RequestURI
		if target == "" {
			target = req.Host
		}
		ac := &registry.ActiveConnection{
			Username:   username,
			UpstreamID: upstream.ID,
			ClientAddr: conn.RemoteAddr().String(),
			Protocol:   "http",
			Target:     target,
			AcceptedAt: time.Now(),
			CancelFunc: cancelConn,
		}
		connID = p.reg.Register(ac)
		defer p.reg.Deregister(connID)
	}
	_ = connID

	if req.Method == http.MethodConnect {
		p.handleConnect(connCtx, conn, req, upstream, upstreamPwd)
		return
	}
	p.handleAbsoluteForm(connCtx, conn, req, upstream, upstreamPwd)
}

// handleConnect implements CONNECT host:port over the dialed upstream.
func (p *HTTPProxy) handleConnect(ctx context.Context, clientConn net.Conn, req *http.Request, upstream *model.UpstreamProxy, upstreamPwd string) {
	target := req.RequestURI // for CONNECT, RequestURI is host:port
	if target == "" {
		target = req.Host
	}
	upConn, err := p.mgr.DialUpstream(ctx, upstream, upstreamPwd, target)
	if err != nil {
		write502(clientConn, "upstream dial failed")
		p.log.Warn("connect dial failed", "target", target, "err", err)
		return
	}
	if _, err := io.WriteString(clientConn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		_ = upConn.Close()
		_ = clientConn.Close()
		return
	}
	_, _, _ = tunnel.Bridge(ctx, clientConn, upConn)
}

// handleAbsoluteForm proxies non-CONNECT requests (e.g. plain HTTP) by
// stripping hop-by-hop headers, rewriting to origin-form, and sending the
// request to the upstream after a CONNECT to the destination's authority.
func (p *HTTPProxy) handleAbsoluteForm(ctx context.Context, clientConn net.Conn, req *http.Request, upstream *model.UpstreamProxy, upstreamPwd string) {
	if req.URL == nil || req.URL.Host == "" {
		write400(clientConn, "absolute-form URL required")
		return
	}
	authority := req.URL.Host
	if !strings.Contains(authority, ":") {
		if req.URL.Scheme == "https" {
			authority += ":443"
		} else {
			authority += ":80"
		}
	}
	upConn, err := p.mgr.DialUpstream(ctx, upstream, upstreamPwd, authority)
	if err != nil {
		write502(clientConn, "upstream dial failed")
		return
	}

	StripHopByHop(req.Header)
	// Rewrite to origin form: keep path+query, drop scheme+host.
	originForm := req.URL.RequestURI()
	if originForm == "" {
		originForm = "/"
	}
	if _, err := fmt.Fprintf(upConn, "%s %s HTTP/1.1\r\nHost: %s\r\n", req.Method, originForm, req.URL.Host); err != nil {
		_ = upConn.Close()
		return
	}
	if err := req.Header.Write(upConn); err != nil {
		_ = upConn.Close()
		return
	}
	if _, err := io.WriteString(upConn, "\r\n"); err != nil {
		_ = upConn.Close()
		return
	}
	if req.Body != nil {
		_, _ = io.Copy(upConn, req.Body)
	}
	_, _, _ = tunnel.Bridge(ctx, clientConn, upConn)
}

// handleAuthError maps tunnel sentinel errors to HTTP responses.
func (p *HTTPProxy) handleAuthError(conn net.Conn, err error) {
	switch {
	case errors.Is(err, tunnel.ErrUnknownUser), errors.Is(err, tunnel.ErrBadPassword):
		write407(conn, "invalid credentials")
	case errors.Is(err, tunnel.ErrBrokenMapping):
		write502(conn, "upstream not available")
	default:
		write502(conn, "internal error")
	}
}

// parseProxyAuth extracts (username, password) from a Basic header.
func parseProxyAuth(h string) (string, string, bool) {
	if !strings.HasPrefix(h, "Basic ") {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, "Basic "))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func write407(conn net.Conn, msg string) {
	body := []byte(msg + "\n")
	_, _ = fmt.Fprintf(conn,
		"HTTP/1.1 407 Proxy Authentication Required\r\n"+
			"Proxy-Authenticate: Basic realm=\"pia\"\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n\r\n", len(body))
	_, _ = conn.Write(body)
	_ = conn.Close()
}

func write502(conn net.Conn, msg string) {
	body := []byte(msg + "\n")
	_, _ = fmt.Fprintf(conn,
		"HTTP/1.1 502 Bad Gateway\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n\r\n", len(body))
	_, _ = conn.Write(body)
	_ = conn.Close()
}

func write400(conn net.Conn, msg string) {
	writeStatus(conn, 400, msg)
}

// writeStatus writes a generic short response with the given status code.
func writeStatus(conn net.Conn, code int, msg string) {
	body := []byte(msg + "\n")
	statusText := http.StatusText(code)
	_, _ = fmt.Fprintf(conn,
		"HTTP/1.1 %d %s\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"Content-Length: %d\r\n"+
			"Connection: close\r\n\r\n", code, statusText, len(body))
	_, _ = conn.Write(body)
	_ = conn.Close()
}
