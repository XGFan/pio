//go:build lockaudit

package lockaudit

import (
	"fmt"
	"runtime"
	"sync"
)

// enabled is true under the lockaudit build tag.
const enabled = true

// state tracks, per goroutine, whether we're currently inside a SQLite
// transaction and/or holding routingMu. Lock-order invariant:
//
//   - If txOpen, acquiring routingMu (Enter*) is illegal.
//   - If routingHeld, opening a tx (EnterTx) is illegal.
type goroutineState struct {
	txOpen      bool
	routingHeld bool
}

var (
	mu     sync.Mutex
	states = map[int64]*goroutineState{} // keyed by a synthesised goroutine id
)

// goid returns a goroutine identifier. The implementation parses the
// runtime stack header, which is documented to be stable enough for
// debug tooling — never use this in production code paths.
func goid() int64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	// stack starts with "goroutine N [..."
	s := string(buf[:n])
	const prefix = "goroutine "
	if len(s) <= len(prefix) {
		return 0
	}
	s = s[len(prefix):]
	var id int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			break
		}
		id = id*10 + int64(c-'0')
	}
	return id
}

func getOrInit(id int64) *goroutineState {
	if g, ok := states[id]; ok {
		return g
	}
	g := &goroutineState{}
	states[id] = g
	return g
}

// EnterTx is called at the start of a SQLite transaction. Panics if
// routingMu is already held on this goroutine.
func EnterTx() {
	id := goid()
	mu.Lock()
	defer mu.Unlock()
	g := getOrInit(id)
	if g.routingHeld {
		panic(fmt.Sprintf("lockaudit: goroutine %d opened tx while holding routingMu", id))
	}
	g.txOpen = true
}

// ExitTx is called when a SQLite transaction commits or rolls back.
func ExitTx() {
	id := goid()
	mu.Lock()
	defer mu.Unlock()
	if g, ok := states[id]; ok {
		g.txOpen = false
	}
}

// EnterRoutingLock is called when routingMu (Lock or RLock) is acquired.
// Panics if a tx is open on this goroutine.
func EnterRoutingLock() {
	id := goid()
	mu.Lock()
	defer mu.Unlock()
	g := getOrInit(id)
	if g.txOpen {
		panic(fmt.Sprintf("lockaudit: goroutine %d acquired routingMu inside open SQLite tx", id))
	}
	g.routingHeld = true
}

// ExitRoutingLock is called when routingMu is released.
func ExitRoutingLock() {
	id := goid()
	mu.Lock()
	defer mu.Unlock()
	if g, ok := states[id]; ok {
		g.routingHeld = false
	}
}
