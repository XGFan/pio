package crypto

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// MasterKeyName is the on-disk filename for the daemon's master key.
const MasterKeyName = "master.key"

// MasterKeySize is the AES-256 key length in bytes.
const MasterKeySize = 32

// MasterKeyMode is the required file mode (owner-only read/write).
const MasterKeyMode os.FileMode = 0o600

// LoadOrCreate returns the 32-byte master key stored at dir/master.key. If
// the file does not exist, it is created with random contents and mode 0600.
// If it exists but is not exactly 32 bytes, an error is returned (no silent
// regeneration — losing the master key means losing every encrypted column).
func LoadOrCreate(dir string) ([]byte, error) {
	if dir == "" {
		return nil, errors.New("crypto: empty data directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, MasterKeyName)

	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != MasterKeySize {
			return nil, fmt.Errorf("crypto: master key at %s has length %d, want %d", path, len(data), MasterKeySize)
		}
		return data, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read master key: %w", err)
	}

	key := make([]byte, MasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	// Write+chmod separately so the mode is right even when umask would
	// strip it on creation.
	if err := os.WriteFile(path, key, MasterKeyMode); err != nil {
		return nil, fmt.Errorf("write master key: %w", err)
	}
	if err := os.Chmod(path, MasterKeyMode); err != nil {
		return nil, fmt.Errorf("chmod master key: %w", err)
	}
	return key, nil
}
