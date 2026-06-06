// Package model defines the persistent and in-memory data types for the
// webshare-proxy daemon. Field semantics and storage decisions follow
// .omc/plans/webshare-v4.1.md §3.
package model

import "time"

// ApiKey is one webshare.io account credential. The API key value itself is
// stored AES-256-GCM-encrypted in EncryptedKey; the master key lives in
// ~/Library/Application Support/webshare-proxy/master.key (mode 0600).
type ApiKey struct {
	ID            int64
	Label         string
	EncryptedKey  []byte
	AddedAt       time.Time
	LastSyncedAt  *time.Time
	LastSyncError string
	Active        bool
}

// UpstreamProxy is one upstream proxy endpoint. For webshare-sourced rows
// ID is the stable 16-hex-char prefix of sha1("host:port:username"); for
// manual rows ID is a random 16-hex string generated at insert time. The
// Source discriminator tells the two apart.
//
// Alive is sync-authoritative for webshare rows (only the sync goroutine
// writes it). Manual rows stay alive=true unless explicitly toggled.
// RecentlyFailing is advisory and set by the dial-failure heuristic — it
// does NOT veto routing decisions.
type UpstreamProxy struct {
	ID                 string
	Source             string // "webshare" | "manual"
	SourceApiKeyID     *int64 // nil for manual rows
	ManualName         string // empty for webshare; unique within source='manual' when non-empty
	Host               string
	Port               int
	Username           string
	EncryptedPassword  []byte
	Protocol           string // "http" | "https" | "socks5"
	DisplayName        string // auto: "{label}-{country}-{seq}"; user-editable for webshare; equals ManualName for manual
	CountryCode        string
	CityName           string
	Alive              bool
	RecentlyFailing    bool
	RecentFailureCount int
	RecentFailureSince *time.Time
	LastSeenAt         time.Time
	// LastLatencyMS is the last on-demand latency probe result through this
	// proxy: nil = never tested, -1 = the probe failed, >=0 = milliseconds.
	// LastLatencyAt is when that probe ran. Display-only; routing ignores them.
	LastLatencyMS *int
	LastLatencyAt *time.Time
}

// LocalUser is one (username, password) credential pair the daemon
// authenticates clients with, plus that user's mapped upstream.
//
// PasswordPlain is intentionally plaintext. The DB row protects API keys
// and upstream passwords via column-level AES-GCM, but LocalUser passwords
// are kept plain so the UI can reveal them on demand. Any attacker with
// read access to data.db + master.key already has every other secret, so
// this is documented in the plan's threat model as an accepted trade-off.
type LocalUser struct {
	Username        string
	PasswordPlain   string
	UpstreamProxyID *string
	Broken          bool
	Notes           string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Settings is the single-row daemon configuration. There is exactly one
// row in the settings table; the UI edits it in place.
type Settings struct {
	SyncIntervalMinutes int
	// ProxyPort / ProxyBind configure the single unified proxy listener that
	// serves BOTH the HTTP forward proxy and SOCKS5 on one port (protocol is
	// auto-detected per connection from the first byte). These replace the
	// former separate http_listener_*/socks5_listener_* pair.
	ProxyPort int
	ProxyBind string
	APIPort   int
	// ProxyEnabled is the user-controlled on/off switch for the unified proxy
	// listener. When false the listener is unbound and the kernel socket is
	// released; the daemon itself stays running so the REST/UI surface
	// remains reachable.
	ProxyEnabled bool
	// SubscriptionEnabled gates the public GET /subscription endpoint. It only
	// actually serves when both this is true AND a universal proxy password is
	// configured. SubscriptionHost is the public ip/domain clients use to reach
	// the unified proxy port; it fills the host portion of each generated line.
	SubscriptionEnabled bool
	SubscriptionHost    string
}

// AuditLogEntry records a state-changing operation. Detail is an opaque
// JSON blob whose shape depends on Action.
type AuditLogEntry struct {
	ID     int64
	At     time.Time
	Actor  string // "ui" | "sync" | "cli"
	Action string // "key_add" | "key_delete" | "user_add" | "user_remap" | "sync_complete" | ...
	Detail string
}
