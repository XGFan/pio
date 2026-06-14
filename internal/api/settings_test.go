package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/guofan/pio/internal/api"
	"github.com/guofan/pio/internal/auth"
	"github.com/guofan/pio/internal/repo"
	"github.com/guofan/pio/internal/routing"
	"github.com/guofan/pio/internal/store"
)

// TestGetUniversalPassword_Set returns the stored value and set:true.
func TestGetUniversalPassword_Set(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	mk := subKey()
	if err := repo.SetUniversalProxyPassword(ctx, db.DB, mk, "s3cret"); err != nil {
		t.Fatalf("SetUniversalProxyPassword: %v", err)
	}

	h := api.New(api.Deps{DB: db.DB, MasterKey: mk}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/settings/universal-password", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Password string `json:"password"`
		Set      bool   `json:"set"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Password != "s3cret" || !out.Set {
		t.Fatalf("got %+v, want {password:s3cret set:true}", out)
	}
}

// TestGetUniversalPassword_Unset returns {"",false} when no password is set.
func TestGetUniversalPassword_Unset(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	t.Cleanup(func() { _ = db.Close() })
	mk := subKey()

	h := api.New(api.Deps{DB: db.DB, MasterKey: mk}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/settings/universal-password", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		Password string `json:"password"`
		Set      bool   `json:"set"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Password != "" || out.Set {
		t.Fatalf("got %+v, want {password:\"\" set:false}", out)
	}
}

// TestPutSettings_409WhenRunning rejects a settings edit while the proxy is
// running with a 409 {error:"proxy_running"} and leaves settings unchanged.
func TestPutSettings_409WhenRunning(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()

	h := api.New(api.Deps{
		DB:          db.DB,
		ProxyStatus: func() (bool, string) { return true, "127.0.0.1:8080" },
	}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(
		http.MethodPut, "/api/v1/settings",
		strings.NewReader(`{"proxy_port":9999,"proxy_bind":"0.0.0.0","sync_interval_minutes":30}`),
	))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out["error"] != "proxy_running" {
		t.Fatalf("error = %q, want proxy_running; body=%s", out["error"], rr.Body.String())
	}
	// Settings must be untouched by the rejected edit.
	st, err := repo.LoadSettings(ctx, db.DB)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if st.ProxyPort == 9999 {
		t.Fatalf("settings were mutated despite 409: proxy_port=%d", st.ProxyPort)
	}
}

// TestPutSettings_SucceedsWhenStopped persists the new settings while the proxy
// is stopped, with no listener reconfigure involved.
func TestPutSettings_SucceedsWhenStopped(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	mk := subKey()
	core := routing.NewCore(db.DB, mk)
	if err := core.Hydrate(ctx); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}

	h := api.New(api.Deps{
		DB:          db.DB,
		MasterKey:   mk,
		Core:        core,
		DenyList:    auth.New(nil),
		ProxyStatus: func() (bool, string) { return false, "" },
	}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(
		http.MethodPut, "/api/v1/settings",
		strings.NewReader(`{"proxy_port":9999,"proxy_bind":"0.0.0.0","sync_interval_minutes":30}`),
	))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var out struct {
		ProxyPort           int    `json:"proxy_port"`
		ProxyBind           string `json:"proxy_bind"`
		SyncIntervalMinutes int    `json:"sync_interval_minutes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ProxyPort != 9999 || out.ProxyBind != "0.0.0.0" || out.SyncIntervalMinutes != 30 {
		t.Fatalf("response = %+v, want port=9999 bind=0.0.0.0 sync=30", out)
	}
	// Verify it persisted.
	st, err := repo.LoadSettings(ctx, db.DB)
	if err != nil {
		t.Fatalf("LoadSettings: %v", err)
	}
	if st.ProxyPort != 9999 || st.ProxyBind != "0.0.0.0" || st.SyncIntervalMinutes != 30 {
		t.Fatalf("persisted settings = %+v, want port=9999 bind=0.0.0.0 sync=30", st)
	}
}
