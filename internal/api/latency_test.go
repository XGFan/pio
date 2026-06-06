package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/guofan/webshare-proxy/internal/api"
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
