//go:build lockaudit

package lockaudit

import "testing"

// TestEnabledFlag is the most basic check that the build tag selected
// the instrumented files.
func TestEnabledFlag(t *testing.T) {
	if !Enabled() {
		t.Fatal("Enabled() must be true under -tags=lockaudit")
	}
}

// TestLockOrderViolationPanics ensures the harness actually fires when
// the documented lock order is violated.
func TestLockOrderViolationPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for routingMu acquired inside open tx")
		}
	}()
	EnterTx()
	defer ExitTx()
	EnterRoutingLock() // must panic
	defer ExitRoutingLock()
}
