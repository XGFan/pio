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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
