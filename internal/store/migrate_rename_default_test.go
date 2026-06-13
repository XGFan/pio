package store

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
)

// applyMigrationFile runs one embedded migration in its own transaction,
// mirroring the runner (so PRAGMA defer_foreign_keys inside the file takes
// effect). t.Fatal on any error.
func applyMigrationFile(t *testing.T, db *sql.DB, name string) {
	t.Helper()
	body, err := migrationsFS.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin %s: %v", name, err)
	}
	if _, err := tx.ExecContext(context.Background(), string(body)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("exec %s: %v", name, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit %s: %v", name, err)
	}
}

// applyMigrationsBelow applies every embedded migration whose filename sorts
// before stop, reconstructing an older schema version for the test.
func applyMigrationsBelow(t *testing.T, db *sql.DB, stop string) {
	t.Helper()
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") && e.Name() < stop {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		applyMigrationFile(t, db, n)
	}
}

// TestMigration0011_DropsLegacyDirect drives migration 0011 against a DB
// reconstructed at the prior (0010) schema that holds the legacy built-in row
// (id/source/display_name all 'direct') plus a user mapped to it and an
// unrelated webshare row with its own mapped user. It asserts the 'direct' row
// is dropped outright (the new 'default' is seeded at boot, not by the
// migration), the direct-mapped user is left unmapped, the unrelated mapping
// survives the table rebuild, and the new CHECK swaps 'direct'→'default'.
func TestMigration0011_DropsLegacyDirect(t *testing.T) {
	ctx := context.Background()
	dsn := "file:mem-mig-" + randHex(8) + "?mode=memory&cache=shared&" + dsnPragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	// Reconstruct the pre-0011 schema.
	applyMigrationsBelow(t, db, "0011")

	// Seed an api key + the legacy 'direct' built-in row + a webshare row.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO api_keys (label, encrypted_key, added_at) VALUES ('k', X'00', datetime('now'))`); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	var keyID int64
	if err := db.QueryRow(`SELECT last_insert_rowid()`).Scan(&keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO upstream_proxies
			(id, source, source_api_key_id, host, port, encrypted_password, protocol, display_name, last_seen_at)
		VALUES ('direct', 'direct', NULL, '', 0, X'', 'http', 'direct', datetime('now')),
		       ('w1', 'webshare', ?, '1.1.1.1', 80, X'00', 'http', 'US-01', datetime('now'))`,
		keyID); err != nil {
		t.Fatalf("seed upstreams: %v", err)
	}
	// Two users: one mapped to the built-in, one to the webshare row.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO local_users (username, password_plain, upstream_proxy_id, created_at, updated_at)
		VALUES ('duser', 'p', 'direct', datetime('now'), datetime('now')),
		       ('wuser', 'p', 'w1',     datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	// Apply the rename migration.
	applyMigrationFile(t, db, "0011_rename_direct_to_default.sql")

	// The legacy 'direct' row is gone, and the migration does NOT seed 'default'
	// (boot's EnsureDefaultUpstream does that).
	var legacy, defaults int
	_ = db.QueryRow(`SELECT count(*) FROM upstream_proxies WHERE id='direct' OR source='direct'`).Scan(&legacy)
	if legacy != 0 {
		t.Fatalf("legacy 'direct' still present: %d row(s)", legacy)
	}
	_ = db.QueryRow(`SELECT count(*) FROM upstream_proxies WHERE id='default'`).Scan(&defaults)
	if defaults != 0 {
		t.Fatalf("migration must not seed a 'default' row (boot does): found %d", defaults)
	}

	// The direct-mapped user is intentionally left unmapped (no carry-over)...
	var dm sql.NullString
	if err := db.QueryRow(`SELECT upstream_proxy_id FROM local_users WHERE username='duser'`).Scan(&dm); err != nil {
		t.Fatal(err)
	}
	if dm.Valid {
		t.Fatalf("duser mapping = %q, want NULL (direct mapping dropped)", dm.String)
	}
	// ...and the UNRELATED mapping survived the table rebuild intact (this is
	// the FK-preservation guarantee the rebuild depends on).
	var wm sql.NullString
	if err := db.QueryRow(`SELECT upstream_proxy_id FROM local_users WHERE username='wuser'`).Scan(&wm); err != nil {
		t.Fatal(err)
	}
	if wm.String != "w1" {
		t.Fatalf("wuser mapping = %q, want 'w1' (must survive the rebuild)", wm.String)
	}

	// The new CHECK rejects the retired 'direct' source value and accepts 'default'.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO upstream_proxies (id, source, host, port, encrypted_password, last_seen_at)
		VALUES ('x', 'direct', '', 0, X'', datetime('now'))`); err == nil {
		t.Fatal("expected CHECK to reject source='direct'")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO upstream_proxies (id, source, host, port, encrypted_password, last_seen_at)
		VALUES ('d1', 'default', '', 0, X'', datetime('now'))`); err != nil {
		t.Fatalf("CHECK should accept source='default': %v", err)
	}
}

// TestMigration0011_FreshDBAndUnrelatedRows covers the common fresh-install path
// (no legacy 'direct' row — the built-in is seeded at boot, not by the
// migration) plus the load-bearing rebuild guarantees the legacy test doesn't
// touch: a NULL mapping stays NULL, a manual row and its mapping survive, and
// the manual_name UNIQUE index is recreated.
func TestMigration0011_FreshDBAndUnrelatedRows(t *testing.T) {
	ctx := context.Background()
	dsn := "file:mem-mig-" + randHex(8) + "?mode=memory&cache=shared&" + dsnPragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	applyMigrationsBelow(t, db, "0011")

	// A manual row + a NULL-mapped user + a manual-mapped user. No 'direct' row.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO upstream_proxies
			(id, source, source_api_key_id, manual_name, host, port, encrypted_password, protocol, display_name, last_seen_at)
		VALUES ('m1', 'manual', NULL, 'm1', '2.2.2.2', 81, X'', 'http', 'm1', datetime('now'))`); err != nil {
		t.Fatalf("seed manual: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO local_users (username, password_plain, upstream_proxy_id, created_at, updated_at)
		VALUES ('nuser', 'p', NULL, datetime('now'), datetime('now')),
		       ('muser', 'p', 'm1', datetime('now'), datetime('now'))`); err != nil {
		t.Fatalf("seed users: %v", err)
	}

	applyMigrationFile(t, db, "0011_rename_direct_to_default.sql")

	// Fresh path: the migration seeds nothing, so there is no 'default' row yet.
	var defaults int
	_ = db.QueryRow(`SELECT count(*) FROM upstream_proxies WHERE id='default'`).Scan(&defaults)
	if defaults != 0 {
		t.Fatalf("migration must not seed a default row (boot does): found %d", defaults)
	}

	// NULL mapping stays NULL; manual mapping survives the rebuild.
	var nm, mm sql.NullString
	_ = db.QueryRow(`SELECT upstream_proxy_id FROM local_users WHERE username='nuser'`).Scan(&nm)
	_ = db.QueryRow(`SELECT upstream_proxy_id FROM local_users WHERE username='muser'`).Scan(&mm)
	if nm.Valid {
		t.Fatalf("nuser mapping = %q, want NULL", nm.String)
	}
	if mm.String != "m1" {
		t.Fatalf("muser mapping = %q, want 'm1' (must survive rebuild)", mm.String)
	}

	// The manual_name UNIQUE index was recreated: a duplicate is rejected.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO upstream_proxies (id, source, manual_name, host, port, encrypted_password, last_seen_at)
		VALUES ('m2', 'manual', 'm1', '3.3.3.3', 82, X'', datetime('now'))`); err == nil {
		t.Fatal("expected UNIQUE(manual_name) to reject a duplicate manual row")
	}

	// New CHECK accepts 'default' and rejects 'direct'.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO upstream_proxies (id, source, host, port, encrypted_password, last_seen_at)
		VALUES ('d1', 'default', '', 0, X'', datetime('now'))`); err != nil {
		t.Fatalf("CHECK should accept source='default': %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO upstream_proxies (id, source, host, port, encrypted_password, last_seen_at)
		VALUES ('d2', 'direct', '', 0, X'', datetime('now'))`); err == nil {
		t.Fatal("expected CHECK to reject source='direct'")
	}
}
