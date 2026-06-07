package api

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/guofan/pia/internal/repo"
)

// SubscriptionHandler returns the public GET /subscription handler so the web
// server can mount it OUTSIDE its cookie-auth middleware. Authentication is
// the ?password= query parameter only (no cookie), compared constant-time
// against the universal proxy password.
func (s *Server) SubscriptionHandler() http.HandlerFunc {
	return s.subscriptionHandler
}

// subscriptionHandler serves the subscription list. It exists only when
// subscription is enabled AND a universal password is set; otherwise it 404s
// so its presence leaks nothing. One line per ROUTABLE proxy — exactly the
// universal-password routing set (unambiguous display name) taken from
// the live routing snapshot.
//
// The line scheme is selected by the ?type= query parameter:
//
//	type=socks | type=socks5 | (omitted)
//	    socks://{display-name}:{universal-password}@{subscription-host}:{mixed-port}#{display-name}
//	type=http
//	    http://{display-name}:{universal-password}@{subscription-host}:{mixed-port}#{display-name}
//
// Both schemes point at the SAME unified proxy port (it auto-detects the
// protocol per connection from the first byte); only the URI scheme differs,
// so HTTP-proxy-only clients (e.g. the Chrome extension, which needs
// onAuthRequired credentials Chrome cannot supply for SOCKS) can consume the
// same routing set.
func (s *Server) subscriptionHandler(w http.ResponseWriter, r *http.Request) {
	st, err := repo.LoadSettings(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	universal, err := repo.LoadUniversalProxyPassword(r.Context(), s.deps.DB, s.deps.MasterKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !st.SubscriptionEnabled || universal == "" {
		http.NotFound(w, r)
		return
	}

	// Brute-force protection: this is a public endpoint guarding the universal
	// proxy password, so reuse the same per-IP deny-list the proxy listeners
	// use (10 failures / 60s → 5-min ban). Banned IPs are refused before the
	// compare.
	if s.deps.DenyList != nil && s.deps.DenyList.IsDenied(r.RemoteAddr) {
		writeErr(w, 403, "rate limited")
		return
	}

	// Query-parameter auth only. Constant-time compare avoids leaking the
	// password length via timing.
	pw := r.URL.Query().Get("password")
	if subtle.ConstantTimeCompare([]byte(pw), []byte(universal)) != 1 {
		if s.deps.DenyList != nil {
			s.deps.DenyList.RecordFailure(r.RemoteAddr)
		}
		writeErr(w, 401, "invalid password")
		return
	}

	host := strings.TrimSpace(st.SubscriptionHost)
	if host == "" {
		// Operator left the field blank — fall back to the request host so the
		// generated lines still carry a usable authority instead of ":port".
		host = hostnameOnly(r.Host)
	}
	authority := net.JoinHostPort(host, strconv.Itoa(st.ProxyPort))

	// Scheme selection: "http" emits HTTP-proxy lines; "socks", "socks5", and
	// the empty default all emit SOCKS lines (the historical behavior). Unknown
	// values fall back to SOCKS so older/mistyped clients keep working.
	scheme := "socks"
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("type")), "http") {
		scheme = "http"
	}

	snap := s.deps.Core.Snapshot()
	names := make([]string, 0)
	if snap != nil {
		for name := range snap.ByDisplayName {
			names = append(names, name)
		}
	}
	sort.Strings(names) // deterministic output

	var b strings.Builder
	for _, name := range names {
		line := url.URL{
			Scheme:   scheme,
			User:     url.UserPassword(name, universal),
			Host:     authority,
			Fragment: name,
		}
		b.WriteString(line.String())
		b.WriteByte('\n')
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

// subscriptionURLHandler is the admin-side helper behind the "copy
// subscription URL" button. It is mounted under /api/v1 (so the web panel's
// cookie auth applies) and returns the full public subscription URL —
// including the ?password= — built from the request's own host/scheme, which
// is where /subscription is actually served (NOT subscription_host, which is
// the proxy host). Returns {enabled:false,url:""} when not configured.
func (s *Server) subscriptionURLHandler(w http.ResponseWriter, r *http.Request) {
	st, err := repo.LoadSettings(r.Context(), s.deps.DB)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	universal, err := repo.LoadUniversalProxyPassword(r.Context(), s.deps.DB, s.deps.MasterKey)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !st.SubscriptionEnabled || universal == "" {
		writeJSON(w, 200, map[string]any{"enabled": false, "url": ""})
		return
	}
	scheme := "http"
	if proto := r.Header.Get("X-Forwarded-Proto"); proto == "https" {
		scheme = "https"
	} else if r.TLS != nil {
		scheme = "https"
	}
	u := url.URL{
		Scheme:   scheme,
		Host:     r.Host,
		Path:     "/subscription",
		RawQuery: url.Values{"password": {universal}}.Encode(),
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "url": u.String()})
}

// hostnameOnly strips a :port from a "host:port" string, returning the input
// unchanged when there is no port.
func hostnameOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}
