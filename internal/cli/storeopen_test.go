package cli

import (
	"context"
	"database/sql"

	"github.com/guofan/webshare-proxy/internal/store"
)

// storeOpen is a tiny wrapper exposed only to tests in this package; the
// production code never opens the DB twice on the same path, but the
// integration test wants a second handle to verify state independently.
func storeOpen(ctx context.Context, path string) (*sql.DB, error) {
	return store.Open(ctx, path)
}
