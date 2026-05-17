package integration

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs goleak once for the integration package after all tests
// have completed and their t.Cleanups have unwound. Per-test goleak is
// brittle here because TCP listener teardown is asynchronous; aggregating
// the check at the package level avoids false positives without losing
// the leak signal.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// modernc/sqlite spawns a background helper per DB; database/sql
		// also keeps a connectionOpener goroutine until the DB is closed.
		// Tests t.Cleanup the DB, so these should drain — but we ignore
		// the top frames defensively in case a parked goroutine lingers.
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*conn).step"),
		goleak.IgnoreTopFunction("modernc.org/sqlite.(*Tx).exec"),
		goleak.IgnoreTopFunction("database/sql.(*DB).connectionOpener"),
	)
}
