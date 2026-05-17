package web

import (
	"testing"
	"time"
)

func TestSessionStore_IssueValidate(t *testing.T) {
	s := NewSessionStore(1 * time.Hour)
	tok, exp := s.Issue()
	if tok == "" {
		t.Fatal("Issue returned empty token")
	}
	if exp.Before(time.Now()) {
		t.Fatalf("Issue returned past expiry: %v", exp)
	}
	if !s.Validate(tok) {
		t.Fatal("Validate on freshly-issued token returned false")
	}
}

func TestSessionStore_ValidateEmptyOrUnknown(t *testing.T) {
	s := NewSessionStore(1 * time.Hour)
	if s.Validate("") {
		t.Fatal("Validate on empty token returned true")
	}
	if s.Validate("does-not-exist") {
		t.Fatal("Validate on unknown token returned true")
	}
}

func TestSessionStore_Revoke(t *testing.T) {
	s := NewSessionStore(1 * time.Hour)
	tok, _ := s.Issue()
	if !s.Validate(tok) {
		t.Fatal("token invalid before revoke")
	}
	s.Revoke(tok)
	if s.Validate(tok) {
		t.Fatal("Validate after Revoke returned true")
	}
}

func TestSessionStore_Expire(t *testing.T) {
	clock := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	s := NewSessionStore(1 * time.Hour)
	s.now = func() time.Time { return clock }

	tok, exp := s.Issue()
	if !exp.Equal(clock.Add(1 * time.Hour)) {
		t.Fatalf("unexpected expiry: got=%v want=%v", exp, clock.Add(1*time.Hour))
	}

	// Just before TTL — still valid.
	s.now = func() time.Time { return clock.Add(59 * time.Minute) }
	if !s.Validate(tok) {
		t.Fatal("token rejected before TTL")
	}

	// Past TTL — invalid and dropped opportunistically.
	s.now = func() time.Time { return clock.Add(2 * time.Hour) }
	if s.Validate(tok) {
		t.Fatal("expired token still validates")
	}
	// Second call must remain false (and prove it's been GC'd from the map).
	if s.Validate(tok) {
		t.Fatal("expired token validates on second call")
	}
}

func TestSessionStore_GCOnIssue(t *testing.T) {
	clock := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	s := NewSessionStore(1 * time.Hour)
	s.now = func() time.Time { return clock }

	old, _ := s.Issue()

	// Advance past TTL so `old` is stale, then mint a new one. The new
	// Issue() must garbage-collect `old`.
	s.now = func() time.Time { return clock.Add(2 * time.Hour) }
	fresh, _ := s.Issue()

	s.mu.Lock()
	_, oldStillThere := s.data[old]
	_, freshThere := s.data[fresh]
	count := len(s.data)
	s.mu.Unlock()

	if oldStillThere {
		t.Fatal("expired token was not GC'd by Issue")
	}
	if !freshThere {
		t.Fatal("freshly-issued token is missing from map")
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 live session, got %d", count)
	}
}

func TestSessionStore_TokensAreUnique(t *testing.T) {
	s := NewSessionStore(1 * time.Hour)
	seen := map[string]struct{}{}
	for i := 0; i < 1000; i++ {
		tok, _ := s.Issue()
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token issued at i=%d: %q", i, tok)
		}
		seen[tok] = struct{}{}
	}
}
