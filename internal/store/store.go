// Package store opens the daemon's SQLite database and applies embedded
// migrations. The database is opened with foreign_keys=ON, WAL journaling,
// and a 5s busy timeout — all enforced per-connection via DSN pragmas so
// connection-pool reuse cannot drop the settings.
//
// Migrations are plain .sql files embedded under migrations/. They are
// applied in lexicographic filename order and recorded in a
// schema_migrations table to make repeat Opens idempotent. We chose a
// hand-rolled runner over golang-migrate/migrate because the v4.1 plan
// mandates a pure-Go (no cgo) build via modernc.org/sqlite, and
// golang-migrate has no maintained pure-Go SQLite driver.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // registers "sqlite" driver
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// dsnPragmas are applied on every connection the sql.DB pool opens. Pragmas
// in the DSN are scoped to the connection, so the pool can recycle freely
// without losing FK enforcement or WAL mode.
const dsnPragmas = "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"

// Open opens (or creates) the SQLite database at path, applies any
// outstanding embedded migrations, and returns the *sql.DB. Calling Open
// twice on the same path is safe — on the second call the migration runner
// notices that schema_migrations already has the highest version and
// applies nothing.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if path == "" {
		return nil, errors.New("store: empty db path")
	}
	dsn := "file:" + filepath.ToSlash(path) + "?" + dsnPragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Ping forces the first connection so a bad path / pragma fails fast.
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// OpenInMemory returns a fresh in-memory database with the full schema
// applied. Each call returns a distinct DB; the shared-cache trick keeps
// it usable across the connection pool. Test-only helper.
func OpenInMemory(ctx context.Context) (*sql.DB, error) {
	// Unique URI per call so two in-memory DBs in the same test don't share.
	name := "mem-" + randHex(8)
	dsn := "file:" + name + "?mode=memory&cache=shared&" + dsnPragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Pin a single connection so the shared-cache memory DB doesn't get
	// dropped when idle conns expire.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := applyMigrations(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func applyMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	applied := make(map[string]bool)
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("read applied: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("exec %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", name, err)
		}
	}
	return nil
}
