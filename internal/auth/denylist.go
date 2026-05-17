// Package auth holds the per-IP auth-failure deny-list used by both
// proxy listeners. 10 failed auth attempts from the same IP within 60
// seconds add that IP to a 5-minute in-memory deny-list. The list is
// cleared on daemon restart by design (no persistence).
package auth

import (
	"net"
	"sync"
	"time"
)

const (
	threshold = 10
	window    = 60 * time.Second
	banFor    = 5 * time.Minute
)

// DenyList is safe for concurrent use. Methods accept either net.IP or a
// "ip:port" string (which is what conn.RemoteAddr().String() returns).
type DenyList struct {
	now     func() time.Time
	mu      sync.Mutex
	fails   map[string][]time.Time // IP → recent failure timestamps within window
	bannedAt map[string]time.Time   // IP → ban start; ban expires at +banFor
}

// New returns an empty deny-list. clock may be nil (defaults to time.Now).
func New(clock func() time.Time) *DenyList {
	if clock == nil {
		clock = time.Now
	}
	return &DenyList{
		now:     clock,
		fails:   map[string][]time.Time{},
		bannedAt: map[string]time.Time{},
	}
}

// IsDenied reports whether the IP behind clientAddr is currently banned.
func (d *DenyList) IsDenied(clientAddr string) bool {
	ip := ipOf(clientAddr)
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.bannedAt[ip]; ok {
		if now.Sub(t) < banFor {
			return true
		}
		delete(d.bannedAt, ip)
	}
	return false
}

// RecordFailure adds a failure timestamp for the IP and trips the ban if
// the threshold is met within the rolling window. Returns the new ban
// state (true if the IP is now banned).
func (d *DenyList) RecordFailure(clientAddr string) bool {
	ip := ipOf(clientAddr)
	now := d.now()
	cutoff := now.Add(-window)
	d.mu.Lock()
	defer d.mu.Unlock()
	stamps := append(d.fails[ip], now)
	// Trim stamps outside the window.
	for len(stamps) > 0 && stamps[0].Before(cutoff) {
		stamps = stamps[1:]
	}
	d.fails[ip] = stamps
	if len(stamps) >= threshold {
		d.bannedAt[ip] = now
		delete(d.fails, ip)
		return true
	}
	return false
}

// Clear forgets a single IP's failure history and ban. Used by the UI's
// "Clear deny-list" button.
func (d *DenyList) Clear(ip string) {
	ip = ipOf(ip)
	d.mu.Lock()
	delete(d.fails, ip)
	delete(d.bannedAt, ip)
	d.mu.Unlock()
}

// ClearAll empties the deny-list completely.
func (d *DenyList) ClearAll() {
	d.mu.Lock()
	d.fails = map[string][]time.Time{}
	d.bannedAt = map[string]time.Time{}
	d.mu.Unlock()
}

// Banned returns the currently-banned IPs with their ban-start times.
func (d *DenyList) Banned() map[string]time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]time.Time, len(d.bannedAt))
	now := d.now()
	for ip, t := range d.bannedAt {
		if now.Sub(t) < banFor {
			out[ip] = t
		}
	}
	return out
}

// ipOf returns just the host portion of "host:port". If clientAddr has no
// port (already an IP), it's returned unchanged.
func ipOf(clientAddr string) string {
	if h, _, err := net.SplitHostPort(clientAddr); err == nil {
		return h
	}
	return clientAddr
}
