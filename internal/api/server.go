// Package api is the JSON+WS REST surface the SwiftUI app talks to. The
// server binds to 127.0.0.1:<random> and writes the actual port to the
// data-dir's api.port file so the SwiftUI app can find it.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/guofan/webshare-proxy/internal/auth"
	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/model"
	"github.com/guofan/webshare-proxy/internal/registry"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/routing"
	"github.com/guofan/webshare-proxy/internal/ws"
)

// Deps is the bag of dependencies the API server reads at every request.
type Deps struct {
	DB        *sql.DB
	MasterKey []byte
	Core      *routing.Core
	Registry  *registry.ConnectionRegistry
	Hub       *ws.Hub
	DenyList  *auth.DenyList
	DataDir   string
	// SyncKey is called by POST /api/v1/keys/:id/sync. Provided as a
	// function pointer so api doesn't import sync directly.
	SyncKey      func(ctx context.Context, keyID int64) error
	// ReconfigureListeners is called after settings PUT so the HTTP/SOCKS5
	// listeners can rebind to any changed host:port. Provided as a function
	// pointer so api stays decoupled from the listener package.
	ReconfigureListeners func() error
	// StartProxy/StopProxy/ProxyStatus expose the listener state machine to
	// the REST layer. Wired in by cli/run.go so the api package doesn't
	// depend on listener internals.
	StartProxy  func() error
	StopProxy   func() error
	ProxyStatus func() (running bool, proxyAddr string)
	// ShutdownFn is called by POST /api/v1/shutdown.
	ShutdownFn   func()
	// ReplaceUpstream replaces a single webshare upstream (by its IP), sourcing
	// the new proxy from a country or ASN. On a non-dry-run it has already
	// re-synced the key + rebuilt routing by the time it returns. Wired in
	// cli/run.go so api stays decoupled from sync/webshare.
	ReplaceUpstream func(ctx context.Context, upstreamID string, in ReplaceUpstreamInput) (*ReplaceUpstreamResult, error)
	// WebshareReplaceOptions returns the available countries/ASNs for a key.
	WebshareReplaceOptions func(ctx context.Context, keyID int64) (*ReplaceOptions, error)
	// TestAllLatency probes every upstream's latency (batched) and persists
	// the results. Wired in cli/run.go.
	TestAllLatency func(ctx context.Context) ([]LatencyResult, error)
	// PasswordPeek is rate-limited (1/sec/IP) by the server itself.
	now func() time.Time
}

// Server hosts the chi router on a 127.0.0.1 listener of its choosing.
type Server struct {
	deps Deps
	ln   net.Listener
	srv  *http.Server
	port int

	// pwPeek tracks the last password-reveal call per IP for the
	// 1-req/sec limiter.
	pwPeekMu sync.Mutex
	pwPeek   map[string]time.Time
}

// New wires a server. Bind is the listen address; "127.0.0.1:0" picks a
// random port at Bind() time.
func New(deps Deps) *Server {
	if deps.now == nil {
		deps.now = time.Now
	}
	return &Server{deps: deps, pwPeek: map[string]time.Time{}}
}

// Bind opens the listener but does not start serving. Returns the picked port.
func (s *Server) Bind(addr string) (int, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return 0, err
	}
	s.ln = ln
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	s.port = port
	return port, nil
}

// Port returns the actually-bound port.
func (s *Server) Port() int { return s.port }

// WriteAPIPortFile writes <dataDir>/api.port containing the port number.
func (s *Server) WriteAPIPortFile() error {
	if s.deps.DataDir == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(s.deps.DataDir, "api.port"),
		[]byte(strconv.Itoa(s.port)+"\n"), 0o600)
}

// Handler returns a chi.Router with /api/v1/* mounted. Exported so a
// second listener (e.g. the LAN web UI) can reuse the same handler set
// behind its own auth middleware.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	s.mountRoutes(r)
	return r
}

// Serve runs the chi handler until ctx is cancelled or Shutdown is called.
func (s *Server) Serve(ctx context.Context) error {
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shctx)
	}()
	if err := s.srv.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown stops the HTTP server gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) mountRoutes(r chi.Router) {
	r.Get("/api/v1/keys", s.listKeys)
	r.Post("/api/v1/keys", s.addKey)
	r.Delete("/api/v1/keys/{id}", s.deleteKey)
	r.Post("/api/v1/keys/{id}/sync", s.syncKey)

	r.Get("/api/v1/upstreams", s.listUpstreams)
	r.Patch("/api/v1/upstreams/{id}", s.patchUpstream)
	r.Post("/api/v1/upstreams/{id}/replace", s.replaceUpstream)
	r.Post("/api/v1/upstreams/test-latency", s.testLatency)
	r.Get("/api/v1/keys/{id}/replace-options", s.replaceOptions)

	r.Get("/api/v1/manual-proxies", s.listManualProxies)
	r.Post("/api/v1/manual-proxies", s.addManualProxy)
	r.Patch("/api/v1/manual-proxies/{id}", s.patchManualProxy)
	r.Delete("/api/v1/manual-proxies/{id}", s.deleteManualProxy)

	r.Get("/api/v1/users", s.listUsers)
	r.Post("/api/v1/users", s.addUser)
	r.Post("/api/v1/users/reorder", s.reorderUsers)
	r.Patch("/api/v1/users/{username}", s.patchUser)
	r.Delete("/api/v1/users/{username}", s.deleteUser)
	r.Get("/api/v1/users/{username}/password", s.peekPassword)

	r.Get("/api/v1/settings", s.getSettings)
	r.Put("/api/v1/settings", s.putSettings)
	r.Put("/api/v1/settings/universal-password", s.putUniversalPassword)
	r.Get("/api/v1/subscription-url", s.subscriptionURLHandler)

	// Public (query-param-auth only) subscription endpoint. Mounted here so it
	// works on the loopback API server; the LAN web server mounts the same
	// handler OUTSIDE its cookie auth via Server.SubscriptionHandler().
	r.Get("/subscription", s.subscriptionHandler)

	r.Get("/api/v1/proxy/status", s.proxyStatusHandler)
	r.Post("/api/v1/proxy/start", s.proxyStartHandler)
	r.Post("/api/v1/proxy/stop", s.proxyStopHandler)

	r.Get("/api/v1/audit", s.getAudit)
	r.Get("/api/v1/connections", s.listConnections)

	r.Post("/api/v1/shutdown", s.shutdown)
	r.Get("/api/v1/events", s.deps.Hub.ServeHTTP)
}

// --- handlers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

type apiKeyDTO struct {
	ID            int64      `json:"id"`
	Label         string     `json:"label"`
	AddedAt       time.Time  `json:"added_at"`
	LastSyncedAt  *time.Time `json:"last_synced_at,omitempty"`
	LastSyncError string     `json:"last_sync_error,omitempty"`
	Active        bool       `json:"active"`
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := repo.ListApiKeys(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]apiKeyDTO, 0, len(keys))
	for _, k := range keys {
		out = append(out, apiKeyDTO{
			ID: k.ID, Label: k.Label, AddedAt: k.AddedAt,
			LastSyncedAt: k.LastSyncedAt, LastSyncError: k.LastSyncError,
			Active: k.Active,
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) addKey(w http.ResponseWriter, r *http.Request) {
	var in struct{ Label, APIKey string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if in.Label == "" || in.APIKey == "" {
		writeErr(w, 400, "label and api_key required")
		return
	}
	id, err := repo.InsertApiKey(r.Context(), s.deps.DB, s.deps.MasterKey, in.Label, in.APIKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, map[string]int64{"id": id})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := repo.DeleteApiKey(r.Context(), s.deps.DB, id); err != nil {
		var inUse *repo.ErrKeyInUse
		if errors.As(err, &inUse) {
			writeJSON(w, 409, map[string]any{
				"error":              "key_in_use",
				"referencing_users":  inUse.ReferencingUsers,
			})
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

func (s *Server) syncKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if s.deps.SyncKey == nil {
		writeErr(w, 500, "sync not configured")
		return
	}
	if err := s.deps.SyncKey(r.Context(), id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Re-hydrate routing so the swap reflects the new upstream set.
	if err := s.deps.Core.RebuildAfterSync(r.Context(), func(u, oldUp string) {
		s.deps.Registry.CloseByUserUpstream(u, oldUp)
		s.deps.Hub.Broadcast("mapping_broken", map[string]any{"username": u, "old_upstream_id": oldUp})
	}); err != nil {
		writeErr(w, 500, "rebuild: "+err.Error())
		return
	}
	s.deps.Hub.Broadcast("sync_completed", map[string]any{"key_id": id})
	writeJSON(w, 200, map[string]any{"synced": id})
}

type upstreamDTO struct {
	ID              string    `json:"id"`
	Source          string    `json:"source"`
	SourceApiKeyID  *int64    `json:"source_api_key_id"`
	ManualName      string    `json:"manual_name,omitempty"`
	Host            string    `json:"host"`
	Port            int       `json:"port"`
	Username        string    `json:"username,omitempty"`
	Protocol        string    `json:"protocol"`
	DisplayName     string    `json:"display_name"`
	CountryCode     string    `json:"country_code"`
	CityName        string    `json:"city_name,omitempty"`
	RecentlyFailing bool       `json:"recently_failing"`
	LastSeenAt      time.Time  `json:"last_seen_at"`
	// LastLatencyMS: omitted when never tested, -1 when the last probe failed,
	// >=0 milliseconds otherwise. LastLatencyAt is the probe time.
	LastLatencyMS *int       `json:"last_latency_ms,omitempty"`
	LastLatencyAt *time.Time `json:"last_latency_at,omitempty"`
}

func toUpstreamDTO(u model.UpstreamProxy) upstreamDTO {
	return upstreamDTO{
		ID: u.ID, Source: u.Source, SourceApiKeyID: u.SourceApiKeyID,
		ManualName: u.ManualName, Host: u.Host, Port: u.Port, Username: u.Username,
		Protocol: u.Protocol, DisplayName: u.DisplayName, CountryCode: u.CountryCode,
		CityName: u.CityName, RecentlyFailing: u.RecentlyFailing,
		LastSeenAt: u.LastSeenAt, LastLatencyMS: u.LastLatencyMS, LastLatencyAt: u.LastLatencyAt,
	}
}

func (s *Server) listUpstreams(w http.ResponseWriter, r *http.Request) {
	rows, err := repo.ListUpstreams(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]upstreamDTO, 0, len(rows))
	for _, u := range rows {
		// The built-in direct upstream is an internal routing pattern, not a
		// managed proxy: it is reachable by name (universal-password path /
		// subscription) but is intentionally hidden from the admin UI listing.
		if u.Source == repo.SourceDirect {
			continue
		}
		out = append(out, toUpstreamDTO(u))
	}
	writeJSON(w, 200, out)
}

// manualProxyInput is the wire shape for POST/PATCH /api/v1/manual-proxies.
// All fields are required on POST; PATCH treats empty Password as "keep".
type manualProxyInput struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) listManualProxies(w http.ResponseWriter, r *http.Request) {
	rows, err := repo.ListManualProxies(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]upstreamDTO, 0, len(rows))
	for _, u := range rows {
		out = append(out, toUpstreamDTO(u))
	}
	writeJSON(w, 200, out)
}

func (s *Server) addManualProxy(w http.ResponseWriter, r *http.Request) {
	var in manualProxyInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	id, err := repo.InsertManualProxy(r.Context(), s.deps.DB, s.deps.MasterKey, repo.ManualProxyInput{
		Name: in.Name, Host: in.Host, Port: in.Port, Protocol: in.Protocol,
		Username: in.Username, Password: in.Password,
	})
	if err != nil {
		if errors.Is(err, repo.ErrManualNameInUse) {
			writeJSON(w, 409, map[string]string{"error": "manual_name_in_use"})
			return
		}
		if errors.Is(err, repo.ErrInvalidManualProxy) {
			writeErr(w, 400, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	// Rehydrate so the new upstream is immediately routable.
	if err := s.deps.Core.RebuildAfterSync(r.Context(), func(u, oldUp string) {
		s.deps.Registry.CloseByUserUpstream(u, oldUp)
	}); err != nil {
		writeErr(w, 500, "rebuild: "+err.Error())
		return
	}
	s.deps.Hub.Broadcast("manual_proxy_added", map[string]any{"id": id})
	writeJSON(w, 201, map[string]string{"id": id})
}

func (s *Server) patchManualProxy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in manualProxyInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	err := repo.UpdateManualProxy(r.Context(), s.deps.DB, s.deps.MasterKey, id, repo.ManualProxyInput{
		Name: in.Name, Host: in.Host, Port: in.Port, Protocol: in.Protocol,
		Username: in.Username, Password: in.Password,
	})
	if err != nil {
		if errors.Is(err, repo.ErrManualNameInUse) {
			writeJSON(w, 409, map[string]string{"error": "manual_name_in_use"})
			return
		}
		if errors.Is(err, repo.ErrInvalidManualProxy) {
			writeErr(w, 400, err.Error())
			return
		}
		if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrUpstreamNotManual) {
			writeErr(w, 404, "manual proxy not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	// Edits to host/port/protocol/creds must be picked up by routing AND
	// any in-flight bridges that captured the pre-edit upstream tuple must
	// be torn down. RebuildAfterSync only rotates CancelGroups on Broken
	// transitions, so use the dedicated upstream-change helper.
	if err := s.deps.Core.RebuildForUpstreamChange(r.Context(), id, func(u, oldUp string) {
		s.deps.Registry.CloseByUserUpstream(u, oldUp)
	}); err != nil {
		writeErr(w, 500, "rebuild: "+err.Error())
		return
	}
	// Tear down any universal-password (display-name) bridges to this upstream
	// too — they aren't anchored to a local user so RebuildForUpstreamChange's
	// per-user callback can't reach them.
	s.deps.Registry.CloseByUpstream(id)
	s.deps.Hub.Broadcast("manual_proxy_updated", map[string]any{"id": id})
	writeJSON(w, 200, map[string]string{"id": id})
}

func (s *Server) deleteManualProxy(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := repo.DeleteManualProxy(r.Context(), s.deps.DB, id); err != nil {
		var inUse *repo.ErrUpstreamInUse
		if errors.As(err, &inUse) {
			writeJSON(w, 409, map[string]any{
				"error":             "upstream_in_use",
				"referencing_users": inUse.ReferencingUsers,
			})
			return
		}
		if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrUpstreamNotManual) {
			writeErr(w, 404, "manual proxy not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.deps.Core.RebuildAfterSync(r.Context(), func(u, oldUp string) {
		s.deps.Registry.CloseByUserUpstream(u, oldUp)
	}); err != nil {
		writeErr(w, 500, "rebuild: "+err.Error())
		return
	}
	// Close universal-password (display-name) bridges to the now-deleted
	// upstream; the per-user rebuild callback above can't reach them.
	s.deps.Registry.CloseByUpstream(id)
	s.deps.Hub.Broadcast("manual_proxy_deleted", map[string]any{"id": id})
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

func (s *Server) patchUpstream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct{ DisplayName string `json:"display_name"` }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	// Manual rows keep display_name and manual_name in sync by going through
	// /api/v1/manual-proxies. Refuse the generic display-name endpoint for
	// them so the two fields can never drift.
	cur, err := repo.GetUpstream(r.Context(), s.deps.DB, id)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			writeErr(w, 404, "upstream not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if cur.Source == repo.SourceManual {
		writeErr(w, 400, "manual upstream: use PATCH /api/v1/manual-proxies/{id}")
		return
	}
	// The built-in direct upstream's display name is the routing contract
	// (universal-password + subscription key); keep it immutable.
	if cur.Source == repo.SourceDirect {
		writeErr(w, 400, "built-in direct upstream cannot be edited")
		return
	}
	if err := repo.UpdateUpstreamDisplayName(r.Context(), s.deps.DB, id, in.DisplayName); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			writeErr(w, 404, "upstream not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"id": id, "display_name": in.DisplayName})
}

type userDTO struct {
	Username        string    `json:"username"`
	UpstreamProxyID *string   `json:"upstream_proxy_id"`
	Broken          bool      `json:"broken"`
	Notes           string    `json:"notes,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := repo.ListLocalUsers(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		out = append(out, userDTO{
			Username: u.Username, UpstreamProxyID: u.UpstreamProxyID,
			Broken: u.Broken, Notes: u.Notes, UpdatedAt: u.UpdatedAt,
		})
	}
	writeJSON(w, 200, out)
}

func (s *Server) addUser(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password, Notes string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if in.Username == "" || in.Password == "" {
		writeErr(w, 400, "username and password required")
		return
	}
	if err := repo.InsertLocalUser(r.Context(), s.deps.DB, in.Username, in.Password, in.Notes); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Rehydrate routing so the new user is immediately routable.
	_ = s.deps.Core.Hydrate(r.Context())
	writeJSON(w, 201, map[string]string{"username": in.Username})
}

func (s *Server) patchUser(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	var in struct {
		Password        *string `json:"password,omitempty"`
		UpstreamProxyID *string `json:"upstream_proxy_id,omitempty"`
		Notes           *string `json:"notes,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if in.Password != nil {
		if err := repo.UpdateLocalUserPassword(r.Context(), s.deps.DB, username, *in.Password); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		_ = s.deps.Core.Hydrate(r.Context())
	}
	if in.UpstreamProxyID != nil {
		newID := ""
		if *in.UpstreamProxyID != "" {
			newID = *in.UpstreamProxyID
		}
		// Use hot-switch path so existing conns under the old mapping are torn down.
		err := s.deps.Core.SwapUserMapping(r.Context(), username, newID, func(old *routing.ResolvedUser) {
			closed := s.deps.Registry.CloseByUserUpstream(old.Username, old.UpstreamID)
			s.deps.Hub.Broadcast("mapping_changed", map[string]any{
				"username": username, "old_upstream_id": old.UpstreamID,
				"new_upstream_id": newID, "closed_connections": closed,
			})
		})
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]string{"username": username})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	if err := repo.DeleteLocalUser(r.Context(), s.deps.DB, username); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	closed := s.deps.Registry.CloseByUsername(username)
	_ = s.deps.Core.Hydrate(r.Context())
	writeJSON(w, 200, map[string]any{"deleted": username, "closed_connections": closed})
}

// peekPassword reveals a user's plaintext password. Rate-limited to one
// call per second per client IP (host-only — a flapping ephemeral port
// must not bypass the limit).
func (s *Server) peekPassword(w http.ResponseWriter, r *http.Request) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		ip = r.RemoteAddr
	}
	s.pwPeekMu.Lock()
	last, ok := s.pwPeek[ip]
	now := s.deps.now()
	if ok && now.Sub(last) < time.Second {
		s.pwPeekMu.Unlock()
		writeErr(w, 429, "rate limited")
		return
	}
	s.pwPeek[ip] = now
	s.pwPeekMu.Unlock()

	username := chi.URLParam(r, "username")
	u, err := repo.GetLocalUser(r.Context(), s.deps.DB, username)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			writeErr(w, 404, "not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"username": username, "password": u.PasswordPlain})
}

// settingsDTO is the snake_case wire shape. We use it for both /settings
// GET (encode) and PUT (decode) so the SwiftUI app and curl scripts see a
// consistent surface — model.Settings has no JSON tags and would otherwise
// emit PascalCase field names.
type settingsDTO struct {
	SyncIntervalMinutes int    `json:"sync_interval_minutes"`
	// ProxyPort / ProxyBind configure the single unified proxy listener that
	// serves both HTTP and SOCKS5 on one port.
	ProxyPort    int    `json:"proxy_port"`
	ProxyBind    string `json:"proxy_bind"`
	ProxyEnabled bool   `json:"proxy_enabled"`
	// Subscription controls the public /subscription endpoint.
	SubscriptionEnabled bool   `json:"subscription_enabled"`
	SubscriptionHost    string `json:"subscription_host"`
	// UniversalProxyPasswordSet is read-only (GET): whether a universal proxy
	// password is configured. The value itself is never returned. Set/clear it
	// via PUT /api/v1/settings/universal-password.
	UniversalProxyPasswordSet bool `json:"universal_proxy_password_set"`
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	st, err := repo.LoadSettings(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	hasUniversal, err := repo.HasUniversalProxyPassword(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, settingsDTO{
		SyncIntervalMinutes:       st.SyncIntervalMinutes,
		ProxyPort:                 st.ProxyPort,
		ProxyBind:                 st.ProxyBind,
		ProxyEnabled:              st.ProxyEnabled,
		SubscriptionEnabled:       st.SubscriptionEnabled,
		SubscriptionHost:          st.SubscriptionHost,
		UniversalProxyPasswordSet: hasUniversal,
	})
}

// putUniversalPassword sets or clears the universal proxy password. An empty
// password clears it (disables the feature). The value lives on its own
// endpoint — decoupled from the bulk settings PUT — so a partial settings
// payload can never zero out the listener ports, and a port-in-use rollback
// can never revert the password. Routing is re-hydrated so the change takes
// effect immediately for new connections.
func (s *Server) putUniversalPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if err := repo.SetUniversalProxyPassword(r.Context(), s.deps.DB, s.deps.MasterKey, in.Password); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := s.deps.Core.Hydrate(r.Context()); err != nil {
		writeErr(w, 500, "rehydrate: "+err.Error())
		return
	}
	s.deps.Hub.Broadcast("settings_updated", map[string]any{"universal_proxy_password_set": in.Password != ""})
	writeJSON(w, 200, map[string]bool{"universal_proxy_password_set": in.Password != ""})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var st = settingsDTO{}
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	cur, err := repo.LoadSettings(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Snapshot the previous listener-affecting fields BEFORE the update —
	// if Reconfigure fails because the new port is in use we restore them
	// so the persisted state matches what the kernel still owns. Required
	// by "新端口被占用 → 旧端口不释放".
	prev := cur
	cur.SyncIntervalMinutes = st.SyncIntervalMinutes
	cur.ProxyPort = st.ProxyPort
	cur.ProxyBind = st.ProxyBind
	cur.SubscriptionEnabled = st.SubscriptionEnabled
	cur.SubscriptionHost = st.SubscriptionHost
	// proxy_enabled is intentionally not honored here — flip it via
	// /proxy/start or /proxy/stop so the listener state machine stays the
	// single point of authority.

	if err := repo.UpdateSettings(r.Context(), s.deps.DB, cur); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if s.deps.ReconfigureListeners != nil {
		if err := s.deps.ReconfigureListeners(); err != nil {
			_ = repo.UpdateSettings(r.Context(), s.deps.DB, prev)
			writeJSON(w, 409, map[string]any{
				"error":   "port_in_use",
				"message": err.Error(),
			})
			return
		}
	}
	// The universal password is managed via its own endpoint and untouched
	// here; report its real current state so this response stays consistent
	// with GET /settings (don't let it default to a misleading false).
	hasUniversal, err := repo.HasUniversalProxyPassword(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, settingsDTO{
		SyncIntervalMinutes:       cur.SyncIntervalMinutes,
		ProxyPort:                 cur.ProxyPort,
		ProxyBind:                 cur.ProxyBind,
		ProxyEnabled:              cur.ProxyEnabled,
		SubscriptionEnabled:       cur.SubscriptionEnabled,
		SubscriptionHost:          cur.SubscriptionHost,
		UniversalProxyPasswordSet: hasUniversal,
	})
}

func (s *Server) proxyStatusHandler(w http.ResponseWriter, r *http.Request) {
	if s.deps.ProxyStatus == nil {
		writeJSON(w, 200, map[string]any{"running": false})
		return
	}
	running, proxyAddr := s.deps.ProxyStatus()
	writeJSON(w, 200, map[string]any{
		"running":    running,
		"proxy_addr": proxyAddr,
	})
}

func (s *Server) proxyStartHandler(w http.ResponseWriter, r *http.Request) {
	if s.deps.StartProxy == nil {
		writeErr(w, 500, "start not configured")
		return
	}
	if err := s.deps.StartProxy(); err != nil {
		writeJSON(w, 409, map[string]any{"error": "port_in_use", "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]bool{"running": true})
}

func (s *Server) proxyStopHandler(w http.ResponseWriter, r *http.Request) {
	if s.deps.StopProxy == nil {
		writeErr(w, 500, "stop not configured")
		return
	}
	if err := s.deps.StopProxy(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]bool{"running": false})
}

func (s *Server) reorderUsers(w http.ResponseWriter, r *http.Request) {
	var usernames []string
	if err := json.NewDecoder(r.Body).Decode(&usernames); err != nil {
		writeErr(w, 400, "invalid JSON; expected array of usernames")
		return
	}
	if err := repo.ReorderLocalUsers(r.Context(), s.deps.DB, usernames); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]int{"reordered": len(usernames)})
}

func (s *Server) getAudit(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	entries, err := repo.ListAuditLog(r.Context(), s.deps.DB, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, entries)
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.deps.Registry.SnapshotAll())
}

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"shutting_down": true})
	go func() {
		time.Sleep(50 * time.Millisecond)
		if s.deps.ShutdownFn != nil {
			s.deps.ShutdownFn()
		}
	}()
}

// shutdownGuard prevents repeated POST /api/v1/shutdown spam.
var shutdownGuard atomic.Bool

// ensureDataDir creates the data dir if it doesn't exist.
func ensureDataDir(p string) error {
	if p == "" {
		return nil
	}
	return os.MkdirAll(p, 0o700)
}

// AADForUpstream is the AAD constant for upstream-password encryption,
// re-exported for the few places api needs to decrypt (currently none).
func AADForUpstream() []byte {
	return crypto.ColumnAAD("upstream_proxies.encrypted_password")
}

// Local helper to silence a noisy import in some builds.
var _ = fmt.Sprintf
