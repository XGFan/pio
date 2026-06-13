package repo_test

import (
	"context"
	"testing"

	"github.com/guofan/pio/internal/repo"
	"github.com/guofan/pio/internal/store"
)

// TestEnsureDefaultUpstream_CreatesAndIsIdempotent verifies the built-in default
// row is seeded with the reserved id/source/display name, carries no upstream
// auth, and that a second call is a harmless no-op (self-healing on every boot).
func TestEnsureDefaultUpstream_CreatesAndIsIdempotent(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	if err := repo.EnsureDefaultUpstream(ctx, db.DB); err != nil {
		t.Fatalf("EnsureDefaultUpstream: %v", err)
	}

	got, err := repo.GetUpstream(ctx, db.DB, repo.DefaultUpstreamID)
	if err != nil {
		t.Fatalf("GetUpstream(default): %v", err)
	}
	if got.Source != repo.SourceDefault {
		t.Errorf("source = %q, want %q", got.Source, repo.SourceDefault)
	}
	if got.DisplayName != repo.DefaultUpstreamID {
		t.Errorf("display_name = %q, want %q", got.DisplayName, repo.DefaultUpstreamID)
	}
	if got.SourceApiKeyID != nil {
		t.Errorf("source_api_key_id = %v, want nil", *got.SourceApiKeyID)
	}

	// Idempotent: second call must not error or duplicate.
	if err := repo.EnsureDefaultUpstream(ctx, db.DB); err != nil {
		t.Fatalf("EnsureDefaultUpstream (2nd): %v", err)
	}

	// It must appear in the routing-hydration map exactly once with no password.
	all, err := repo.ListAllResolvedUpstreams(ctx, db.DB, mk)
	if err != nil {
		t.Fatalf("ListAllResolvedUpstreams: %v", err)
	}
	row, ok := all[repo.DefaultUpstreamID]
	if !ok {
		t.Fatal("default upstream missing from resolved map")
	}
	if row.Password != "" {
		t.Errorf("default password = %q, want empty", row.Password)
	}
	if len(all) != 1 {
		t.Errorf("resolved upstream count = %d, want 1 (only default)", len(all))
	}
}
