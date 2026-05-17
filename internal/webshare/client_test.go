package webshare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newClientForTest returns a Client whose baseURL points at srv and whose
// retry schedule is tiny so the suite runs in milliseconds, not seconds.
func newClientForTest(t *testing.T, srv *httptest.Server, delays ...time.Duration) *Client {
	t.Helper()
	c := New("test-token", srv.Client())
	c.baseURL = srv.URL
	if len(delays) > 0 {
		c.retryDelays = delays
	} else {
		c.retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	}
	return c
}

func TestListProxiesSinglePage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token test-token" {
			t.Errorf("auth header = %q", got)
		}
		if !strings.Contains(r.URL.RawQuery, "mode=direct") || !strings.Contains(r.URL.RawQuery, "page_size=100") {
			t.Errorf("query missing required params: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{
			"next": null,
			"results": [
				{"proxy_address":"1.1.1.1","port":1080,"username":"u","password":"p","country_code":"US"},
				{"proxy_address":"2.2.2.2","port":1080,"username":"v","password":"q","country_code":"DE","city_name":"Berlin"}
			]
		}`))
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	got, err := c.ListProxies(context.Background())
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d proxies, want 2", len(got))
	}
	if got[1].CityName != "Berlin" {
		t.Errorf("city_name not parsed: %+v", got[1])
	}
}

func TestListProxiesPagination(t *testing.T) {
	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/proxy/list/", func(w http.ResponseWriter, r *http.Request) {
		// page 1: emit next pointing at /page2
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{
			"next": %q,
			"results": [{"proxy_address":"1.1.1.1","port":1,"username":"a","password":"x","country_code":"US"}]
		}`, srv.URL+"/page2")
	})
	mux.HandleFunc("/page2", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"next": null,
			"results": [
				{"proxy_address":"2.2.2.2","port":2,"username":"b","password":"x","country_code":"DE"},
				{"proxy_address":"3.3.3.3","port":3,"username":"c","password":"x","country_code":"JP"}
			]
		}`))
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	c := newClientForTest(t, srv)
	got, err := c.ListProxies(context.Background())
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d proxies, want 3", len(got))
	}
	if got[0].CountryCode != "US" || got[2].CountryCode != "JP" {
		t.Errorf("page order wrong: %+v", got)
	}
}

func TestListProxiesRetriesOn5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"next":null,"results":[{"proxy_address":"1.1.1.1","port":1,"username":"u","password":"p","country_code":"US"}]}`))
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	got, err := c.ListProxies(context.Background())
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 calls (2 fail + 1 succeed), got %d", calls.Load())
	}
}

func TestListProxiesExhaustsRetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "still broken", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	_, err := c.ListProxies(context.Background())
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "exhausted retries") {
		t.Errorf("error should mention exhausted retries, got: %v", err)
	}
	// initial attempt + 3 retries = 4 total
	if calls.Load() != 4 {
		t.Errorf("expected 4 calls, got %d", calls.Load())
	}
}

func TestListProxiesUnauthorizedNoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, `{"detail":"Invalid token"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	_, err := c.ListProxies(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 call (no retry), got %d", calls.Load())
	}
}

func TestListProxies4xxNoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	_, err := c.ListProxies(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 call (no retry on 4xx), got %d", calls.Load())
	}
}

func TestListProxiesContextCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// Long backoff so the cancel below fires while we're sleeping between retries.
	c := newClientForTest(t, srv, 200*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.ListProxies(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("cancel during backoff was slow: %v", elapsed)
	}
}

func TestListProxiesNetworkErrorRetries(t *testing.T) {
	// Close the server immediately so every Dial fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	c := newClientForTest(t, srv)
	_, err := c.ListProxies(context.Background())
	if err == nil {
		t.Fatal("expected error against a closed server")
	}
	if !strings.Contains(err.Error(), "exhausted retries") {
		t.Errorf("expected retry-exhaustion error, got: %v", err)
	}
}
