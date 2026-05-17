package mockwebshare

import (
	"errors"
	"io"
	"net"
	"sync"
)

// EchoTarget is a plain TCP server that echoes every byte it receives.
// Used as the "origin" at the end of a proxy chain in integration tests.
type EchoTarget struct {
	listener net.Listener
	wg       sync.WaitGroup
}

// NewEchoTarget binds an echo server on a random local port.
func NewEchoTarget() (*EchoTarget, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	e := &EchoTarget{listener: ln}
	e.wg.Add(1)
	go e.accept()
	return e, nil
}

// Addr returns "host:port".
func (e *EchoTarget) Addr() string { return e.listener.Addr().String() }

// Close stops accepting and waits for in-flight echoers to exit.
func (e *EchoTarget) Close() error {
	err := e.listener.Close()
	e.wg.Wait()
	return err
}

func (e *EchoTarget) accept() {
	defer e.wg.Done()
	for {
		conn, err := e.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			return
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer conn.Close()
			_, _ = io.Copy(conn, conn)
		}()
	}
}
