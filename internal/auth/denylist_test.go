package auth

import (
	"testing"
	"time"
)

func TestDenyListBansAfterThreshold(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	d := New(clock)

	for i := 0; i < threshold-1; i++ {
		if d.RecordFailure("10.0.0.5:12345") {
			t.Fatalf("banned too early after %d failures", i+1)
		}
		if d.IsDenied("10.0.0.5:1") {
			t.Fatalf("denied prematurely after %d failures", i+1)
		}
	}
	if !d.RecordFailure("10.0.0.5:99") {
		t.Fatal("threshold failure should trip the ban")
	}
	if !d.IsDenied("10.0.0.5:anything") {
		t.Fatal("ban should be host-keyed (port-agnostic)")
	}

	// Different host is unaffected.
	if d.IsDenied("10.0.0.6:1") {
		t.Fatal("other IPs must not be banned")
	}
}

func TestDenyListExpiresBan(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowVar := t0
	d := New(func() time.Time { return nowVar })

	for i := 0; i < threshold; i++ {
		d.RecordFailure("10.0.0.7:1000")
	}
	if !d.IsDenied("10.0.0.7:1") {
		t.Fatal("expected banned")
	}
	// Advance past the ban window.
	nowVar = t0.Add(banFor + time.Second)
	if d.IsDenied("10.0.0.7:1") {
		t.Fatal("ban should have expired")
	}
}

func TestDenyListWindowRollsForward(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowVar := t0
	d := New(func() time.Time { return nowVar })

	// 9 failures, then a 70-second gap, then 9 more — none should trip
	// because the window slides.
	for i := 0; i < threshold-1; i++ {
		d.RecordFailure("10.0.0.8:1")
	}
	nowVar = t0.Add(window + 10*time.Second)
	for i := 0; i < threshold-1; i++ {
		if d.RecordFailure("10.0.0.8:1") {
			t.Fatalf("banned despite rolling window (i=%d)", i)
		}
	}
	if d.IsDenied("10.0.0.8:1") {
		t.Fatal("rolling window should have kept the IP clean")
	}
}

func TestDenyListClearAndBanned(t *testing.T) {
	d := New(nil)
	for i := 0; i < threshold; i++ {
		d.RecordFailure("10.0.0.9:1")
	}
	if len(d.Banned()) != 1 {
		t.Fatalf("Banned() len = %d want 1", len(d.Banned()))
	}
	d.Clear("10.0.0.9")
	if d.IsDenied("10.0.0.9:1") {
		t.Fatal("Clear should remove ban")
	}
	d.ClearAll()
	if len(d.Banned()) != 0 {
		t.Fatal("ClearAll should empty bans")
	}
}
