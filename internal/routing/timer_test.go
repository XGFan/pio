package routing_test

import "time"

// newTimer is a tiny helper that returns a chan firing after ms milliseconds.
// Lets the test suite avoid importing "time" repeatedly while still keeping
// the timing logic explicit.
func newTimer(ms int) <-chan time.Time {
	return time.After(time.Duration(ms) * time.Millisecond)
}
