// Package routing owns the in-memory authoritative routing state and the
// two-lock split that protects it.
//
// Phase 2 builds the full immutable *RoutingState data structure end-to-end
// (read-only at runtime; populated from SQLite at boot via Hydrate). Phase 4
// adds the swap mechanism (SwapUserMapping + CancelGroup cancellation walk)
// without rewriting the data structure.
//
// Invariants enforced here:
//
//   - All readers (data-plane Acquire AND diagnostic emitters) read the
//     in-memory *RoutingState pointer under routingMu.RLock(). SQLite is
//     never read at runtime for routing decisions; it is consulted only at
//     process start to rehydrate.
//   - The in-memory swap (Phase 4) is a single pointer assignment under
//     routingMu.Lock() (microsecond hold). The new *RoutingState is fully
//     constructed before the lock is acquired.
//   - Lock order: mappingChangeMu > routingMu > sql.Tx. failureCountersMu
//     (Phase 4 advisory counter lock) is a leaf — never held while
//     acquiring any other lock; no other lock held while acquiring it.
package routing

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/guofan/pia/internal/model"
	"github.com/guofan/pia/internal/repo"
)

// CancelGroup is the cancellation primitive shared by all connections
// routing through one (user → upstream) mapping. When a mapping changes,
// the old CancelGroup's Cancel() is invoked once, propagating ctx.Done()
// into every active connection's bridge goroutine.
type CancelGroup struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// NewCancelGroup returns a fresh group whose Context is independent of
// the daemon's root context — cancelling one CancelGroup never affects
// any other.
func NewCancelGroup() *CancelGroup {
	ctx, cancel := context.WithCancel(context.Background())
	return &CancelGroup{ctx: ctx, cancel: cancel}
}

// Context returns the cancellation context. Bridge goroutines wait on this.
func (g *CancelGroup) Context() context.Context { return g.ctx }

// Cancel signals every reader of Context() to terminate. Safe to call
// multiple times; subsequent calls are no-ops.
func (g *CancelGroup) Cancel() { g.cancel() }

// ResolvedUser is one fully-resolved auth+routing record. Pointers are
// owned by the immutable RoutingState; readers must not mutate any field.
type ResolvedUser struct {
	Username      string
	PasswordPlain string
	UpstreamID    string
	Upstream      *model.UpstreamProxy
	UpstreamPwd   string
	Broken        bool
	CancelGroup   *CancelGroup
}

// DisplayNameRoute is one (display name → upstream) entry the universal-
// password path resolves against. It carries the same fields a ResolvedUser
// would for the data plane, minus any per-user credential: the credential is
// the daemon-wide universal password, validated in tunnel.Acquire.
//
// CancelGroup is a fresh group per build. Unrelated rebuilds do NOT cancel it
// (so a routine sync never tears down universal-routed connections); explicit
// upstream edits/deletes tear those connections down via the connection
// registry's per-connection CancelFunc (registry.CloseByUpstream).
type DisplayNameRoute struct {
	Upstream    *model.UpstreamProxy
	UpstreamPwd string
	CancelGroup *CancelGroup
}

// RoutingState is the immutable snapshot the data plane reads. Phase 4
// replaces this whole struct via a single pointer swap; readers that
// captured the prior pointer keep seeing consistent state until they
// release their RLock.
type RoutingState struct {
	Users     map[string]*ResolvedUser
	Upstreams map[string]*repo.ResolvedUpstream
	// ByDisplayName indexes routable upstreams by their display name for the
	// universal-password path. Only upstreams with a non-empty, unambiguous
	// (unique) display name appear here.
	ByDisplayName map[string]*DisplayNameRoute
	// UniversalPwd is the decrypted universal proxy password, or "" when the
	// feature is disabled. Compared constant-time in tunnel.Acquire.
	UniversalPwd string
	Version      uint64
	BuiltAt      time.Time
}

// buildDisplayNameRoutes indexes upstreams by display name for the universal-
// password path. A display name shared by 2+ upstreams is ambiguous and is
// dropped entirely (not routable) so the universal path can never silently
// pick the wrong proxy.
func buildDisplayNameRoutes(upstreams map[string]*repo.ResolvedUpstream) map[string]*DisplayNameRoute {
	counts := make(map[string]int, len(upstreams))
	for _, up := range upstreams {
		if up.DisplayName != "" {
			counts[up.DisplayName]++
		}
	}
	routes := make(map[string]*DisplayNameRoute)
	for _, up := range upstreams {
		if up.DisplayName == "" {
			continue
		}
		if counts[up.DisplayName] != 1 {
			continue // ambiguous display name → refuse to route it
		}
		routes[up.DisplayName] = &DisplayNameRoute{
			Upstream:    &up.UpstreamProxy,
			UpstreamPwd: up.Password,
			CancelGroup: NewCancelGroup(),
		}
	}
	return routes
}

// Core owns the routing state and the two mutexes that gate access to it.
type Core struct {
	db        *sql.DB
	masterKey []byte

	routingMu       sync.RWMutex
	routingState    *RoutingState
	mappingChangeMu sync.Mutex
}

// NewCore wires a routing core. Caller is responsible for calling Hydrate
// before any reader touches Snapshot.
func NewCore(db *sql.DB, masterKey []byte) *Core {
	return &Core{db: db, masterKey: masterKey}
}

// DB returns the *sql.DB the core was built with. Phase 4's mapping-change
// workflow uses it to begin transactions under mappingChangeMu.
func (c *Core) DB() *sql.DB { return c.db }

// MasterKey returns the master key. Phase 4 uses it for re-encryption
// during mapping changes; Phase 5 uses it for user-password-reveal.
func (c *Core) MasterKey() []byte { return c.masterKey }

// MappingChangeMu exposes the workflow lock so Phase 4's swap workflow
// can serialize mapping changes. The reader path never touches this.
func (c *Core) MappingChangeMu() *sync.Mutex { return &c.mappingChangeMu }

// Snapshot returns the current *RoutingState. The returned pointer is
// immutable; callers must not mutate any field reachable from it.
//
// The RLock is released before the function returns — readers operate on
// the snapshot pointer afterwards without any lock held. This is the
// COW/RCU pattern: subsequent Swaps allocate a new RoutingState; old
// readers continue with the old snapshot until they drop the pointer.
func (c *Core) Snapshot() *RoutingState {
	c.routingMu.RLock()
	s := c.routingState
	c.routingMu.RUnlock()
	return s
}

// Swap atomically replaces the in-memory state with new. Called only from
// Phase 4's mapping-change workflow (which holds mappingChangeMu).
func (c *Core) Swap(new *RoutingState) {
	c.routingMu.Lock()
	c.routingState = new
	c.routingMu.Unlock()
}

// Hydrate reads SQLite once and builds the initial RoutingState. Must be
// called before any reader touches Snapshot. Re-callable: replaces the
// existing state via Swap.
func (c *Core) Hydrate(ctx context.Context) error {
	upstreams, err := repo.ListAllResolvedUpstreams(ctx, c.db, c.masterKey)
	if err != nil {
		return fmt.Errorf("hydrate upstreams: %w", err)
	}
	users, err := repo.ListLocalUsers(ctx, c.db)
	if err != nil {
		return fmt.Errorf("hydrate users: %w", err)
	}
	universalPwd, err := repo.LoadUniversalProxyPassword(ctx, c.db, c.masterKey)
	if err != nil {
		return fmt.Errorf("hydrate universal password: %w", err)
	}

	resolved := make(map[string]*ResolvedUser, len(users))
	for _, u := range users {
		ru := &ResolvedUser{
			Username:      u.Username,
			PasswordPlain: u.PasswordPlain,
			Broken:        u.Broken,
			CancelGroup:   NewCancelGroup(),
		}
		if u.UpstreamProxyID != nil {
			ru.UpstreamID = *u.UpstreamProxyID
			if up, ok := upstreams[*u.UpstreamProxyID]; ok {
				ru.Upstream = &up.UpstreamProxy
				ru.UpstreamPwd = up.Password
			} else {
				// FK should prevent this, but guard for the cascade-null race.
				ru.Broken = true
			}
		} else {
			ru.Broken = true
		}
		resolved[u.Username] = ru
	}

	state := &RoutingState{
		Users:         resolved,
		Upstreams:     upstreams,
		ByDisplayName: buildDisplayNameRoutes(upstreams),
		UniversalPwd:  universalPwd,
		Version:       uint64(time.Now().UnixNano()),
		BuiltAt:       time.Now().UTC(),
	}
	c.Swap(state)
	return nil
}
