package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guofan/pio/internal/api"
	"github.com/guofan/pio/internal/repo"
	"github.com/guofan/pio/internal/store"
)

// TestListUpstreams_IncludesDefault proves the built-in default upstream IS
// returned by the admin listing, so the web portal can offer it as a
// user→upstream mapping target (it was previously hidden — that was the bug).
func TestListUpstreams_IncludesDefault(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	ctx := context.Background()
	if err := repo.EnsureDefaultUpstream(ctx, db.DB); err != nil {
		t.Fatalf("EnsureDefaultUpstream: %v", err)
	}
	// Seed a real (webshare) row so we can tell the default row apart from an
	// empty list.
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO api_keys (label, encrypted_key, added_at) VALUES ('k', X'00', datetime('now'))`,
	); err != nil {
		t.Fatal(err)
	}
	var keyID int64
	_ = db.DB.QueryRow(`SELECT last_insert_rowid()`).Scan(&keyID)
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO upstream_proxies
			(id, source, source_api_key_id, host, port, encrypted_password, protocol, display_name, last_seen_at)
		VALUES ('w_visible', 'webshare', ?, '1.1.1.1', 80, X'00', 'http', 'US-01', datetime('now'))`,
		keyID); err != nil {
		t.Fatal(err)
	}

	h := api.New(api.Deps{DB: db.DB}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/upstreams", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawDefault, sawWebshare bool
	for _, u := range out {
		if u["id"] == repo.DefaultUpstreamID && u["source"] == repo.SourceDefault {
			sawDefault = true
		}
		if u["id"] == "w_visible" {
			sawWebshare = true
		}
	}
	if !sawDefault {
		t.Fatalf("default upstream missing from /api/v1/upstreams: %+v", out)
	}
	if !sawWebshare {
		t.Fatalf("webshare row missing from /api/v1/upstreams: %+v", out)
	}
}

// TestPatchUpstream_RejectsDefault proves the built-in default upstream's display
// name (the universal-password/subscription routing key) is immutable via the
// generic display-name PATCH endpoint.
func TestPatchUpstream_RejectsDefault(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	ctx := context.Background()
	if err := repo.EnsureDefaultUpstream(ctx, db.DB); err != nil {
		t.Fatalf("EnsureDefaultUpstream: %v", err)
	}

	h := api.New(api.Deps{DB: db.DB}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/upstreams/"+repo.DefaultUpstreamID,
		strings.NewReader(`{"display_name":"renamed"}`),
	))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}

	// The display name must be unchanged after the rejected edit.
	got, err := repo.GetUpstream(ctx, db.DB, repo.DefaultUpstreamID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if got.DisplayName != repo.DefaultUpstreamID {
		t.Errorf("display_name mutated to %q", got.DisplayName)
	}
}
