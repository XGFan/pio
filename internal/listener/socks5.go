package listener

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/guofan/webshare-proxy/internal/auth"
	"github.com/guofan/webshare-proxy/internal/registry"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// SOCKS5 protocol constants per RFC 1928 / RFC 1929.
const (
	socksVer       byte = 0x05
	authVerUserPwd byte = 0x01

	methodUserPwd  byte = 0x02
	methodNoAccept byte = 0xff

	cmdConnect byte = 0x01
	// cmdBind (0x02) and cmdUDPAssociate (0x03) are intentionally not
	// named here: the listener rejects every non-CONNECT command with
	// reply 0x07, so the values appear only via the "cmd != cmdConnect"
	// guard. Tests reference them by literal value, which is fine for
	// the cross-package boundary.

	atypIPv4   byte = 0x01
	atypDomain byte = 0x03
	atypIPv6   byte = 0x04

	repSucceeded             byte = 0x00
	repGeneralFailure        byte = 0x01
	repCommandNotSupported   byte = 0x07
	repAddrTypeNotSupported  byte = 0x08
)

// SOCKS5Proxy is the local-side SOCKS5 forward proxy. Like HTTPProxy, it
// reads creds from the client, calls tunnel.Manager.Acquire, then bridges
// the upstream connection. Hand-rolled (vs a third-party library) so the
// ATYP/CMD matrix and "never resolve domain locally" rule are explicit
// in this file.
type SOCKS5Proxy struct {
	bindAddr string
	mgr      *tunnel.Manager
	reg      *registry.ConnectionRegistry
	deny     *auth.DenyList
	log      *slog.Logger
	ln       net.Listener
}

// NewSOCKS5Proxy returns an unbound proxy. reg + deny may be nil; when
// deny is non-nil, 10 auth failures within 60s from one client IP lead
// to a 5-min deny (immediate close on accept).
func NewSOCKS5Proxy(bindAddr string, mgr *tunnel.Manager, reg *registry.ConnectionRegistry, deny *auth.DenyList, log *slog.Logger) *SOCKS5Proxy {
	if log == nil {
		log = slog.Default()
	}
	return &SOCKS5Proxy{bindAddr: bindAddr, mgr: mgr, reg: reg, deny: deny, log: log}
}

// Bind opens the TCP listener but does not start accepting.
func (p *SOCKS5Proxy) Bind() error {
	if p.ln != nil {
		return fmt.Errorf("socks5 proxy already bound to %s", p.ln.Addr())
	}
	ln, err := net.Listen("tcp", p.bindAddr)
	if err != nil {
		return fmt.Errorf("socks5 listener bind %s: %w", p.bindAddr, err)
	}
	p.ln = ln
	return nil
}

// Addr returns the bound address (or the configured bindAddr if not yet bound).
func (p *SOCKS5Proxy) Addr() string {
	if p.ln == nil {
		return p.bindAddr
	}
	return p.ln.Addr().String()
}

// Serve runs the accept loop until ctx is cancelled.
func (p *SOCKS5Proxy) Serve(ctx context.Context) error {
	if p.ln == nil {
		return fmt.Errorf("socks5 proxy: Serve called before Bind")
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
func (p *SOCKS5Proxy) Close() error {
	if p.ln != nil {
		return p.ln.Close()
	}
	return nil
}

func (p *SOCKS5Proxy) handleConn(ctx context.Context, conn net.Conn) {
	// Deny-list gate: banned IPs are closed without parsing.
	if p.deny != nil && p.deny.IsDenied(conn.RemoteAddr().String()) {
		_ = conn.Close()
		return
	}

	// 1. Negotiation: read [VER, NMETHODS, METHODS...] and pick user/pass.
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		_ = conn.Close()
		return
	}
	if hdr[0] != socksVer {
		_ = conn.Close()
		return
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(conn, methods); err != nil {
		_ = conn.Close()
		return
	}
	if !containsByte(methods, methodUserPwd) {
		_, _ = conn.Write([]byte{socksVer, methodNoAccept})
		_ = conn.Close()
		return
	}
	if _, err := conn.Write([]byte{socksVer, methodUserPwd}); err != nil {
		_ = conn.Close()
		return
	}

	// 2. RFC 1929 sub-negotiation: [VER=1, ULEN, UNAME, PLEN, PASSWD] →
	// [VER=1, STATUS]. STATUS 0x00 = success; non-zero closes the conn.
	username, password, ok := readRFC1929(conn)
	if !ok {
		_, _ = conn.Write([]byte{authVerUserPwd, 0x01})
		_ = conn.Close()
		return
	}

	upstream, upstreamPwd, cg, err := p.mgr.Acquire(ctx, username, password)
	if err != nil {
		if p.deny != nil && (errors.Is(err, tunnel.ErrUnknownUser) || errors.Is(err, tunnel.ErrBadPassword)) {
			p.deny.RecordFailure(conn.RemoteAddr().String())
		}
		_, _ = conn.Write([]byte{authVerUserPwd, 0x01})
		_ = conn.Close()
		return
	}
	if _, err := conn.Write([]byte{authVerUserPwd, 0x00}); err != nil {
		_ = conn.Close()
		return
	}

	// 3. SOCKS5 request: [VER=5, CMD, RSV=0, ATYP, DST.ADDR, DST.PORT].
	// Header-first so unknown ATYP gets reply 0x08 (not 0x01) without
	// blocking on a partial-addr Read that would never complete.
	cmd, atyp, err := readRequestHeader(conn)
	if err != nil {
		writeReply(conn, repGeneralFailure)
		_ = conn.Close()
		return
	}
	if cmd != cmdConnect {
		// BIND (0x02) and UDP ASSOCIATE (0x03) explicitly rejected per
		// .omc/plans/webshare-v4.1.md §5.2.
		writeReply(conn, repCommandNotSupported)
		_ = conn.Close()
		return
	}
	if atyp != atypIPv4 && atyp != atypIPv6 && atyp != atypDomain {
		writeReply(conn, repAddrTypeNotSupported)
		_ = conn.Close()
		return
	}
	target, err := readRequestAddr(conn, atyp)
	if err != nil {
		writeReply(conn, repGeneralFailure)
		_ = conn.Close()
		return
	}

	// 4. Dial upstream via HTTP CONNECT. Target is passed VERBATIM —
	// for ATYP=0x03 (domain), we never resolve it locally. That's the
	// invariant that keeps geo-routing honest.
	connCtx, cancelConn := context.WithCancel(cg.Context())
	defer cancelConn()

	if p.reg != nil {
		ac := &registry.ActiveConnection{
			Username:   username,
			UpstreamID: upstream.ID,
			ClientAddr: conn.RemoteAddr().String(),
			Protocol:   "socks5",
			Target:     target,
			AcceptedAt: time.Now(),
			CancelFunc: cancelConn,
		}
		connID := p.reg.Register(ac)
		defer p.reg.Deregister(connID)
	}

	upConn, err := p.mgr.DialUpstream(connCtx, upstream, upstreamPwd, target)
	if err != nil {
		writeReply(conn, repGeneralFailure)
		_ = conn.Close()
		p.log.Warn("socks5 dial failed", "target", target, "err", err)
		return
	}
	// Reply 0x00 succeeded with BND.ADDR = 0.0.0.0:0 (our local socket
	// address isn't useful to the client and SOCKS5 allows zero here).
	if err := writeReply(conn, repSucceeded); err != nil {
		_ = upConn.Close()
		_ = conn.Close()
		return
	}

	_, _, _ = tunnel.Bridge(connCtx, conn, upConn)
}

// readRFC1929 parses [VER=1, ULEN, UNAME, PLEN, PASSWD] from the client.
func readRFC1929(conn net.Conn) (username, password string, ok bool) {
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return "", "", false
	}
	if hdr[0] != authVerUserPwd {
		return "", "", false
	}
	uname := make([]byte, hdr[1])
	if _, err := io.ReadFull(conn, uname); err != nil {
		return "", "", false
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(conn, plen); err != nil {
		return "", "", false
	}
	pword := make([]byte, plen[0])
	if _, err := io.ReadFull(conn, pword); err != nil {
		return "", "", false
	}
	return string(uname), string(pword), true
}

// readRequestHeader parses the fixed-size [VER, CMD, RSV, ATYP] prefix.
// Split out so the caller can validate cmd/atyp before attempting to
// consume a variable-length address whose layout depends on atyp.
func readRequestHeader(conn net.Conn) (cmd, atyp byte, err error) {
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return 0, 0, err
	}
	if hdr[0] != socksVer || hdr[2] != 0x00 {
		return 0, 0, errors.New("socks5: malformed request header")
	}
	return hdr[1], hdr[3], nil
}

// readRequestAddr consumes the variable-length address + 2-byte port and
// returns "host:port" with the host portion preserved verbatim
// (dotted-quad for IPv4, bracketed for IPv6, FQDN for domain).
func readRequestAddr(conn net.Conn, atyp byte) (string, error) {
	var host string
	switch atyp {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(conn, b); err != nil {
			return "", err
		}
		host = "[" + net.IP(b).String() + "]"
	case atypDomain:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(conn, lb); err != nil {
			return "", err
		}
		nb := make([]byte, lb[0])
		if _, err := io.ReadFull(conn, nb); err != nil {
			return "", err
		}
		host = string(nb)
	default:
		return "", errors.New("socks5: unsupported address type")
	}
	pb := make([]byte, 2)
	if _, err := io.ReadFull(conn, pb); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(pb)
	return host + ":" + strconv.Itoa(int(port)), nil
}

// writeReply emits [VER=5, REP, RSV=0, ATYP=IPv4, 0.0.0.0, 0] — the
// minimum legal SOCKS5 reply. We always advertise 0.0.0.0:0 as BND
// because the client doesn't care about our local-side socket.
func writeReply(conn net.Conn, rep byte) error {
	_, err := conn.Write([]byte{socksVer, rep, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

func containsByte(haystack []byte, needle byte) bool {
	return slices.Contains(haystack, needle)
}
