package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/guofan/pia/internal/sync"
	"github.com/guofan/pia/internal/webshare"
)

// fixtureProxies is the canned data the mock webshare API serves across
// two pages so the integration test exercises pagination too.
var fixtureProxies = []webshare.Proxy{
	{ProxyAddress: "1.1.1.1", Port: 1080, Username: "u1", Password: "p1", CountryCode: "US"},
	{ProxyAddress: "2.2.2.2", Port: 1080, Username: "u2", Password: "p2", CountryCode: "US"},
	{ProxyAddress: "3.3.3.3", Port: 1080, Username: "u3", Password: "p3", CountryCode: "DE"},
	{ProxyAddress: "4.4.4.4", Port: 1080, Username: "u4", Password: "p4", CountryCode: "JP"},
}

// mockWebshareServer serves the fixture across two pages so the test
// exercises Client pagination. Page 1 returns the first two rows; "next"
// points at /page2 which returns the remaining two.
func mockWebshareServer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	mux := http.NewServeMux()
	calls := atomic.Int32{}
	mux.HandleFunc("/api/v2/proxy/list/", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		page := map[string]any{
			"next":    srv.URL + "/page2",
			"results": fixtureProxies[:2],
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("/page2", func(w http.ResponseWriter, r *http.Request) {
		page := map[string]any{
			"next":    nil,
			"results": fixtureProxies[2:],
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runCLI is a tiny test helper that invokes Run with capturing buffers.
// Returns (exitCode, stdout, stderr).
func runCLI(t *testing.T, deps Deps, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	deps.Stdout = &stdout
	deps.Stderr = &stderr
	code := Run(context.Background(), args, deps)
	return code, stdout.String(), stderr.String()
}

func TestAC1_AddKeyThenSyncIntegration(t *testing.T) {
	srv := mockWebshareServer(t)

	// Wire the FetcherFactory the CLI will hand to sync.NewService. The
	// factory builds a real webshare.Client (which already satisfies
	// sync.Fetcher) and rewires its base URL to the httptest server, so
	// the AC#1 test exercises real pagination + retry against the mock.
	deps := Deps{
		FetcherFactory: func(apiKey string) sync.Fetcher {
			return webshare.NewForTest(apiKey, srv.Client(), srv.URL)
		},
	}

	dir := t.TempDir()

	// add-key
	code, out, errOut := runCLI(t, deps, "add-key", "--label=US Premium", "--key=sk_live_test", "--data-dir="+dir)
	if code != 0 {
		t.Fatalf("add-key exit=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "added api_key id=1") {
		t.Errorf("add-key stdout = %q want contains 'added api_key id=1'", out)
	}

	// sync
	code, out, errOut = runCLI(t, deps, "sync", "--key-id=1", "--data-dir="+dir)
	if code != 0 {
		t.Fatalf("sync exit=%d stderr=%q", code, errOut)
	}
	if !strings.Contains(out, "sync ok key_id=1") {
		t.Errorf("sync stdout = %q", out)
	}

	// AC#1 verification: row count + country codes in SQLite.
	verifyAC1(t, filepath.Join(dir, DataDBName))
}

func TestSyncBadKeyIDReturnsExitCode1(t *testing.T) {
	deps := Deps{
		FetcherFactory: func(apiKey string) sync.Fetcher {
			return webshare.NewForTest(apiKey, nil, "http://example.invalid")
		},
	}
	dir := t.TempDir()

	code, _, errOut := runCLI(t, deps, "sync", "--key-id=999", "--data-dir="+dir)
	if code != 1 {
		t.Fatalf("expected exit 1 for unknown key, got %d (stderr=%q)", code, errOut)
	}
	if !strings.Contains(errOut, "no api_key") {
		t.Errorf("stderr = %q want mention of missing key", errOut)
	}
}

func TestUsageErrorsReturnExit2(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no args", []string{}},
		{"unknown command", []string{"frobnicate"}},
		{"sync without key-id", []string{"sync", "--data-dir=/tmp"}},
		{"add-key missing label", []string{"add-key", "--key=x"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, _ := runCLI(t, Deps{}, tc.args...)
			if code != 2 {
				t.Errorf("exit = %d want 2", code)
			}
		})
	}
}

func TestVersionPrints(t *testing.T) {
	code, out, _ := runCLI(t, Deps{}, "version")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, Version) {
		t.Errorf("stdout = %q does not contain version %q", out, Version)
	}
}

// verifyAC1 opens data.db directly and checks the row count + country codes
// match the fixture. This is the canonical AC#1 assertion: "after sync,
// SQLite has N rows with correct country codes."
func verifyAC1(t *testing.T, dbPath string) {
	t.Helper()
	// Reuse the same store.Open so the test sees the migrated schema.
	ctx := context.Background()
	db, err := storeOpen(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT host, port, country_code FROM upstream_proxies ORDER BY host`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := []string{}
	for rows.Next() {
		var host, cc string
		var port int
		if err := rows.Scan(&host, &port, &cc); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%s:%d/%s", host, port, cc))
	}
	want := []string{
		"1.1.1.1:1080/US",
		"2.2.2.2:1080/US",
		"3.3.3.3:1080/DE",
		"4.4.4.4:1080/JP",
	}
	if len(got) != len(want) {
		t.Fatalf("row count = %d, want %d (rows=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %q want %q", i, got[i], want[i])
		}
	}

	// ApiKey.LastSyncedAt set?
	var lastSynced *string
	if err := db.QueryRowContext(ctx, `SELECT last_synced_at FROM api_keys WHERE id=1`).Scan(&lastSynced); err != nil {
		t.Fatal(err)
	}
	if lastSynced == nil || *lastSynced == "" {
		t.Errorf("last_synced_at not set after successful sync")
	}
}
