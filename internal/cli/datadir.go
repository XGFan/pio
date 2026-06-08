package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// DataDBName is the filename of the SQLite database inside the data dir.
const DataDBName = "data.db"

// DefaultDataDir returns the per-OS default location for the daemon's
// persistent state.
//
//   - darwin: $HOME/Library/Application Support/pio
//   - linux/others: $XDG_CONFIG_HOME/pio if set, else $HOME/.config/pio
//
// The directory is not created here; openDB does that.
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "pio"), nil
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "pio"), nil
		}
		return filepath.Join(home, ".config", "pio"), nil
	}
}

// dataDBPath returns the full path of data.db inside dir.
func dataDBPath(dir string) string {
	return filepath.Join(dir, DataDBName)
}
