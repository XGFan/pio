// Package mockwebshare provides test doubles for the webshare.io services
// that piad talks to: the v2 list API, the CONNECT-capable HTTP
// proxy, the optional native SOCKS5 proxy, and a plain TCP echo target
// used as the "origin" in end-to-end integration tests.
//
// Each constructor returns a *Server with a URL/Addr field and a Close
// method. The HTTP upstream and SOCKS5 upstream both record every
// observed (proxy_authorization_username, target) pair so tests can
// assert routing behavior.
package mockwebshare

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// HTTPUpstreamRequest captures one CONNECT request handled by HTTPUpstream.
type HTTPUpstreamRequest struct {
	Username string // decoded from Proxy-Authorization Basic
	Password string
	Target   string // CONNECT target authority "host:port"
}

// HTTPUpstream is a minimal HTTP CONNECT proxy. It accepts a single set
// of credentials and bridges to whatever target the client requests.
type HTTPUpstream struct {
	listener net.Listener
	user     string
	pass     string

	mu       sync.Mutex
	requests []HTTPUpstreamRequest

	wg   sync.WaitGroup
	done chan struct{}
}

// NewHTTPUpstream binds an HTTP CONNECT proxy on a random local port.
// Connections must authenticate with (user, pass) — anything else gets 407.
func NewHTTPUpstream(user, pass string) (*HTTPUpstream, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	u := &HTTPUpstream{listener: ln, user: user, pass: pass, done: make(chan struct{})}
	u.wg.Add(1)
	go u.accept()
	return u, nil
}

// Addr returns the bound address as "host:port".
func (s *HTTPUpstream) Addr() string { return s.listener.Addr().String() }

// Host returns just the host portion of Addr.
func (s *HTTPUpstream) Host() string {
	h, _, _ := net.SplitHostPort(s.listener.Addr().String())
	return h
}

// Port returns just the port portion of Addr.
func (s *HTTPUpstream) Port() int {
	_, p, _ := net.SplitHostPort(s.listener.Addr().String())
	var port int
	fmt.Sscanf(p, "%d", &port)
	return port
}

// Requests returns a snapshot of every CONNECT this server has handled.
func (s *HTTPUpstream) Requests() []HTTPUpstreamRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]HTTPUpstreamRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

// Close stops accepting new connections and waits for in-flight ones to drain.
func (s *HTTPUpstream) Close() error {
	close(s.done)
	err := s.listener.Close()
	s.wg.Wait()
	return err
}

func (s *HTTPUpstream) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(conn)
		}()
	}
}

func (s *HTTPUpstream) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		return
	}

	user, pass, ok := decodeBasic(req.Header.Get("Proxy-Authorization"))
	if !ok || user != s.user || pass != s.pass {
		conn.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		return
	}

	target := req.RequestURI
	if target == "" {
		target = req.Host
	}

	s.mu.Lock()
	s.requests = append(s.requests, HTTPUpstreamRequest{Username: user, Password: pass, Target: target})
	s.mu.Unlock()

	upstream, err := net.DialTimeout("tcp", target, 5*1000*1000*1000)
	if err != nil {
		conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		return
	}
	defer upstream.Close()

	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}

	// Bridge until either side returns or we're closed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-s.done:
			cancel()
		case <-ctx.Done():
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, conn); _ = upstream.Close() }()
	go func() { defer wg.Done(); _, _ = io.Copy(conn, upstream); _ = conn.Close() }()
	wg.Wait()
}

func decodeBasic(h string) (string, string, bool) {
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
