//go:build !lockaudit

package lockaudit

// enabled is false in production builds; the lock-order tracking harness
// only ships under `-tags=lockaudit`.
const enabled = false

// EnterTx and ExitTx are no-ops in production builds.
func EnterTx() {}
func ExitTx()  {}

// EnterRoutingLock and ExitRoutingLock are no-ops in production builds.
func EnterRoutingLock() {}
func ExitRoutingLock()  {}
