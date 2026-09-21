package listener

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guofan/pio/internal/crypto"
	"github.com/guofan/pio/internal/repo"
	"github.com/guofan/pio/internal/routing"
	"github.com/guofan/pio/internal/store"
	"github.com/guofan/pio/internal/tunnel"
)

// startUnified serves a UnifiedProxy over mgr (nil for tests that never get
// past the handshake).
func startUnified(t *testing.T, mgr *tunnel.Manager) string {
	t.Helper()
	p := NewUnifiedProxy("127.0.0.1:0", mgr, nil, nil, nil)
	if err := p.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = p.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return p.Addr()
}

// expectClosed fails unless the proxy closes conn within d.
func expectClosed(t *testing.T, conn net.Conn, d time.Duration) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(d))
	if _, err := io.Copy(io.Discard, conn); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("proxy still holds the connection after %v", d)
	}
}

// TestHandshakeTimeout_ClosesStalledClients: a client that stops partway
// through the HTTP request head or the SOCKS5 handshake is cut off.
func TestHandshakeTimeout_ClosesStalledClients(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })
	addr := startUnified(t, nil)

	for name, partial := range map[string]string{
		"http request head": "GET http://example.com/ HTTP/1.1\r\nHost: exa",
		"socks5 greeting":   "\x05\x02",
		"socks5 auth":       "\x05\x01\x02\x01\x05al",
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, partial); err != nil {
				t.Fatal(err)
			}
			expectClosed(t, conn, 2*time.Second)
		})
	}
}

// TestHTTPRequestHead_OversizedIsRejected: an endless request header is cut
// off at the size cap, long before the handshake timeout, instead of being
// buffered without bound.
func TestHTTPRequestHead_OversizedIsRejected(t *testing.T) {
	addr := startUnified(t, nil)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		_, _ = io.WriteString(conn, "GET http://example.com/ HTTP/1.1\r\nX-Big: ")
		_, _ = io.WriteString(conn, strings.Repeat("a", 2*http.DefaultMaxHeaderBytes))
	}()
	expectClosed(t, conn, 3*time.Second)
	_ = conn.Close()
	<-wrote
}

const (
	hsUser = "alice"
	hsPwd  = "alicepw"
)

// directManager routes hsUser to the built-in default upstream, which dials
// targets directly, so tunnels reach local test servers.
func directManager(t *testing.T) *tunnel.Manager {
	t.Helper()
	ctx := context.Background()
	db := store.MustOpenInMemoryTest(t)
	if err := repo.EnsureDefaultUpstream(ctx, db.DB); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertLocalUser(ctx, db.DB, hsUser, hsPwd, ""); err != nil {
		t.Fatal(err)
	}
	id := repo.DefaultUpstreamID
	if err := repo.UpdateLocalUserMapping(ctx, db.DB, hsUser, &id); err != nil {
		t.Fatal(err)
	}
	core := routing.NewCore(db.DB, make([]byte, crypto.MasterKeySize))
	if err := core.Hydrate(ctx); err != nil {
		t.Fatal(err)
	}
	return tunnel.New(core)
}

// startEcho serves a TCP echo target.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	return ln.Addr().String()
}

// echoAfterIdle sits idle past handshakeTimeout, then round-trips a payload.
func echoAfterIdle(t *testing.T, conn net.Conn, r io.Reader) {
	t.Helper()
	time.Sleep(3 * handshakeTimeout)
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo after idle: %q %v", buf, err)
	}
}

// TestHandshakeTimeout_OnlyBoundsTheHandshake: once the handshake is done its
// deadline and head cap are lifted — tunnels outlive handshakeTimeout and a
// plain-HTTP body may exceed the head cap.
func TestHandshakeTimeout_OnlyBoundsTheHandshake(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })
	addr := startUnified(t, directManager(t))
	echo := startEcho(t)
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(hsUser+":"+hsPwd))

	t.Run("http connect tunnel", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", echo, echo, auth)
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, nil)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("CONNECT: %v %v", resp, err)
		}
		echoAfterIdle(t, conn, br)
	})

	t.Run("socks5 tunnel", func(t *testing.T) {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		host, portStr, _ := net.SplitHostPort(echo)
		port, _ := strconv.Atoi(portStr)
		req := []byte{0x05, 0x01, 0x02, 0x01, byte(len(hsUser))}
		req = append(req, hsUser...)
		req = append(req, byte(len(hsPwd)))
		req = append(req, hsPwd...)
		req = append(req, 0x05, 0x01, 0x00, 0x01)
		req = append(req, net.ParseIP(host).To4()...)
		req = binary.BigEndian.AppendUint16(req, uint16(port))
		if _, err := conn.Write(req); err != nil {
			t.Fatal(err)
		}
		// method choice (2) + auth status (2) + CONNECT reply (10)
		reply := make([]byte, 14)
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, reply); err != nil || reply[3] != 0x00 || reply[5] != 0x00 {
			t.Fatalf("socks5 handshake: % x %v", reply, err)
		}
		echoAfterIdle(t, conn, conn)
	})

	t.Run("plain http body over head cap", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			fmt.Fprint(w, n)
		}))
		defer origin.Close()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		size := 3 * http.DefaultMaxHeaderBytes
		go func() {
			fmt.Fprintf(conn, "POST %s/ HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\nContent-Length: %d\r\n\r\n",
				origin.URL, strings.TrimPrefix(origin.URL, "http://"), auth, size)
			_, _ = io.WriteString(conn, strings.Repeat("x", size))
		}()
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != strconv.Itoa(size) {
			t.Fatalf("status %d body %q, want 200 %d", resp.StatusCode, body, size)
		}
	})
}
