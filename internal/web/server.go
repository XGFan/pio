// Package web is the LAN-facing companion to internal/api. It serves a
// static single-page admin UI plus a proxied /api/v1/* surface protected
// by a cookie-session password challenge. The existing 127.0.0.1:random
// API used by the macOS app is untouched.
package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

const (
	cookieName    = "pia_session"
	sessionTTL    = 24 * time.Hour
	loginMinDelay = 200 * time.Millisecond
)

// Options is the dependency bag for New. APIHandler must be the chi.Router
// returned by api.Server.Handler(); the web server mounts it under cookie
// auth at /api/v1/*.
type Options struct {
	Bind       string
	Password   string
	APIHandler http.Handler
	// SubscriptionHandler, when non-nil, is mounted at GET /subscription
	// OUTSIDE the cookie-auth middleware — it authenticates via its own
	// ?password= query parameter only, so proxy clients can fetch it without
	// a session. May be nil to disable the public route.
	SubscriptionHandler http.Handler
	Logger              *slog.Logger
}

// Server is the LAN-facing HTTP server.
type Server struct {
	opts     Options
	sessions *SessionStore
	ln       net.Listener
	srv      *http.Server
}

// New validates the options and returns a server ready to Bind. Password
// must be non-empty — the caller is responsible for refusing to start the
// web listener at all when the operator has not supplied one.
func New(opts Options) (*Server, error) {
	if opts.Bind == "" {
		return nil, errors.New("web: Bind is required")
	}
	if opts.Password == "" {
		return nil, errors.New("web: Password is required")
	}
	if opts.APIHandler == nil {
		return nil, errors.New("web: APIHandler is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Server{
		opts:     opts,
		sessions: NewSessionStore(sessionTTL),
	}, nil
}

// Bind opens the listener. Returns the bound port.
func (s *Server) Bind() (int, error) {
	ln, err := net.Listen("tcp", s.opts.Bind)
	if err != nil {
		return 0, err
	}
	s.ln = ln
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(p)
	return port, nil
}

// Serve runs until ctx is cancelled or Shutdown is called.
func (s *Server) Serve(ctx context.Context) error {
	s.srv = &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second}
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

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

func (s *Server) handler() http.Handler {
	r := chi.NewRouter()

	r.Get("/", s.rootHandler)
	r.Get("/login", serveStatic("login.html"))
	r.Get("/app", s.requireAuth(serveStatic("index.html")))
	r.Handle("/assets/*", http.StripPrefix("/assets/", http.FileServerFS(staticFS)))

	r.Post("/web/api/login", s.loginHandler)
	r.Post("/web/api/logout", s.logoutHandler)
	r.Get("/web/api/session", s.sessionStatusHandler)

	// Public subscription endpoint: query-param auth only, no cookie. Mounted
	// before the auth-gated API so proxy clients can fetch their node list.
	if s.opts.SubscriptionHandler != nil {
		r.Method(http.MethodGet, "/subscription", s.opts.SubscriptionHandler)
	}

	// /api/v1/* is forwarded to the embedded api handler behind auth.
	r.With(s.requireAuthAPI).Handle("/api/v1/*", s.opts.APIHandler)

	return r
}

// --- handlers --------------------------------------------------------------

func (s *Server) rootHandler(w http.ResponseWriter, r *http.Request) {
	if s.validCookie(r) {
		http.Redirect(w, r, "/app", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// serveStatic returns a handler that serves a single named file out of the
// embedded FS. Cache-Control: no-store keeps a re-deploy from being shadowed
// by a stale browser cache.
func serveStatic(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, staticFS, name)
	}
}

func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() {
		// Constant-time floor — even on success — so an attacker can't
		// time-distinguish good vs bad passwords.
		if d := loginMinDelay - time.Since(start); d > 0 {
			time.Sleep(d)
		}
	}()

	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(s.opts.Password)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid password"})
		return
	}
	token, expires := s.sessions.Issue()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessions.Revoke(c.Value)
	}
	// Always clear the cookie regardless.
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) sessionStatusHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": s.validCookie(r)})
}

// requireAuth wraps HTML routes: redirects to /login when the cookie is
// missing or stale.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.validCookie(r) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

// requireAuthAPI wraps the /api/v1/* mount: returns 401 JSON when the
// cookie is missing or stale. The /events WebSocket also lives under
// /api/v1 and is gated the same way; browsers attach the cookie on the
// WebSocket handshake automatically.
func (s *Server) requireAuthAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validCookie(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validCookie(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return s.sessions.Validate(c.Value)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// IsLoopbackBind returns true when the supplied bind address resolves to a
// loopback interface. Exported so cli/run.go can print the LAN warning.
func IsLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "" || host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
