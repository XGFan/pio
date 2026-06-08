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
)

// replaceServer builds an api.Server whose ReplaceUpstream/WebshareReplaceOptions
// closures are supplied by the test, so we can exercise the HTTP handlers
// (validation + error mapping) in isolation.
func replaceServer(deps api.Deps) http.Handler {
	return api.New(deps).Handler()
}

func postReplace(t *testing.T, h http.Handler, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/v1/upstreams/"+id+"/replace", strings.NewReader(body)))
	return rr
}

func TestReplaceUpstream_Validation(t *testing.T) {
	called := false
	h := replaceServer(api.Deps{
		ReplaceUpstream: func(context.Context, string, api.ReplaceUpstreamInput) (*api.ReplaceUpstreamResult, error) {
			called = true
			return &api.ReplaceUpstreamResult{}, nil
		},
	})

	cases := []struct{ name, body string }{
		{"bad json", `{`},
		{"missing replace_with", `{"dry_run":true}`},
		{"country without code", `{"replace_with":"country"}`},
		{"asn without numbers", `{"replace_with":"asn"}`},
		{"unknown replace_with", `{"replace_with":"city","country_code":"JP"}`},
	}
	for _, c := range cases {
		rr := postReplace(t, h, "abc", c.body)
		if rr.Code != 400 {
			t.Errorf("%s: status %d, want 400", c.name, rr.Code)
		}
	}
	if called {
		t.Error("ReplaceUpstream closure should not be reached for invalid input")
	}
}

func TestReplaceUpstream_SuccessDryRun(t *testing.T) {
	var gotIn api.ReplaceUpstreamInput
	h := replaceServer(api.Deps{
		ReplaceUpstream: func(_ context.Context, id string, in api.ReplaceUpstreamInput) (*api.ReplaceUpstreamResult, error) {
			gotIn = in
			return &api.ReplaceUpstreamResult{DryRun: in.DryRun, State: "completed", ProxiesRemoved: 1, ProxiesAdded: 1}, nil
		},
	})
	rr := postReplace(t, h, "u1", `{"replace_with":"country","country_code":"JP","dry_run":true}`)
	if rr.Code != 200 {
		t.Fatalf("status %d want 200: %s", rr.Code, rr.Body.String())
	}
	if gotIn.ReplaceWith != "country" || gotIn.CountryCode != "JP" || !gotIn.DryRun {
		t.Errorf("input not forwarded: %+v", gotIn)
	}
	var out api.ReplaceUpstreamResult
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if !out.DryRun || out.ProxiesAdded != 1 {
		t.Errorf("result wrong: %+v", out)
	}
}

func TestReplaceUpstream_ErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"not found", repo.ErrNotFound, 404},
		{"not webshare", api.ErrNotWebshareUpstream, 400},
		{"webshare api error", errString("webshare: status 400: quota exceeded"), 502},
	}
	for _, c := range cases {
		h := replaceServer(api.Deps{
			ReplaceUpstream: func(context.Context, string, api.ReplaceUpstreamInput) (*api.ReplaceUpstreamResult, error) {
				return nil, c.err
			},
		})
		rr := postReplace(t, h, "u1", `{"replace_with":"asn","asn_numbers":[3356]}`)
		if rr.Code != c.want {
			t.Errorf("%s: status %d, want %d", c.name, rr.Code, c.want)
		}
	}
}

func TestReplaceOptions_ReturnsList(t *testing.T) {
	h := replaceServer(api.Deps{
		WebshareReplaceOptions: func(_ context.Context, keyID int64) (*api.ReplaceOptions, error) {
			if keyID != 7 {
				t.Errorf("keyID = %d want 7", keyID)
			}
			return &api.ReplaceOptions{
				Countries: []api.ReplaceOptionCountry{{Code: "JP", Available: 757}},
				ASNs:      []api.ReplaceOptionASN{{Number: 3356, Name: "Level3", Available: 1510}},
			}, nil
		},
	})
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/keys/7/replace-options", nil))
	if rr.Code != 200 {
		t.Fatalf("status %d want 200", rr.Code)
	}
	var out api.ReplaceOptions
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Countries) != 1 || out.Countries[0].Code != "JP" || len(out.ASNs) != 1 || out.ASNs[0].Number != 3356 {
		t.Errorf("options wrong: %+v", out)
	}
}

// errString is a tiny error type so the mapping test can supply an opaque
// webshare-style error (which should map to 502).
type errString string

func (e errString) Error() string { return string(e) }
