package repo_test

import (
	"context"
	"testing"

	"github.com/guofan/pio/internal/repo"
	"github.com/guofan/pio/internal/store"
)

// TestEnsureDirectUpstream_CreatesAndIsIdempotent verifies the built-in direct
// row is seeded with the reserved id/source/display name, carries no upstream
// auth, and that a second call is a harmless no-op (self-healing on every boot).
func TestEnsureDirectUpstream_CreatesAndIsIdempotent(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	if err := repo.EnsureDirectUpstream(ctx, db.DB); err != nil {
		t.Fatalf("EnsureDirectUpstream: %v", err)
	}

	got, err := repo.GetUpstream(ctx, db.DB, repo.DirectUpstreamID)
	if err != nil {
		t.Fatalf("GetUpstream(direct): %v", err)
	}
	if got.Source != repo.SourceDirect {
		t.Errorf("source = %q, want %q", got.Source, repo.SourceDirect)
	}
	if got.DisplayName != repo.DirectUpstreamID {
		t.Errorf("display_name = %q, want %q", got.DisplayName, repo.DirectUpstreamID)
	}
	if got.SourceApiKeyID != nil {
		t.Errorf("source_api_key_id = %v, want nil", *got.SourceApiKeyID)
	}

	// Idempotent: second call must not error or duplicate.
	if err := repo.EnsureDirectUpstream(ctx, db.DB); err != nil {
		t.Fatalf("EnsureDirectUpstream (2nd): %v", err)
	}

	// It must appear in the routing-hydration map exactly once with no password.
	all, err := repo.ListAllResolvedUpstreams(ctx, db.DB, mk)
	if err != nil {
		t.Fatalf("ListAllResolvedUpstreams: %v", err)
	}
	row, ok := all[repo.DirectUpstreamID]
	if !ok {
		t.Fatal("direct upstream missing from resolved map")
	}
	if row.Password != "" {
		t.Errorf("direct password = %q, want empty", row.Password)
	}
	if len(all) != 1 {
		t.Errorf("resolved upstream count = %d, want 1 (only direct)", len(all))
	}
}
