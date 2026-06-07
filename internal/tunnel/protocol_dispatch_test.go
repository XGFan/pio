package tunnel_test

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/guofan/pia/internal/model"
	"github.com/guofan/pia/internal/repo"
	"github.com/guofan/pia/internal/tunnel"
)

// startSOCKS5Stub spins up an in-process server that accepts the canonical
// RFC 1928 / RFC 1929 client handshake with USERNAME/PASSWORD and a
// CONNECT for ATYP=domain, then bridges the resulting tunnel to a
// payload conn the test controls.
func startSOCKS5Stub(t *testing.T, wantUser, wantPwd, wantTarget string) (addr string, peer net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	peerL, peerR := net.Pipe()
	t.Cleanup(func() { _ = peerL.Close(); _ = peerR.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Greeting: VER NMETHODS METHODS
		hdr := make([]byte, 2)
		if _, err := io.ReadFull(conn, hdr); err != nil {
			return
		}
		methods := make([]byte, hdr[1])
		if _, err := io.ReadFull(conn, methods); err != nil {
			return
		}
		// Select USER/PASS if offered, else NO AUTH.
		method := byte(0xFF)
		for _, m := range methods {
			if m == 0x02 && wantUser != "" {
				method = 0x02
				break
			}
			if m == 0x00 && wantUser == "" {
				method = 0x00
				break
			}
		}
		if _, err := conn.Write([]byte{0x05, method}); err != nil || method == 0xFF {
			return
		}

		if method == 0x02 {
			ah := make([]byte, 2)
			if _, err := io.ReadFull(conn, ah); err != nil {
				return
			}
			uname := make([]byte, ah[1])
			if _, err := io.ReadFull(conn, uname); err != nil {
				return
			}
			pl := make([]byte, 1)
			if _, err := io.ReadFull(conn, pl); err != nil {
				return
			}
			pword := make([]byte, pl[0])
			if _, err := io.ReadFull(conn, pword); err != nil {
				return
			}
			if string(uname) != wantUser || string(pword) != wantPwd {
				_, _ = conn.Write([]byte{0x01, 0x01})
				return
			}
			if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
				return
			}
		}

		// CONNECT request: VER CMD RSV ATYP …
		head := make([]byte, 4)
		if _, err := io.ReadFull(conn, head); err != nil {
			return
		}
		if head[1] != 0x01 || head[3] != 0x03 { // CONNECT + domain
			return
		}
		lb := make([]byte, 1)
		if _, err := io.ReadFull(conn, lb); err != nil {
			return
		}
		hostBuf := make([]byte, lb[0])
		if _, err := io.ReadFull(conn, hostBuf); err != nil {
			return
		}
		portBuf := make([]byte, 2)
		if _, err := io.ReadFull(conn, portBuf); err != nil {
			return
		}
		host := string(hostBuf)
		port := int(portBuf[0])<<8 | int(portBuf[1])
		gotTarget := host + ":" + itoa(port)
		if gotTarget != wantTarget {
			t.Errorf("socks5 target = %q want %q", gotTarget, wantTarget)
		}
		// Reply success: VER REP RSV ATYP=IPv4 0.0.0.0:0
		if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}

		// Bridge bytes between the dialed conn and the test-controlled peer.
		go io.Copy(conn, peerR)
		_, _ = io.Copy(peerR, conn)
	}()

	return ln.Addr().String(), peerL
}

func itoa(n int) string {
	// Tiny dependency-free int-to-string; we only ever pass valid uint16s.
	if n == 0 {
		return "0"
	}
	var b [6]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestDialUpstream_SOCKS5_WithAuth(t *testing.T) {
	addr, peer := startSOCKS5Stub(t, "alice", "p@ss", "example.com:443")
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	mgr := tunnel.New(nil) // Acquire isn't exercised; DialUpstream uses just the dialer.
	upstream := &model.UpstreamProxy{
		Host:     host,
		Port:     port,
		Username: "alice",
		Protocol: repo.ProtocolSOCKS5,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := mgr.DialUpstream(ctx, upstream, "p@ss", "example.com:443")
	if err != nil {
		t.Fatalf("DialUpstream: %v", err)
	}
	defer conn.Close()

	// End-to-end byte echo: write into the tunnel; the stub's peer side
	// should receive it.
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(peer, buf); err != nil {
		t.Fatalf("peer read: %v", err)
	}
	if string(buf) != "hello" {
		t.Errorf("peer got %q want hello", buf)
	}
}

func TestDialUpstream_SOCKS5_NoAuth(t *testing.T) {
	addr, _ := startSOCKS5Stub(t, "", "", "example.com:443")
	host, portStr, _ := net.SplitHostPort(addr)
	var port int
	for _, c := range portStr {
		port = port*10 + int(c-'0')
	}

	mgr := tunnel.New(nil)
	upstream := &model.UpstreamProxy{
		Host: host, Port: port, Protocol: repo.ProtocolSOCKS5,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := mgr.DialUpstream(ctx, upstream, "", "example.com:443")
	if err != nil {
		t.Fatalf("DialUpstream no-auth: %v", err)
	}
	_ = conn.Close()
}

func TestDialUpstream_UnknownProtocol(t *testing.T) {
	mgr := tunnel.New(nil)
	upstream := &model.UpstreamProxy{Host: "127.0.0.1", Port: 1, Protocol: "ftp"}
	_, err := mgr.DialUpstream(context.Background(), upstream, "", "example.com:443")
	if err == nil || !strings.Contains(err.Error(), "unsupported upstream protocol") {
		t.Fatalf("expected unsupported-protocol error, got %v", err)
	}
}
