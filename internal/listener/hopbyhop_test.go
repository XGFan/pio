package listener

import (
	"net/http"
	"testing"
)

// TestStripHopByHop: the fixed hop-by-hop set, Proxy-Connection, and every
// header named in any Connection line are all removed; end-to-end headers stay.
func TestStripHopByHop(t *testing.T) {
	h := http.Header{}
	h.Add("Connection", "X-Listed-A")
	h.Add("Connection", "keep-alive, X-Listed-B")
	h.Set("X-Listed-A", "a")
	h.Set("X-Listed-B", "b")
	h.Set("Keep-Alive", "timeout=5")
	h.Set("Proxy-Connection", "keep-alive")
	h.Set("Proxy-Authorization", "Basic eDp5")
	h.Set("X-End-To-End", "kept")

	StripHopByHop(h)

	if len(h) != 1 || h.Get("X-End-To-End") != "kept" {
		t.Fatalf("after strip: %v, want only X-End-To-End", h)
	}
}
