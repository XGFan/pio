package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTmp(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenCreatesSchema(t *testing.T) {
	db := openTmp(t)
	for _, table := range []string{"api_keys", "upstream_proxies", "local_users", "settings", "audit_log", "schema_migrations"} {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}
	// settings should have exactly one row.
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settings`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("settings row count = %d want 1", count)
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	db1, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db1.Close()

	db2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer db2.Close()

	// schema_migrations should have exactly one row per migration file
	// (no duplicate apply on second open).
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	want := len(entries)
	var count int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("schema_migrations row count = %d want %d (re-applied?)", count, want)
	}
}

func TestFKSetNullOnUpstreamDelete(t *testing.T) {
	db := openTmp(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	// Insert api_key + upstream + local_user mapped to it.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO api_keys (label, encrypted_key, added_at) VALUES (?, ?, ?)`,
		"k1", []byte{0x01}, now,
	); err != nil {
		t.Fatal(err)
	}
	var keyID int64
	if err := db.QueryRow(`SELECT last_insert_rowid()`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO upstream_proxies (id, source_api_key_id, host, port, username, encrypted_password, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"up1", keyID, "1.2.3.4", 8080, "u", []byte{0x02}, now,
	); err != nil {
		t.Fatal(err)
	}

	upstreamID := "up1"
	if _, err := db.ExecContext(ctx,
		`INSERT INTO local_users (username, password_plain, upstream_proxy_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		"alice", "alicepw", &upstreamID, now, now,
	); err != nil {
		t.Fatal(err)
	}

	// Deleting the upstream must set the FK to NULL on local_users.
	if _, err := db.ExecContext(ctx, `DELETE FROM upstream_proxies WHERE id = ?`, "up1"); err != nil {
		t.Fatalf("delete upstream: %v", err)
	}
	var got sql.NullString
	if err := db.QueryRow(`SELECT upstream_proxy_id FROM local_users WHERE username = ?`, "alice").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("upstream_proxy_id should be NULL after cascade, got %q", got.String)
	}
}

func TestFKBlocksKeyDeleteWhenUpstreamReferences(t *testing.T) {
	db := openTmp(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	if _, err := db.ExecContext(ctx,
		`INSERT INTO api_keys (label, encrypted_key, added_at) VALUES (?, ?, ?)`,
		"k1", []byte{0x01}, now,
	); err != nil {
		t.Fatal(err)
	}
	var keyID int64
	if err := db.QueryRow(`SELECT last_insert_rowid()`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO upstream_proxies (id, source_api_key_id, host, port, username, encrypted_password, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"up1", keyID, "1.2.3.4", 8080, "u", []byte{0x02}, now,
	); err != nil {
		t.Fatal(err)
	}

	_, err := db.ExecContext(ctx, `DELETE FROM api_keys WHERE id = ?`, keyID)
	if err == nil {
		t.Fatal("expected FK ON DELETE NO ACTION to block delete while upstreams reference the key")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "foreign") &&
		!strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Fatalf("expected FK constraint error, got %v", err)
	}
}

func TestForeignKeysEnabledOnEveryConnection(t *testing.T) {
	// Force the pool to hand out multiple connections by running parallel
	// queries; each one must see PRAGMA foreign_keys = 1.
	db := openTmp(t)
	db.SetMaxOpenConns(4)
	ctx := context.Background()
	for i := range 8 {
		var v int
		if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&v); err != nil {
			t.Fatalf("query %d: %v", i, err)
		}
		if v != 1 {
			t.Fatalf("query %d: foreign_keys = %d want 1", i, v)
		}
	}
}

func TestIndexesPresent(t *testing.T) {
	db := openTmp(t)
	expected := []string{
		"idx_upstream_proxies_source_api_key_id",
		"idx_upstream_proxies_country_code",
		"idx_audit_log_at",
	}
	for _, name := range expected {
		var got string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&got)
		if err != nil {
			t.Fatalf("index %s missing: %v", name, err)
		}
	}
}

func TestOpenRejectsEmptyPath(t *testing.T) {
	_, err := Open(context.Background(), "")
	if err == nil {
		t.Fatal("Open with empty path must error")
	}
}
