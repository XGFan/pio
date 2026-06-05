package webshare

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReplace_PostsCorrectBodyAndParses(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody ReplaceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1,"state":"completed","dry_run":true,"proxies_removed":1,"proxies_added":1}`))
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	res, err := c.Replace(context.Background(), ReplaceRequest{
		ToReplace:   ReplaceTarget{Type: "ip_address", IPAddresses: []string{"82.22.69.206"}},
		ReplaceWith: []ReplaceWith{{Type: "asn", ASNNumbers: []int{3356}}},
		DryRun:      true,
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/api/v2/proxy/replace/" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Token test-token" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotBody.ToReplace.Type != "ip_address" || len(gotBody.ToReplace.IPAddresses) != 1 || gotBody.ToReplace.IPAddresses[0] != "82.22.69.206" {
		t.Errorf("to_replace wrong: %+v", gotBody.ToReplace)
	}
	if len(gotBody.ReplaceWith) != 1 || gotBody.ReplaceWith[0].Type != "asn" || len(gotBody.ReplaceWith[0].ASNNumbers) != 1 || gotBody.ReplaceWith[0].ASNNumbers[0] != 3356 {
		t.Errorf("replace_with wrong: %+v", gotBody.ReplaceWith)
	}
	if !gotBody.DryRun {
		t.Error("dry_run should be true")
	}
	if !res.DryRun || res.ProxiesRemoved != 1 || res.ProxiesAdded != 1 || res.State != "completed" {
		t.Errorf("result parse wrong: %+v", res)
	}
}

func TestReplace_CountryOmitsAsnField(t *testing.T) {
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"state":"completed","proxies_removed":1,"proxies_added":1}`))
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	if _, err := c.Replace(context.Background(), ReplaceRequest{
		ToReplace:   ReplaceTarget{Type: "ip_address", IPAddresses: []string{"1.2.3.4"}},
		ReplaceWith: []ReplaceWith{{Type: "country", CountryCode: "JP"}},
	}); err != nil {
		t.Fatal(err)
	}
	rw := raw["replace_with"].([]any)[0].(map[string]any)
	if rw["country_code"] != "JP" {
		t.Errorf("country_code missing: %v", rw)
	}
	if _, present := rw["asn_numbers"]; present {
		t.Error("asn_numbers should be omitted for a country replacement")
	}
}

func TestReplace_APIErrorSurfacesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"replace_with":{"asn_numbers":[{"message":"This field is required."}]}}`))
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	_, err := c.Replace(context.Background(), ReplaceRequest{ToReplace: ReplaceTarget{Type: "ip_address"}})
	if err == nil {
		t.Fatal("expected error on 400")
	}
}

func TestGetConfig_ParsesAvailableCountriesAndASNs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/proxy/config/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"available_countries": {"JP": 757, "US": 30475},
			"available_asns": {"3356": ["Level3", 1510], "8881": ["1&1 Versatel Gmbh", 6576]}
		}`))
	}))
	defer srv.Close()

	c := newClientForTest(t, srv)
	cfg, err := c.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if cfg.AvailableCountries["JP"] != 757 || cfg.AvailableCountries["US"] != 30475 {
		t.Errorf("countries wrong: %+v", cfg.AvailableCountries)
	}
	if len(cfg.AvailableASNs) != 2 {
		t.Fatalf("asns len = %d want 2", len(cfg.AvailableASNs))
	}
	// Sorted by name: "1&1 Versatel Gmbh" before "Level3".
	if cfg.AvailableASNs[0].Number != 8881 || cfg.AvailableASNs[0].Name != "1&1 Versatel Gmbh" || cfg.AvailableASNs[0].Available != 6576 {
		t.Errorf("asn[0] wrong: %+v", cfg.AvailableASNs[0])
	}
	if cfg.AvailableASNs[1].Number != 3356 || cfg.AvailableASNs[1].Available != 1510 {
		t.Errorf("asn[1] wrong: %+v", cfg.AvailableASNs[1])
	}
}
