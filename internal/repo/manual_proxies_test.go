package repo_test

import (
	"context"
	"errors"
	"testing"

	"github.com/guofan/webshare-proxy/internal/crypto"
	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/store"
)

func mustKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, crypto.MasterKeySize)
	for i := range k {
		k[i] = byte(i + 7)
	}
	return k
}

func TestInsertManualProxy_Success(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	id, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "home-vpn", Host: "1.2.3.4", Port: 1080, Protocol: repo.ProtocolSOCKS5,
		Username: "alice", Password: "s3cret",
	})
	if err != nil {
		t.Fatalf("InsertManualProxy: %v", err)
	}
	if len(id) != 16 {
		t.Errorf("manual id = %q, want 16-char hex", id)
	}

	got, err := repo.GetUpstream(ctx, db.DB, id)
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if got.Source != repo.SourceManual {
		t.Errorf("source = %q, want %q", got.Source, repo.SourceManual)
	}
	if got.SourceApiKeyID != nil {
		t.Errorf("source_api_key_id should be nil for manual, got %v", *got.SourceApiKeyID)
	}
	if got.ManualName != "home-vpn" {
		t.Errorf("manual_name = %q", got.ManualName)
	}
	if got.Protocol != repo.ProtocolSOCKS5 {
		t.Errorf("protocol = %q", got.Protocol)
	}
	if got.DisplayName != "home-vpn" {
		t.Errorf("display_name = %q, want display=manual_name", got.DisplayName)
	}

	// Password should decrypt through ListAllResolvedUpstreams.
	all, err := repo.ListAllResolvedUpstreams(ctx, db.DB, mk)
	if err != nil {
		t.Fatalf("ListAllResolvedUpstreams: %v", err)
	}
	row, ok := all[id]
	if !ok {
		t.Fatalf("manual proxy missing from resolved map")
	}
	if row.Password != "s3cret" {
		t.Errorf("decrypted password = %q want s3cret", row.Password)
	}
}

func TestInsertManualProxy_DuplicateName(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	in := repo.ManualProxyInput{
		Name: "dup", Host: "1.1.1.1", Port: 80, Protocol: repo.ProtocolHTTP,
	}
	if _, err := repo.InsertManualProxy(ctx, db.DB, mk, in); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	_, err := repo.InsertManualProxy(ctx, db.DB, mk, in)
	if !errors.Is(err, repo.ErrManualNameInUse) {
		t.Fatalf("expected ErrManualNameInUse, got %v", err)
	}
}

func TestInsertManualProxy_NoAuthHasEmptyCiphertext(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	id, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "no-auth", Host: "10.0.0.1", Port: 3128, Protocol: repo.ProtocolHTTP,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	all, err := repo.ListAllResolvedUpstreams(ctx, db.DB, mk)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	row := all[id]
	if row.Password != "" {
		t.Errorf("expected empty password for no-auth manual proxy, got %q", row.Password)
	}
}

func TestInsertManualProxy_InvalidInput(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	cases := []repo.ManualProxyInput{
		{Name: "", Host: "h", Port: 80, Protocol: "http"},
		{Name: "n", Host: "", Port: 80, Protocol: "http"},
		{Name: "n", Host: "h", Port: 0, Protocol: "http"},
		{Name: "n", Host: "h", Port: 80, Protocol: "ftp"},
	}
	for _, in := range cases {
		_, err := repo.InsertManualProxy(ctx, db.DB, mk, in)
		if !errors.Is(err, repo.ErrInvalidManualProxy) {
			t.Errorf("input %+v: expected ErrInvalidManualProxy, got %v", in, err)
		}
	}
}

func TestUpdateManualProxy_RenameAndKeepPassword(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	id, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "before", Host: "h", Port: 80, Protocol: repo.ProtocolHTTP,
		Username: "u", Password: "p",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Empty password should preserve existing ciphertext.
	if err := repo.UpdateManualProxy(ctx, db.DB, mk, id, repo.ManualProxyInput{
		Name: "after", Host: "h2", Port: 81, Protocol: repo.ProtocolHTTPS,
		Username: "u2", Password: "",
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	all, err := repo.ListAllResolvedUpstreams(ctx, db.DB, mk)
	if err != nil {
		t.Fatal(err)
	}
	row := all[id]
	if row.ManualName != "after" || row.Host != "h2" || row.Port != 81 ||
		row.Protocol != repo.ProtocolHTTPS || row.Username != "u2" {
		t.Errorf("update fields not applied: %+v", row.UpstreamProxy)
	}
	if row.Password != "p" {
		t.Errorf("password should be preserved, got %q", row.Password)
	}
}

func TestUpdateManualProxy_DuplicateName(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	if _, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "a", Host: "h", Port: 80, Protocol: repo.ProtocolHTTP,
	}); err != nil {
		t.Fatal(err)
	}
	id2, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "b", Host: "h", Port: 80, Protocol: repo.ProtocolHTTP,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = repo.UpdateManualProxy(ctx, db.DB, mk, id2, repo.ManualProxyInput{
		Name: "a", Host: "h", Port: 80, Protocol: repo.ProtocolHTTP,
	})
	if !errors.Is(err, repo.ErrManualNameInUse) {
		t.Fatalf("expected ErrManualNameInUse, got %v", err)
	}
}

func TestDeleteManualProxy_NotInUse(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	id, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "tmp", Host: "h", Port: 80, Protocol: repo.ProtocolHTTP,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteManualProxy(ctx, db.DB, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetUpstream(ctx, db.DB, id); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got %v", err)
	}
}

func TestDeleteManualProxy_InUseBlocks(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	id, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "tmp", Host: "h", Port: 80, Protocol: repo.ProtocolHTTP,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertLocalUser(ctx, db.DB, "alice", "pw", ""); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateLocalUserMapping(ctx, db.DB, "alice", &id); err != nil {
		t.Fatal(err)
	}

	err = repo.DeleteManualProxy(ctx, db.DB, id)
	var inUse *repo.ErrUpstreamInUse
	if !errors.As(err, &inUse) {
		t.Fatalf("expected ErrUpstreamInUse, got %v", err)
	}
	if len(inUse.ReferencingUsers) != 1 || inUse.ReferencingUsers[0].Username != "alice" {
		t.Errorf("ReferencingUsers wrong: %+v", inUse.ReferencingUsers)
	}
}

func TestListManualProxies_FiltersWebshare(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	// Insert a webshare row via direct SQL to mimic sync output.
	if _, err := db.DB.ExecContext(ctx,
		`INSERT INTO api_keys (label, encrypted_key, added_at) VALUES ('k', X'00', datetime('now'))`,
	); err != nil {
		t.Fatal(err)
	}
	var keyID int64
	_ = db.DB.QueryRow(`SELECT last_insert_rowid()`).Scan(&keyID)
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO upstream_proxies
			(id, source, source_api_key_id, host, port, encrypted_password, protocol, last_seen_at)
		VALUES ('w_id_for_test1', 'webshare', ?, '1.1.1.1', 80, X'00', 'http', datetime('now'))
	`, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.InsertManualProxy(ctx, db.DB, mk, repo.ManualProxyInput{
		Name: "m", Host: "2.2.2.2", Port: 80, Protocol: repo.ProtocolHTTP,
	}); err != nil {
		t.Fatal(err)
	}

	manuals, err := repo.ListManualProxies(ctx, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if len(manuals) != 1 {
		t.Fatalf("expected 1 manual proxy, got %d", len(manuals))
	}
	if manuals[0].ManualName != "m" {
		t.Errorf("got %q", manuals[0].ManualName)
	}
}
