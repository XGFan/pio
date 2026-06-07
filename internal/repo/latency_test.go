package repo_test

import (
	"context"
	"testing"
	"time"

	"github.com/guofan/pia/internal/repo"
	"github.com/guofan/pia/internal/store"
)

func TestUpdateUpstreamLatency_RoundTrip(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	id, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "lat", Host: "1.2.3.4", Port: 1080, Protocol: repo.ProtocolHTTP,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Fresh row: latency unset.
	u, err := repo.GetUpstream(ctx, db.DB, id)
	if err != nil {
		t.Fatal(err)
	}
	if u.LastLatencyMS != nil || u.LastLatencyAt != nil {
		t.Fatalf("fresh latency should be nil: ms=%v at=%v", u.LastLatencyMS, u.LastLatencyAt)
	}

	// A measured latency.
	at := time.Now().UTC().Truncate(time.Second)
	if err := repo.UpdateUpstreamLatency(ctx, db.DB, id, 123, at); err != nil {
		t.Fatal(err)
	}
	u, _ = repo.GetUpstream(ctx, db.DB, id)
	if u.LastLatencyMS == nil || *u.LastLatencyMS != 123 {
		t.Errorf("LastLatencyMS = %v, want 123", u.LastLatencyMS)
	}
	if u.LastLatencyAt == nil || !u.LastLatencyAt.Equal(at) {
		t.Errorf("LastLatencyAt = %v, want %v", u.LastLatencyAt, at)
	}

	// A failed probe is recorded as -1.
	if err := repo.UpdateUpstreamLatency(ctx, db.DB, id, -1, at); err != nil {
		t.Fatal(err)
	}
	u, _ = repo.GetUpstream(ctx, db.DB, id)
	if u.LastLatencyMS == nil || *u.LastLatencyMS != -1 {
		t.Errorf("LastLatencyMS = %v, want -1", u.LastLatencyMS)
	}
}
