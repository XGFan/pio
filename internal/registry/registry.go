// Package registry tracks every active proxied connection so Phase 4's
// hot-switch path can force them closed when the mapping that anchors
// them changes.
//
// One ActiveConnection corresponds to one (client ↔ upstream) bridge.
// The registry is in-memory only: it's recreated from scratch on every
// process start, since restarting a daemon necessarily severs every
// real connection anyway.
package registry

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// ActiveConnection is the in-memory record for one live connection.
// CancelFunc is invoked from CloseByUserUpstream to wake the connection's
// bridge goroutine via its derived ctx → SetDeadline(now) → unblock Read.
type ActiveConnection struct {
	ID         uint64
	Username   string
	UpstreamID string
	ClientAddr string
	Protocol   string // "http" | "socks5"
	Target     string
	AcceptedAt time.Time
	BytesIn    atomic.Int64
	BytesOut   atomic.Int64
	CancelFunc context.CancelFunc
}

// ConnectionRegistry is safe for concurrent use.
type ConnectionRegistry struct {
	mu     sync.RWMutex
	nextID atomic.Uint64
	conns  map[uint64]*ActiveConnection
	// byKey maps "username|upstreamID" to the set of conn IDs under it.
	// CloseByUserUpstream walks this index without scanning the full map.
	byKey map[string]map[uint64]struct{}
}

// New returns an empty registry.
func New() *ConnectionRegistry {
	return &ConnectionRegistry{
		conns: map[uint64]*ActiveConnection{},
		byKey: map[string]map[uint64]struct{}{},
	}
}

// Register assigns an ID, stores the connection, and returns its ID.
func (r *ConnectionRegistry) Register(c *ActiveConnection) uint64 {
	id := r.nextID.Add(1)
	c.ID = id
	key := c.Username + "|" + c.UpstreamID
	r.mu.Lock()
	r.conns[id] = c
	set, ok := r.byKey[key]
	if !ok {
		set = map[uint64]struct{}{}
		r.byKey[key] = set
	}
	set[id] = struct{}{}
	r.mu.Unlock()
	return id
}

// Deregister removes the connection from both indexes.
func (r *ConnectionRegistry) Deregister(id uint64) {
	r.mu.Lock()
	if c, ok := r.conns[id]; ok {
		key := c.Username + "|" + c.UpstreamID
		if set, ok := r.byKey[key]; ok {
			delete(set, id)
			if len(set) == 0 {
				delete(r.byKey, key)
			}
		}
		delete(r.conns, id)
	}
	r.mu.Unlock()
}

// CloseByUserUpstream cancels every connection currently registered under
// (username, upstreamID) and returns the count of cancels issued. Phase 4's
// hot-switch calls this with the OLD (username, upstreamID) pair after
// the routing-state pointer swap.
func (r *ConnectionRegistry) CloseByUserUpstream(username, upstreamID string) int {
	key := username + "|" + upstreamID
	r.mu.RLock()
	ids := r.byKey[key]
	cancels := make([]context.CancelFunc, 0, len(ids))
	for id := range ids {
		if c, ok := r.conns[id]; ok {
			cancels = append(cancels, c.CancelFunc)
		}
	}
	r.mu.RUnlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

// CloseByUpstream cancels every connection currently bridging through the
// given upstream ID, regardless of which username (or universal/display-name
// route) opened it. The per-username/per-mapping CancelGroup machinery can't
// reach universal-password connections — they aren't anchored to a local user
// — so this upstream-scoped sweep is how an upstream edit or delete tears
// those bridges down within ~1 TCP RTT. Returns the count of cancels issued.
func (r *ConnectionRegistry) CloseByUpstream(upstreamID string) int {
	r.mu.RLock()
	var cancels []context.CancelFunc
	for _, c := range r.conns {
		if c.UpstreamID == upstreamID {
			cancels = append(cancels, c.CancelFunc)
		}
	}
	r.mu.RUnlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

// CloseByUsername cancels every connection currently registered under a
// username regardless of upstream. Used by API-key cascade-delete when
// the user's mapping is being nulled.
func (r *ConnectionRegistry) CloseByUsername(username string) int {
	r.mu.RLock()
	var cancels []context.CancelFunc
	for _, c := range r.conns {
		if c.Username == username {
			cancels = append(cancels, c.CancelFunc)
		}
	}
	r.mu.RUnlock()
	for _, cancel := range cancels {
		cancel()
	}
	return len(cancels)
}

// Snapshot returns a flat copy of every active connection's metadata.
// Safe for use in REST GET /api/v1/connections.
type Snapshot struct {
	ID         uint64
	Username   string
	UpstreamID string
	ClientAddr string
	Protocol   string
	Target     string
	AcceptedAt time.Time
	BytesIn    int64
	BytesOut   int64
}

// SnapshotAll returns every active conn's metadata. Caller is welcome to
// sort the slice however the UI wants.
func (r *ConnectionRegistry) SnapshotAll() []Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Snapshot, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, Snapshot{
			ID: c.ID, Username: c.Username, UpstreamID: c.UpstreamID,
			ClientAddr: c.ClientAddr, Protocol: c.Protocol, Target: c.Target,
			AcceptedAt: c.AcceptedAt, BytesIn: c.BytesIn.Load(), BytesOut: c.BytesOut.Load(),
		})
	}
	return out
}

// Len returns the current count of registered connections.
func (r *ConnectionRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.conns)
}
