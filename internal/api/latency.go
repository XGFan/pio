package api

import "net/http"

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
