// Package webshare is a thin client for the webshare.io v2 proxy-list API.
//
// The daemon's sync loop calls Client.ListProxies once per API key to pull
// every upstream proxy that key has access to. The client handles
// pagination via the API's "next" cursor and retries transient failures
// (5xx, network errors) with exponential backoff. 401 responses surface
// as ErrUnauthorized so the sync loop can mark the key inactive in the
// UI rather than retry forever.
package webshare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// DefaultBaseURL is the production webshare API root.
const DefaultBaseURL = "https://proxy.webshare.io"

// initialListPath is the first request URL relative to BaseURL.
const initialListPath = "/api/v2/proxy/list/?mode=direct&page_size=100"

// ErrUnauthorized is returned by ListProxies when the API key is rejected
// (HTTP 401). The sync loop should mark the ApiKey row as inactive and
// surface the failure in the UI instead of retrying.
var ErrUnauthorized = errors.New("webshare: unauthorized (401)")

// Client is a webshare API client bound to a single API key. It is safe
// for concurrent use.
type Client struct {
	apiKey  string
	baseURL string
	http    *http.Client
	// retryDelays controls retry backoff. The default schedule (1s, 3s, 9s)
	// is set in New; tests inject a shorter schedule via newClientForTest.
	retryDelays []time.Duration
}

// New returns a Client. If httpClient is nil, http.DefaultClient is used.
func New(apiKey string, httpClient *http.Client) *Client {
	c := &Client{
		apiKey:      apiKey,
		baseURL:     DefaultBaseURL,
		http:        httpClient,
		retryDelays: []time.Duration{time.Second, 3 * time.Second, 9 * time.Second},
	}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	return c
}

// NewForTest is the same as New but overrides the base URL. Integration
// tests use it to point a real Client at an httptest.Server. Production
// code MUST NOT call this.
func NewForTest(apiKey string, httpClient *http.Client, baseURL string) *Client {
	c := New(apiKey, httpClient)
	c.baseURL = baseURL
	return c
}

// Proxy is one upstream proxy as reported by webshare. Fields are the
// subset the sync loop needs; the API returns more we ignore. CreatedAt
// is a *time.Time so its zero state survives JSON marshal round-trips
// (json's "omitempty" does nothing for nested struct fields).
type Proxy struct {
	ProxyAddress string     `json:"proxy_address"`
	Port         int        `json:"port"`
	Username     string     `json:"username"`
	Password     string     `json:"password"`
	CountryCode  string     `json:"country_code"`
	CityName     string     `json:"city_name,omitempty"`
	CreatedAt    *time.Time `json:"created_at,omitempty"`
}

// listPage is the on-wire shape of one paginated response.
type listPage struct {
	Next    *string `json:"next"`
	Results []Proxy `json:"results"`
}

// --- Proxy replacement (by country / by ASN) -------------------------------

// ReplaceTarget is the `to_replace` selector. Exactly one shape is used:
// by IP address (the common per-proxy case), by country, or by ASN.
type ReplaceTarget struct {
	Type        string   `json:"type"` // "ip_address" | "country" | "asn"
	IPAddresses []string `json:"ip_addresses,omitempty"`
	CountryCode string   `json:"country_code,omitempty"`
	ASNNumbers  []int    `json:"asn_numbers,omitempty"`
}

// ReplaceWith is one entry of the `replace_with` array: where the replacement
// proxy should come from. The API accepts "country" or "asn".
type ReplaceWith struct {
	Type        string `json:"type"` // "country" | "asn"
	CountryCode string `json:"country_code,omitempty"`
	ASNNumbers  []int  `json:"asn_numbers,omitempty"`
}

// ReplaceRequest is the POST /api/v2/proxy/replace/ body.
type ReplaceRequest struct {
	ToReplace   ReplaceTarget `json:"to_replace"`
	ReplaceWith []ReplaceWith `json:"replace_with"`
	DryRun      bool          `json:"dry_run"`
}

// ReplaceResult is the subset of the replacement response we surface. The
// operation is synchronous: state is "completed" on the response.
type ReplaceResult struct {
	ID             int64  `json:"id"`
	State          string `json:"state"`
	DryRun         bool   `json:"dry_run"`
	ProxiesRemoved int    `json:"proxies_removed"`
	ProxiesAdded   int    `json:"proxies_added"`
}

// Replace requests a proxy replacement. With DryRun=true the API simulates
// the change (no quota consumed, no proxies actually swapped) and still
// reports proxies_removed/added, which the caller can show as a preview.
//
// A real replacement is NON-IDEMPOTENT and billable: it is sent with NO retry,
// because a 5xx after the server already applied the change would, on retry,
// replace a second proxy and burn another replacement. The caller surfaces a
// transient failure to the operator, who can re-check and retry deliberately.
func (c *Client) Replace(ctx context.Context, req ReplaceRequest) (*ReplaceResult, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal replace request: %w", err)
	}
	var out ReplaceResult
	if _, err := c.doJSONOnce(ctx, http.MethodPost, c.baseURL+"/api/v2/proxy/replace/", b, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ASN is one autonomous-system option for the replacement UI.
type ASN struct {
	Number    int    `json:"number"`
	Name      string `json:"name"`
	Available int    `json:"available"`
}

// Config is the subset of GET /api/v2/proxy/config/ used to populate the
// replacement UI's country/ASN dropdowns. AvailableCountries maps a country
// code to how many proxies are available there; AvailableASNs is the ASN
// equivalent (the raw API encodes each as [name, count]).
type Config struct {
	AvailableCountries map[string]int
	AvailableASNs      []ASN
}

// rawConfig mirrors the wire shape we decode before flattening into Config.
// available_asns is {"3356":["Level3",1510], ...}; the value is a 2-tuple of
// (name, available-count) decoded loosely so a schema tweak can't panic.
type rawConfig struct {
	AvailableCountries map[string]int             `json:"available_countries"`
	AvailableASNs      map[string][]json.RawMessage `json:"available_asns"`
}

// GetConfig fetches the account's proxy config and returns the available
// countries and ASNs for the replacement UI.
func (c *Client) GetConfig(ctx context.Context) (*Config, error) {
	var raw rawConfig
	if err := c.getJSON(ctx, "/api/v2/proxy/config/", &raw); err != nil {
		return nil, err
	}
	cfg := &Config{AvailableCountries: raw.AvailableCountries}
	for num, tuple := range raw.AvailableASNs {
		n, err := strconv.Atoi(num)
		if err != nil || len(tuple) < 2 {
			continue
		}
		var name string
		var avail int
		_ = json.Unmarshal(tuple[0], &name)
		_ = json.Unmarshal(tuple[1], &avail)
		cfg.AvailableASNs = append(cfg.AvailableASNs, ASN{Number: n, Name: name, Available: avail})
	}
	sort.Slice(cfg.AvailableASNs, func(i, j int) bool {
		return cfg.AvailableASNs[i].Name < cfg.AvailableASNs[j].Name
	})
	return cfg, nil
}

// ListProxies fetches every upstream proxy reachable via this client's API
// key, following the API's pagination cursor until exhausted. The returned
// slice is the concatenation of all pages in API order.
func (c *Client) ListProxies(ctx context.Context) ([]Proxy, error) {
	out := make([]Proxy, 0, 100)
	url := c.baseURL + initialListPath
	for {
		page, err := c.fetchWithRetry(ctx, url)
		if err != nil {
			return nil, err
		}
		out = append(out, page.Results...)
		if page.Next == nil || *page.Next == "" {
			break
		}
		url = *page.Next
	}
	return out, nil
}

// fetchWithRetry does one paginated request, retrying transient failures
// per c.retryDelays. Returns the parsed page or the last error encountered.
//
// Attempt budget: one initial attempt + len(retryDelays) retries.
// With the default schedule (1s, 3s, 9s) that's up to 4 HTTP calls
// per page: try, sleep 1s, retry, sleep 3s, retry, sleep 9s, retry.
func (c *Client) fetchWithRetry(ctx context.Context, url string) (*listPage, error) {
	attempts := len(c.retryDelays) + 1
	var lastErr error
	for i := range attempts {
		if i > 0 {
			delay := c.retryDelays[i-1]
			t := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil, ctx.Err()
			case <-t.C:
			}
		}
		page, retry, err := c.fetchOnce(ctx, url)
		if err == nil {
			return page, nil
		}
		lastErr = err
		if !retry {
			return nil, err
		}
	}
	return nil, fmt.Errorf("webshare: exhausted retries: %w", lastErr)
}

// fetchOnce does one HTTP attempt. The retry bool tells fetchWithRetry
// whether the error class (network or 5xx) is worth backing off and
// trying again. 401 / 4xx / parse errors return retry=false.
func (c *Client) fetchOnce(ctx context.Context, url string) (page *listPage, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Network-level failure (DNS, refused, TLS, timeout) is retryable.
		// ctx cancellation surfaces here too; the next loop iteration will
		// observe ctx.Done() during the backoff timer.
		return nil, true, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, false, ErrUnauthorized
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, true, fmt.Errorf("webshare: status %d: %s", resp.StatusCode, body)
	case resp.StatusCode >= 400:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, false, fmt.Errorf("webshare: status %d: %s", resp.StatusCode, body)
	case resp.StatusCode != http.StatusOK:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, false, fmt.Errorf("webshare: unexpected status %d: %s", resp.StatusCode, body)
	}

	var p listPage
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return nil, false, fmt.Errorf("decode page: %w", err)
	}
	return &p, false, nil
}

// doJSON performs method+path with an optional JSON body, retrying transient
// failures (network, 5xx) per c.retryDelays, and decodes any 2xx response into
// out (when non-nil). 401 → ErrUnauthorized; other 4xx surface the body so the
// caller can show the API's validation message.
func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal body: %w", err)
		}
		bodyBytes = b
	}
	url := c.baseURL + path
	attempts := len(c.retryDelays) + 1
	var lastErr error
	for i := range attempts {
		if i > 0 {
			t := time.NewTimer(c.retryDelays[i-1])
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
		retry, err := c.doJSONOnce(ctx, method, url, bodyBytes, out)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry {
			return err
		}
	}
	return fmt.Errorf("webshare: exhausted retries: %w", lastErr)
}

func (c *Client) doJSONOnce(ctx context.Context, method, url string, body []byte, out any) (retry bool, err error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return true, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return false, ErrUnauthorized
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return true, fmt.Errorf("webshare: status %d: %s", resp.StatusCode, b)
	case resp.StatusCode >= 400:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("webshare: status %d: %s", resp.StatusCode, b)
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("webshare: unexpected status %d: %s", resp.StatusCode, b)
	}

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return false, fmt.Errorf("decode response: %w", err)
		}
	}
	return false, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, nil, out)
}
