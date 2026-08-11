package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/guofan/pio/internal/repo"
)

// LatencyResult is one upstream's latency-probe outcome on the wire.
type LatencyResult struct {
	UpstreamID string `json:"upstream_id"`
	OK         bool   `json:"ok"`
	LatencyMS  int    `json:"latency_ms"`
}

// testLatency probes every upstream's latency in a batch and persists the
// results (the closure handles persistence), returning the per-upstream
// outcomes. The UI then refreshes to show the updated latency column.
func (s *Server) testLatency(w http.ResponseWriter, r *http.Request) {
	if s.deps.TestAllLatency == nil {
		writeErr(w, 500, "latency test not configured")
		return
	}
	results, err := s.deps.TestAllLatency(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, results)
}

// testUpstreamLatency probes exactly one upstream (by id) and persists the
// result. Lets the UI check a single proxy — e.g. a manual proxy the operator
// just added — without paying for a full batch across every synced upstream.
func (s *Server) testUpstreamLatency(w http.ResponseWriter, r *http.Request) {
	if s.deps.TestUpstreamLatency == nil {
		writeErr(w, 500, "latency test not configured")
		return
	}
	res, err := s.deps.TestUpstreamLatency(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			writeErr(w, 404, "upstream not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}
