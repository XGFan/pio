package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/guofan/pia/internal/repo"
)

// ErrNotWebshareUpstream is returned by the ReplaceUpstream closure when the
// targeted upstream is not a webshare row (manual proxies can't be replaced
// via the Webshare API). The handler maps it to 400.
var ErrNotWebshareUpstream = errors.New("api: upstream is not a webshare proxy")

// ReplaceUpstreamInput is the POST /api/v1/upstreams/{id}/replace body.
type ReplaceUpstreamInput struct {
	ReplaceWith string `json:"replace_with"` // "country" | "asn"
	CountryCode string `json:"country_code"`
	ASNNumbers  []int  `json:"asn_numbers"`
	DryRun      bool   `json:"dry_run"`
}

// ReplaceUpstreamResult is the response. For a dry run it is a preview; for a
// real run the key has already been re-synced by the time it is returned.
type ReplaceUpstreamResult struct {
	DryRun         bool   `json:"dry_run"`
	State          string `json:"state"`
	ProxiesRemoved int    `json:"proxies_removed"`
	ProxiesAdded   int    `json:"proxies_added"`
}

// ReplaceOptions is the GET /api/v1/keys/{id}/replace-options response — the
// countries and ASNs available to source replacement proxies from.
type ReplaceOptions struct {
	Countries []ReplaceOptionCountry `json:"countries"`
	ASNs      []ReplaceOptionASN     `json:"asns"`
}

type ReplaceOptionCountry struct {
	Code      string `json:"code"`
	Available int    `json:"available"`
}

type ReplaceOptionASN struct {
	Number    int    `json:"number"`
	Name      string `json:"name"`
	Available int    `json:"available"`
}

// replaceUpstream replaces one webshare proxy, sourcing the new proxy from a
// country or ASN. dry_run previews without consuming a replacement.
func (s *Server) replaceUpstream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in ReplaceUpstreamInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	switch in.ReplaceWith {
	case "country":
		if in.CountryCode == "" {
			writeErr(w, 400, "country_code required when replace_with=country")
			return
		}
	case "asn":
		if len(in.ASNNumbers) == 0 {
			writeErr(w, 400, "asn_numbers required when replace_with=asn")
			return
		}
	default:
		writeErr(w, 400, "replace_with must be \"country\" or \"asn\"")
		return
	}
	if s.deps.ReplaceUpstream == nil {
		writeErr(w, 500, "replace not configured")
		return
	}
	res, err := s.deps.ReplaceUpstream(r.Context(), id, in)
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound):
			writeErr(w, 404, "upstream not found")
		case errors.Is(err, ErrNotWebshareUpstream):
			writeErr(w, 400, "only webshare proxies can be replaced")
		default:
			// Webshare API / network error.
			writeErr(w, 502, err.Error())
		}
		return
	}
	writeJSON(w, 200, res)
}

// replaceOptions returns the available countries/ASNs for a key's account.
func (s *Server) replaceOptions(w http.ResponseWriter, r *http.Request) {
	keyID, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad key id")
		return
	}
	if s.deps.WebshareReplaceOptions == nil {
		writeErr(w, 500, "replace options not configured")
		return
	}
	opts, err := s.deps.WebshareReplaceOptions(r.Context(), keyID)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, opts)
}
