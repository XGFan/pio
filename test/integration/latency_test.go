package integration

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/guofan/webshare-proxy/internal/latency"
	"github.com/guofan/webshare-proxy/internal/model"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/routing"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// fakeTunnelProxy is a minimal HTTP CONNECT proxy that ignores the requested
// target and tunnels every accepted connection to backendAddr. It lets us
// drive MeasureLatency (whose hard-coded target www.gstatic.com:80 must NOT be
// resolved) against a local mock instead of the real internet.
func fakeTunnelProxy(t *testing.T, backendAddr string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil { // consume CONNECT
					return
				}
				if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
					return
				}
				back, err := net.Dial("tcp", backendAddr)
				if err != nil {
					return
				}
				defer back.Close()
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(back, br); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, back); done <- struct{}{} }()
				<-done
			}()
		}
	}()
	return ln.Addr().String()
}

func mockProxyUpstream(id, addr string) model.UpstreamProxy {
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	return model.UpstreamProxy{ID: id, Host: host, Port: port, Protocol: "http"}
}

func latencyMgr() *tunnel.Manager { return tunnel.New(routing.NewCore(nil, nil)) }

func TestMeasureLatency_Success(t *testing.T) {
	gstatic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // 204, like the real generate_204
	}))
	defer gstatic.Close()
	proxyAddr := fakeTunnelProxy(t, gstatic.Listener.Addr().String())

	up := mockProxyUpstream("u1", proxyAddr)
	d, err := latencyMgr().MeasureLatency(context.Background(), &up, "")
	if err != nil {
		t.Fatalf("MeasureLatency: %v", err)
	}
	if d <= 0 || d > 5*time.Second {
		t.Errorf("latency = %v, want a small positive duration", d)
	}
}

func TestMeasureLatency_BadStatusFails(t *testing.T) {
	gstatic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError) // 500 → not reachable-OK
	}))
	defer gstatic.Close()
	proxyAddr := fakeTunnelProxy(t, gstatic.Listener.Addr().String())

	up := mockProxyUpstream("u1", proxyAddr)
	if _, err := latencyMgr().MeasureLatency(context.Background(), &up, ""); err == nil {
		t.Fatal("expected error on non-2xx status")
	}
}

func TestLatencyRunBatch_GoodAndDead(t *testing.T) {
	gstatic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer gstatic.Close()
	goodAddr := fakeTunnelProxy(t, gstatic.Listener.Addr().String())

	good := repo.ResolvedUpstream{UpstreamProxy: mockProxyUpstream("good", goodAddr)}
	// Dead: a closed port → connection refused.
	dead := repo.ResolvedUpstream{UpstreamProxy: model.UpstreamProxy{
		ID: "dead", Host: "127.0.0.1", Port: 1, Protocol: "http",
	}}

	results := latency.RunBatch(context.Background(), latencyMgr(), []repo.ResolvedUpstream{good, dead}, 4)
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	byID := map[string]latency.Result{}
	for _, r := range results {
		byID[r.UpstreamID] = r
	}
	if !byID["good"].OK || byID["good"].LatencyMS < 0 {
		t.Errorf("good proxy result wrong: %+v", byID["good"])
	}
	if byID["dead"].OK {
		t.Errorf("dead proxy: OK=true, want false")
	}
}
