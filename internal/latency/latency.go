// Package latency runs on-demand, batched latency probes against upstream
// proxies. Each probe fetches http://www.gstatic.com/generate_204 through the
// proxy (see tunnel.Manager.MeasureLatency) and records the round-trip time.
package latency

import (
	"context"
	"sync"

	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/tunnel"
)

// Result is one upstream's probe outcome. LatencyMS is valid only when OK.
type Result struct {
	UpstreamID string
	OK         bool
	LatencyMS  int
}

// RunBatch probes every upstream concurrently, capped at `concurrency`
// simultaneous probes, and returns one Result per upstream (order matches
// ups). It never returns an error — an unreachable proxy yields OK=false —
// and honours ctx cancellation via each probe's own timeout.
func RunBatch(ctx context.Context, m *tunnel.Manager, ups []repo.ResolvedUpstream, concurrency int) []Result {
	if concurrency < 1 {
		concurrency = 1
	}
	results := make([]Result, len(ups))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i := range ups {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			up := ups[i]
			r := Result{UpstreamID: up.ID}
			if d, err := m.MeasureLatency(ctx, &up.UpstreamProxy, up.Password); err == nil {
				r.OK = true
				r.LatencyMS = int(d.Milliseconds())
			}
			results[i] = r
		}(i)
	}
	wg.Wait()
	return results
}
