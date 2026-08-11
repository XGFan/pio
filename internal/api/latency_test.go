package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guofan/pio/internal/api"
	"github.com/guofan/pio/internal/repo"
)

func TestTestLatencyHandler_ReturnsResults(t *testing.T) {
	var called bool
	h := api.New(api.Deps{
		TestAllLatency: func(ctx context.Context) ([]api.LatencyResult, error) {
			called = true
			return []api.LatencyResult{
				{UpstreamID: "a", OK: true, LatencyMS: 42},
				{UpstreamID: "b", OK: false, LatencyMS: 0},
			}, nil
		},
	}).Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/upstreams/test-latency", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if !called {
		t.Error("TestAllLatency closure was not invoked")
	}
	var out []api.LatencyResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].UpstreamID != "a" || out[0].LatencyMS != 42 || !out[0].OK || out[1].OK {
		t.Errorf("results wrong: %+v", out)
	}
}

func TestTestLatencyHandler_NotConfigured(t *testing.T) {
	h := api.New(api.Deps{}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/upstreams/test-latency", nil))
	if rr.Code != 500 {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}

// The single-upstream route must not be shadowed by the batch route, and it
// must pass the path id straight through to the closure.
func TestTestUpstreamLatencyHandler_ReturnsResult(t *testing.T) {
	var gotID string
	h := api.New(api.Deps{
		TestUpstreamLatency: func(ctx context.Context, id string) (*api.LatencyResult, error) {
			gotID = id
			return &api.LatencyResult{UpstreamID: id, OK: true, LatencyMS: 37}, nil
		},
		TestAllLatency: func(ctx context.Context) ([]api.LatencyResult, error) {
			t.Error("batch closure invoked for a single-upstream request")
			return nil, nil
		},
	}).Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/upstreams/manual-1/test-latency", nil))
	if rr.Code != 200 {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if gotID != "manual-1" {
		t.Errorf("upstream id = %q, want manual-1", gotID)
	}
	var out api.LatencyResult
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.LatencyMS != 37 || out.UpstreamID != "manual-1" {
		t.Errorf("result wrong: %+v", out)
	}
}

func TestTestUpstreamLatencyHandler_NotFound(t *testing.T) {
	h := api.New(api.Deps{
		TestUpstreamLatency: func(ctx context.Context, id string) (*api.LatencyResult, error) {
			return nil, repo.ErrNotFound
		},
	}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/upstreams/nope/test-latency", nil))
	if rr.Code != 404 {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
}

func TestTestUpstreamLatencyHandler_NotConfigured(t *testing.T) {
	h := api.New(api.Deps{}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/upstreams/x/test-latency", nil))
	if rr.Code != 500 {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
}
