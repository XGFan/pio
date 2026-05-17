// Package lockaudit is a build-tag-gated harness that catches violations
// of the v4.1 plan §4 lock order: "never hold routingMu while doing
// SQLite I/O; never call into routing-locking code from inside a SQLite
// transaction."
//
// Normal builds compile to a no-op shim with zero runtime cost (the
// production code calls Enter/Exit and the no-op functions return
// immediately). The real harness is in lockaudit_enabled.go and only
// builds under `-tags=lockaudit`. CI runs:
//
//	go test -tags=lockaudit -race ./...
//
// to exercise the assertion path.
package lockaudit

// Enabled reports whether the harness is compiled in. Used by tests that
// want to assert "this test only runs under the lockaudit build tag".
func Enabled() bool { return enabled }
