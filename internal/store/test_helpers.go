package store

import (
	"context"
	"database/sql"
	"testing"
)

// DBHandle wraps *sql.DB for test helpers that want a typed close.
type DBHandle struct {
	DB *sql.DB
}

// Close closes the underlying *sql.DB.
func (h *DBHandle) Close() error { return h.DB.Close() }

// MustOpenInMemoryTest opens a fresh in-memory DB and t.Fatals on error.
// Cleanup is registered automatically. Used across packages to seed routing
// fixtures without each test re-rolling the same boilerplate.
func MustOpenInMemoryTest(t *testing.T) *DBHandle {
	t.Helper()
	ctx := context.Background()
	db, err := OpenInMemory(ctx)
	if err != nil {
		t.Fatalf("OpenInMemory: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &DBHandle{DB: db}
}
