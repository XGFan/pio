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

// UpstreamProxy is one webshare-issued proxy endpoint. ID is the stable
// 16-hex-char prefix of sha1("host:port:username"), so the same physical
// proxy keeps the same ID across syncs even if webshare's own pagination
// reshuffles rows.
//
// Alive is sync-authoritative (only the sync goroutine writes it).
// RecentlyFailing is advisory and set by the dial-failure heuristic — it
// does NOT veto routing decisions; see plan §4.4.
type UpstreamProxy struct {
	ID                 string
	SourceApiKeyID     int64
	Host               string
	Port               int
	Username           string
	EncryptedPassword  []byte
	Protocol           string // "http" | "socks5"
	DisplayName        string // auto: "{country}-{label}-{seq}"; user-editable; preserved across syncs
	CountryCode        string
	CityName           string
	Alive              bool
	RecentlyFailing    bool
	RecentFailureCount int
	RecentFailureSince *time.Time
	LastSeenAt         time.Time
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
	HTTPListenerPort    int
	HTTPListenerBind    string
	SOCKS5ListenerPort  int
	SOCKS5ListenerBind  string
	APIPort             int
	// ProxyEnabled is the user-controlled on/off switch for the HTTP+SOCKS5
	// listeners. When false the listeners are unbound and the kernel sockets
	// are released; the daemon itself stays running so the REST/UI surface
	// remains reachable.
	ProxyEnabled bool
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
