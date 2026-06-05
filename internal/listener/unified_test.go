package listener

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

// fakeConn is a minimal net.Conn that serves Read from a buffer and records
// the control-method calls prefixConn is expected to delegate to the real
// socket (deadlines, Write, Close).
type fakeConn struct {
	readBuf      *bytes.Buffer
	writeBuf     bytes.Buffer
	closed       bool
	readDeadline time.Time
	deadlineSet  bool
}

func (c *fakeConn) Read(p []byte) (int, error)  { return c.readBuf.Read(p) }
func (c *fakeConn) Write(p []byte) (int, error) { return c.writeBuf.Write(p) }
func (c *fakeConn) Close() error                { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr         { return nil }
func (c *fakeConn) RemoteAddr() net.Addr        { return nil }
func (c *fakeConn) SetDeadline(t time.Time) error {
	c.deadlineSet = true
	c.readDeadline = t
	return nil
}
func (c *fakeConn) SetReadDeadline(t time.Time) error {
	c.deadlineSet = true
	c.readDeadline = t
	return nil
}
func (c *fakeConn) SetWriteDeadline(t time.Time) error { return nil }

// TestPrefixConn_ReplaysByteThenStream verifies the sniffed byte is replayed
// ahead of the live connection: a reader sees the original, uninterrupted
// stream (prefix byte first, then whatever the socket carries).
func TestPrefixConn_ReplaysByteThenStream(t *testing.T) {
	underlying := &fakeConn{readBuf: bytes.NewBufferString("ELLO")} // after the sniffed 'H'
	pc := newPrefixConn(underlying, []byte{'H'})

	got, err := io.ReadAll(pc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != "HELLO" {
		t.Fatalf("replayed stream = %q, want %q", got, "HELLO")
	}
}

// TestPrefixConn_DelegatesControlToRealConn pins the contract tunnel.Bridge
// relies on: deadline, Write, and Close all act on the real socket, not on
// the byte-replay wrapper. If these didn't delegate, a hot-switch could never
// tear the connection down.
func TestPrefixConn_DelegatesControlToRealConn(t *testing.T) {
	underlying := &fakeConn{readBuf: bytes.NewBuffer(nil)}
	pc := newPrefixConn(underlying, []byte{0x05})

	deadline := time.Now().Add(42 * time.Second)
	if err := pc.SetReadDeadline(deadline); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if !underlying.deadlineSet || !underlying.readDeadline.Equal(deadline) {
		t.Errorf("SetReadDeadline not delegated to underlying conn (set=%v val=%v want %v)",
			underlying.deadlineSet, underlying.readDeadline, deadline)
	}

	if _, err := pc.Write([]byte("xyz")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if underlying.writeBuf.String() != "xyz" {
		t.Errorf("Write not delegated: underlying got %q", underlying.writeBuf.String())
	}

	if err := pc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !underlying.closed {
		t.Error("Close not delegated to underlying conn")
	}
}
