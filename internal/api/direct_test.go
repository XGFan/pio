package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guofan/webshare-proxy/internal/api"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/store"
)

// TestListUpstreams_HidesDirect proves the built-in direct upstream is an
// internal routing pattern, not shown in the admin UI listing — even though
// it exists in the DB and is routable by name.
func TestListUpstreams_HidesDirect(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	ctx := context.Background()
	if err := repo.EnsureDirectUpstream(ctx, db.DB); err != nil {
		t.Fatalf("EnsureDirectUpstream: %v", err)
	}
	// Seed a real (webshare) row so the list is non-empty and we can tell
	// "filtered direct" apart from "empty list".
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
	for _, u := range out {
		if u["id"] == repo.DirectUpstreamID || u["source"] == repo.SourceDirect {
			t.Fatalf("direct upstream leaked into /api/v1/upstreams: %+v", u)
		}
	}
	if len(out) != 1 || out[0]["id"] != "w_visible" {
		t.Fatalf("expected only the webshare row, got %+v", out)
	}
}

// TestPatchUpstream_RejectsDirect proves the built-in direct upstream's display
// name (the universal-password/subscription routing key) is immutable via the
// generic display-name PATCH endpoint.
func TestPatchUpstream_RejectsDirect(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	ctx := context.Background()
	if err := repo.EnsureDirectUpstream(ctx, db.DB); err != nil {
		t.Fatalf("EnsureDirectUpstream: %v", err)
	}

	h := api.New(api.Deps{DB: db.DB}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/upstreams/"+repo.DirectUpstreamID,
		strings.NewReader(`{"display_name":"renamed"}`),
	))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}

	// The display name must be unchanged after the rejected edit.
	got, err := repo.GetUpstream(ctx, db.DB, repo.DirectUpstreamID)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if got.DisplayName != repo.DirectUpstreamID {
		t.Errorf("display_name mutated to %q", got.DisplayName)
	}
}
