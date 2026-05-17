// Package cli implements the webshare-proxyd CLI subcommands. The entry
// point Run is invoked from cmd/webshare-proxyd/main.go and from
// integration tests, which inject a fake FetcherFactory to drive the
// sync code path against an httptest server.
package cli

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/store"
	"github.com/guofan/webshare-proxy/internal/sync"
)

// Version is the CLI's reported version string.
const Version = "0.1.0-dev"

// Deps carries injectable dependencies. All zero-value fields fall back to
// production defaults (os.Stdout/Stderr/Stdin and sync.DefaultFetcherFactory).
type Deps struct {
	Stdout         io.Writer
	Stderr         io.Writer
	Stdin          io.Reader
	FetcherFactory sync.FetcherFactory
}

// Run dispatches a subcommand and returns the process exit code. Exit 0
// on success; 2 on usage error; 1 on runtime error.
func Run(ctx context.Context, args []string, deps Deps) int {
	if deps.Stdout == nil {
		deps.Stdout = os.Stdout
	}
	if deps.Stderr == nil {
		deps.Stderr = os.Stderr
	}
	if deps.Stdin == nil {
		deps.Stdin = os.Stdin
	}
	if deps.FetcherFactory == nil {
		deps.FetcherFactory = sync.DefaultFetcherFactory
	}

	if len(args) < 1 {
		printUsage(deps.Stderr)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(deps.Stdout, Version)
		return 0
	case "add-key":
		return runAddKey(ctx, deps, args[1:])
	case "sync":
		return runSync(ctx, deps, args[1:])
	case "run":
		return runDaemon(ctx, deps, args[1:])
	case "help", "--help", "-h":
		printUsage(deps.Stdout)
		return 0
	default:
		fmt.Fprintf(deps.Stderr, "unknown command: %s\n", args[0])
		printUsage(deps.Stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `webshare-proxyd %s

Usage:
  webshare-proxyd version
  webshare-proxyd add-key --label=<s> --key=<s> [--data-dir=<path>]
  webshare-proxyd sync --key-id=<id> [--data-dir=<path>]
  webshare-proxyd run [--data-dir=<path>] [--web-bind=<addr>] [--web-password=<s>]

Flags:
  --web-bind=<addr>      Serve the LAN web admin panel on this addr (e.g. 0.0.0.0:9090).
                         Disabled when empty (default). Mac menubar app continues to
                         talk to the unauthenticated loopback API regardless.
  --web-password=<s>     Password for the web admin panel; required when --web-bind is set.
                         Can also be supplied via $WEBSHARE_WEB_PASSWORD (recommended,
                         keeps the password out of the process list).
`, Version)
}

// runAddKey inserts a new ApiKey row and prints the assigned ID.
func runAddKey(ctx context.Context, deps Deps, args []string) int {
	fs := flag.NewFlagSet("add-key", flag.ContinueOnError)
	fs.SetOutput(deps.Stderr)
	label := fs.String("label", "", "human-readable label for this key")
	key := fs.String("key", "", "webshare API key (sk_...)")
	dataDir := fs.String("data-dir", "", "override the default data directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *label == "" || *key == "" {
		fmt.Fprintln(deps.Stderr, "add-key: --label and --key are required")
		return 2
	}

	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "data dir: %v\n", err)
		return 1
	}
	db, mk, err := openDB(ctx, dir)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "open: %v\n", err)
		return 1
	}
	defer db.Close()

	id, err := repo.InsertApiKey(ctx, db, mk, *label, *key)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "insert: %v\n", err)
		return 1
	}
	fmt.Fprintf(deps.Stdout, "added api_key id=%d label=%q\n", id, *label)
	return 0
}

// runSync syncs one API key's upstream list.
func runSync(ctx context.Context, deps Deps, args []string) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(deps.Stderr)
	keyID := fs.Int64("key-id", 0, "ID of the api_keys row to sync")
	dataDir := fs.String("data-dir", "", "override the default data directory")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keyID <= 0 {
		fmt.Fprintln(deps.Stderr, "sync: --key-id is required and must be > 0")
		return 2
	}

	dir, err := resolveDataDir(*dataDir)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "data dir: %v\n", err)
		return 1
	}
	db, mk, err := openDB(ctx, dir)
	if err != nil {
		fmt.Fprintf(deps.Stderr, "open: %v\n", err)
		return 1
	}
	defer db.Close()

	svc := sync.NewService(db, mk, deps.FetcherFactory)
	if err := svc.SyncKey(ctx, *keyID); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			fmt.Fprintf(deps.Stderr, "sync: no api_key with id=%d\n", *keyID)
		} else {
			fmt.Fprintf(deps.Stderr, "sync: %v\n", err)
		}
		return 1
	}
	fmt.Fprintf(deps.Stdout, "sync ok key_id=%d\n", *keyID)
	return 0
}

// resolveDataDir returns the override if non-empty, else the OS default.
func resolveDataDir(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return DefaultDataDir()
}

// openDB ensures the data directory exists, loads-or-creates the master
// key, and opens (with migrations applied) the SQLite database. Caller
// must close the returned *sql.DB.
func openDB(ctx context.Context, dir string) (*sql.DB, []byte, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	mk, err := crypto.LoadOrCreate(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("master key: %w", err)
	}
	db, err := store.Open(ctx, dataDBPath(dir))
	if err != nil {
		return nil, nil, fmt.Errorf("store open: %w", err)
	}
	return db, mk, nil
}
