package web

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// SessionStore is an in-memory token→expiry map. Sessions are issued at
// login and validated on every request to the protected /api/v1/* surface.
// State is intentionally not persisted — restarting the daemon logs every
// user out, which is the right default for a LAN admin panel.
type SessionStore struct {
	ttl  time.Duration
	mu   sync.Mutex
	data map[string]time.Time
	now  func() time.Time
}

// NewSessionStore wires a store with the given TTL.
func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{
		ttl:  ttl,
		data: map[string]time.Time{},
		now:  time.Now,
	}
}

// Issue mints a fresh token and stores it. Returns the token and its
// expiry time. 256 bits of entropy.
func (s *SessionStore) Issue() (token string, expires time.Time) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand on macOS/Linux does not fail in practice; if it did,
		// minting a session is unsafe.
		panic("session: crypto/rand failed: " + err.Error())
	}
	token = base64.RawURLEncoding.EncodeToString(b[:])
	expires = s.now().Add(s.ttl)
	s.mu.Lock()
	s.data[token] = expires
	s.gcLocked()
	s.mu.Unlock()
	return token, expires
}

// Validate returns true if the token is present and not expired. Expired
// tokens are dropped opportunistically.
func (s *SessionStore) Validate(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.data[token]
	if !ok {
		return false
	}
	if !s.now().Before(exp) {
		delete(s.data, token)
		return false
	}
	return true
}

// Revoke deletes a session token (used by logout).
func (s *SessionStore) Revoke(token string) {
	s.mu.Lock()
	delete(s.data, token)
	s.mu.Unlock()
}

// gcLocked drops expired entries. Called under s.mu.
func (s *SessionStore) gcLocked() {
	now := s.now()
	for tok, exp := range s.data {
		if !now.Before(exp) {
			delete(s.data, tok)
		}
	}
}
