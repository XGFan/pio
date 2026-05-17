package store

import (
	"crypto/rand"
	"encoding/hex"
)

// randHex returns 2n random lowercase-hex characters. Used by the
// in-memory test helper to give each opened DB a unique URI.
func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand on darwin/linux/windows does not return errors in
		// practice; if it does we'd rather panic than continue with all-zero.
		panic(err)
	}
	return hex.EncodeToString(b)
}
