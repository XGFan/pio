package routing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/guofan/webshare-proxy/internal/repo"
)

// SwapUserMapping is the v4.1 §4.1 hot-switch primitive. Workflow:
//
//	1. mappingChangeMu.Lock()
//	2. RLock snapshot of current *RoutingState
//	3. Build new *RoutingState (pure; affected users get fresh
//	   *ResolvedUser + *CancelGroup; unaffected share pointers)
//	4. BEGIN IMMEDIATE: UPDATE local_users + INSERT audit + COMMIT
//	   (durability fence; on failure: release lock, return error, no swap)
//	5. routingMu.Lock() + pointer-swap + Unlock (microsecond hold)
//	6. Outside both locks: cancel OLD user's CancelGroup
//	7. mappingChangeMu.Unlock()
//
// onCancel is invoked AFTER step 5 with the old ResolvedUser; this is
// the seam where listeners' ConnectionRegistry.CloseByUserUpstream gets
// called. Pure routing logic stays here; the registry side-effect is
// injected so this package doesn't import registry.
//
// newUpstreamID == "" means "unmap" (set upstream_proxy_id NULL + broken=true).
func (c *Core) SwapUserMapping(ctx context.Context, username string, newUpstreamID string, onCancel func(oldUser *ResolvedUser)) error {
	c.mappingChangeMu.Lock()
	defer c.mappingChangeMu.Unlock()

	cur := c.Snapshot()
	if cur == nil {
		return errors.New("routing: state not hydrated")
	}
	oldUser, exists := cur.Users[username]
	if !exists {
		return repo.ErrNotFound
	}

	var newUser *ResolvedUser
	var dbUpstreamID *string
	if newUpstreamID == "" {
		// Unmap: keep the user, clear the mapping, mark broken.
		newUser = &ResolvedUser{
			Username:      oldUser.Username,
			PasswordPlain: oldUser.PasswordPlain,
			Broken:        true,
			CancelGroup:   NewCancelGroup(),
		}
	} else {
		newUp, ok := cur.Upstreams[newUpstreamID]
		if !ok {
			return fmt.Errorf("routing: upstream %s not found", newUpstreamID)
		}
		if !newUp.Alive {
			return fmt.Errorf("routing: upstream %s not alive", newUpstreamID)
		}
		newUser = &ResolvedUser{
			Username:      oldUser.Username,
			PasswordPlain: oldUser.PasswordPlain,
			UpstreamID:    newUp.ID,
			Upstream:      &newUp.UpstreamProxy,
			UpstreamPwd:   newUp.Password,
			Broken:        false,
			CancelGroup:   NewCancelGroup(),
		}
		dbUpstreamID = &newUp.ID
	}

	// Durability fence: persist BEFORE in-memory swap so a crash between
	// step 4 and step 5 leaves SQLite with the new state; next boot
	// rehydrates consistently.
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := repo.UpdateLocalUserMapping(ctx, tx, username, dbUpstreamID); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("persist mapping: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO audit_log (at, actor, action, detail) VALUES (?, 'ui', 'user_remap', ?)`,
		time.Now().UTC(),
		fmt.Sprintf(`{"username":%q,"old_upstream_id":%q,"new_upstream_id":%q}`,
			username, oldUser.UpstreamID, newUpstreamID),
	); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("persist audit: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Build new RoutingState — affected user gets the freshly-allocated
	// ResolvedUser (which carries a fresh CancelGroup); unaffected users
	// share pointers with the previous state for efficiency.
	newUsers := make(map[string]*ResolvedUser, len(cur.Users))
	for u, ru := range cur.Users {
		if u == username {
			newUsers[u] = newUser
			continue
		}
		newUsers[u] = ru
	}
	next := &RoutingState{
		Users:     newUsers,
		Upstreams: cur.Upstreams,
		Version:   cur.Version + 1,
		BuiltAt:   time.Now().UTC(),
	}
	c.Swap(next)

	// Cancel old mapping's CancelGroup outside both locks — bridge
	// goroutines wake via SetDeadline(now) and unwind within ~1 TCP RTT.
	if onCancel != nil {
		onCancel(oldUser)
	}
	return nil
}

// RebuildAfterSync rebuilds the routing state after a webshare sync has
// already committed its upstream changes to SQLite. Called by sync.Service
// at the end of its workflow, AFTER its own SQLite COMMIT.
//
// onMappingBroken is invoked once per (username, oldUpstreamID) whose
// mapping is now Broken because the upstream became alive=false. Phase 4
// listeners hook this to close those users' active connections.
func (c *Core) RebuildAfterSync(ctx context.Context, onMappingBroken func(username, oldUpstreamID string)) error {
	c.mappingChangeMu.Lock()
	defer c.mappingChangeMu.Unlock()

	cur := c.Snapshot()
	upstreams, err := repo.ListAllResolvedUpstreams(ctx, c.db, c.masterKey)
	if err != nil {
		return fmt.Errorf("list upstreams: %w", err)
	}

	newUsers := make(map[string]*ResolvedUser, len(cur.Users))
	var nowBroken []brokenInfo
	for username, ru := range cur.Users {
		wasBroken := ru.Broken
		nru := &ResolvedUser{
			Username:      ru.Username,
			PasswordPlain: ru.PasswordPlain,
			Broken:        false,
			CancelGroup:   ru.CancelGroup, // keep — only swap CG for users whose mapping CHANGES
		}
		if ru.UpstreamID != "" {
			nru.UpstreamID = ru.UpstreamID
			if up, ok := upstreams[ru.UpstreamID]; ok && up.Alive {
				nru.Upstream = &up.UpstreamProxy
				nru.UpstreamPwd = up.Password
			} else {
				nru.Broken = true
			}
		} else {
			nru.Broken = true
		}
		if nru.Broken && !wasBroken {
			// New brokenness; need to cancel any in-flight conns under
			// the old (username, upstream) pair.
			nru.CancelGroup = NewCancelGroup()
			nowBroken = append(nowBroken, brokenInfo{username: username, oldUpstreamID: ru.UpstreamID})
		}
		newUsers[username] = nru
	}

	next := &RoutingState{
		Users:     newUsers,
		Upstreams: upstreams,
		Version:   cur.Version + 1,
		BuiltAt:   time.Now().UTC(),
	}
	c.Swap(next)

	// Outside both locks: cancel old CancelGroups for users that became Broken.
	if onMappingBroken != nil {
		for _, b := range nowBroken {
			// Use the OLD ResolvedUser to access its old CancelGroup.
			if oldRU, ok := cur.Users[b.username]; ok && oldRU.CancelGroup != nil {
				oldRU.CancelGroup.Cancel()
			}
			onMappingBroken(b.username, b.oldUpstreamID)
		}
	}
	return nil
}

type brokenInfo struct {
	username      string
	oldUpstreamID string
}
