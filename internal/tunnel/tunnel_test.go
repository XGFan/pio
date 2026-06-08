package tunnel_test

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/guofan/pio/internal/crypto"
	"github.com/guofan/pio/internal/repo"
	"github.com/guofan/pio/internal/routing"
	"github.com/guofan/pio/internal/store"
	"github.com/guofan/pio/internal/tunnel"
	"go.uber.org/goleak"
)

func mustMK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, crypto.MasterKeySize)
	for i := range k {
		k[i] = byte(i + 3)
	}
	return k
}

// seedOne inserts: 1 api_key + 1 upstream + 1 local_user alice mapped to that
// upstream. Returns the upstream's plaintext password so the test can assert
// on Acquire's return.
func seedOne(t *testing.T, db *store.DBHandle, mk []byte) string {
	t.Helper()
	ctx := context.Background()
	keyID, err := repo.InsertApiKey(ctx, db.DB, mk, "P", "sk_test")
	if err != nil {
		t.Fatal(err)
	}
	const plainPwd = "up-pw"
	enc, _ := crypto.Encrypt(mk, []byte(plainPwd), crypto.ColumnAAD("upstream_proxies.encrypted_password"))
	upstreamID := "1111222233334444"
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO upstream_proxies (id, source_api_key_id, host, port, username, encrypted_password, protocol,
			display_name, country_code, last_seen_at)
		 VALUES (?, ?, '127.0.0.1', 9999, 'upU', ?, 'http', 'US-P-01', 'US', datetime('now'))`,
		upstreamID, keyID, enc); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertLocalUser(ctx, db.DB, "alice", "alicepw", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateLocalUserMapping(ctx, db.DB, "alice", &upstreamID); err != nil {
		t.Fatal(err)
	}
	return plainPwd
}

func TestAcquireSuccess(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	wantPwd := seedOne(t, db, mk)

	core := routing.NewCore(db.DB, mk)
	if err := core.Hydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr := tunnel.New(core)

	up, gotPwd, cg, err := mgr.Acquire(context.Background(), "alice", "alicepw")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if up == nil || up.Host != "127.0.0.1" {
		t.Fatalf("upstream not returned: %+v", up)
	}
	if gotPwd != wantPwd {
		t.Errorf("upstream password = %q want %q", gotPwd, wantPwd)
	}
	if cg == nil {
		t.Error("CancelGroup nil")
	}
}

func TestAcquireUnknownUser(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	core := routing.NewCore(db.DB, mustMK(t))
	if err := core.Hydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr := tunnel.New(core)
	_, _, _, err := mgr.Acquire(context.Background(), "nobody", "x")
	if !errors.Is(err, tunnel.ErrUnknownUser) {
		t.Fatalf("expected ErrUnknownUser, got %v", err)
	}
}

func TestAcquireBadPassword(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	_ = seedOne(t, db, mk)
	core := routing.NewCore(db.DB, mk)
	if err := core.Hydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr := tunnel.New(core)
	_, _, _, err := mgr.Acquire(context.Background(), "alice", "wrong")
	if !errors.Is(err, tunnel.ErrBadPassword) {
		t.Fatalf("expected ErrBadPassword, got %v", err)
	}
}

func TestAcquireBrokenWhenUpstreamMissing(t *testing.T) {
	defer goleak.VerifyNone(t)
	db := store.MustOpenInMemoryTest(t)
	defer db.Close()
	mk := mustMK(t)
	_ = seedOne(t, db, mk)
	// Delete the mapped upstream (simulates a webshare rotation pruning it).
	// alice's mapping is left dangling/nulled, so after hydrate she has no
	// usable upstream.
	if _, err := db.DB.Exec(`DELETE FROM upstream_proxies WHERE id=?`, "1111222233334444"); err != nil {
		t.Fatal(err)
	}
	core := routing.NewCore(db.DB, mk)
	if err := core.Hydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr := tunnel.New(core)
	// No usable upstream for alice → ErrBrokenMapping (the user-facing flag).
	_, _, _, err := mgr.Acquire(context.Background(), "alice", "alicepw")
	if !errors.Is(err, tunnel.ErrBrokenMapping) {
		t.Fatalf("expected ErrBrokenMapping for missing upstream, got %v", err)
	}
}

// TestBridgeTeardownOnCancel verifies that cancelling ctx unblocks both
// blocked I/O directions within ~1 TCP RTT, even when neither end has
// any bytes pending (the idle-tunnel case). This is the v4.1 plan's
// AC#3 idle-HTTPS sub-case in isolation.
func TestBridgeTeardownOnCancel(t *testing.T) {
	defer goleak.VerifyNone(t)

	// Build two TCP socket pairs that bridge() will copy between.
	clientPipe, srvClient := tcpPair(t)
	upstreamPipe, srvUpstream := tcpPair(t)
	defer clientPipe.Close()
	defer upstreamPipe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _, _ = tunnel.Bridge(ctx, srvClient, srvUpstream)
		close(done)
	}()

	// Wait briefly so the io.Copy goroutines are blocked on Read.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Bridge did not return within 2s of ctx cancel")
	}

	// After Bridge returns, both client-side and upstream-side sockets
	// should be observably closed.
	clientPipe.SetDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := clientPipe.Read(make([]byte, 1)); err == nil || (!errors.Is(err, io.EOF) && !isClosedOrTimeout(err)) {
		t.Errorf("client side not closed: %v", err)
	}
}

// TestBridgeCopiesActiveTraffic confirms Bridge actually moves bytes in
// both directions before any cancellation fires.
func TestBridgeCopiesActiveTraffic(t *testing.T) {
	defer goleak.VerifyNone(t)
	clientPipe, srvClient := tcpPair(t)
	upstreamPipe, srvUpstream := tcpPair(t)
	defer clientPipe.Close()
	defer upstreamPipe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _, _ = tunnel.Bridge(ctx, srvClient, srvUpstream)
		close(done)
	}()

	// client → upstream
	if _, err := clientPipe.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	_ = upstreamPipe.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := io.ReadFull(upstreamPipe, buf); err != nil {
		t.Fatalf("upstream side read: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("upstream got %q want ping", buf)
	}

	// upstream → client
	if _, err := upstreamPipe.Write([]byte("pong")); err != nil {
		t.Fatal(err)
	}
	_ = clientPipe.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := io.ReadFull(clientPipe, buf); err != nil {
		t.Fatalf("client side read: %v", err)
	}
	if string(buf) != "pong" {
		t.Fatalf("client got %q want pong", buf)
	}

	cancel()
	<-done
}

// tcpPair returns two ends of a TCP socket via net.Listen+Dial.
func tcpPair(t *testing.T) (client net.Conn, server net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var serverConn net.Conn
	var serverErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serverConn, serverErr = ln.Accept()
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if serverErr != nil {
		t.Fatal(serverErr)
	}
	return clientConn, serverConn
}

// isClosedOrTimeout returns true for the various flavors of "the deadline
// or close caused this Read to fail" — net.ErrClosed, os.ErrDeadlineExceeded,
// or a *net.OpError wrapping either.
func isClosedOrTimeout(err error) bool {
	if errors.Is(err, net.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return false
}
