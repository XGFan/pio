package repo_test

import (
	"context"
	"testing"

	"github.com/guofan/webshare-proxy/internal/repo"
	"github.com/guofan/webshare-proxy/internal/store"
)

func TestUniversalProxyPassword_RoundTrip(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	// Fresh DB: feature disabled.
	if has, err := repo.HasUniversalProxyPassword(ctx, db.DB); err != nil || has {
		t.Fatalf("Has on fresh db = (%v, %v), want (false, nil)", has, err)
	}
	if pwd, err := repo.LoadUniversalProxyPassword(ctx, db.DB, mk); err != nil || pwd != "" {
		t.Fatalf("Load on fresh db = (%q, %v), want (\"\", nil)", pwd, err)
	}

	// Set a password.
	if err := repo.SetUniversalProxyPassword(ctx, db.DB, mk, "s3cret-master"); err != nil {
		t.Fatal(err)
	}
	if has, err := repo.HasUniversalProxyPassword(ctx, db.DB); err != nil || !has {
		t.Fatalf("Has after set = (%v, %v), want (true, nil)", has, err)
	}
	if pwd, err := repo.LoadUniversalProxyPassword(ctx, db.DB, mk); err != nil || pwd != "s3cret-master" {
		t.Fatalf("Load after set = (%q, %v), want (\"s3cret-master\", nil)", pwd, err)
	}

	// Replace it.
	if err := repo.SetUniversalProxyPassword(ctx, db.DB, mk, "rotated"); err != nil {
		t.Fatal(err)
	}
	if pwd, _ := repo.LoadUniversalProxyPassword(ctx, db.DB, mk); pwd != "rotated" {
		t.Fatalf("Load after rotate = %q, want \"rotated\"", pwd)
	}

	// Clear it (empty string disables the feature).
	if err := repo.SetUniversalProxyPassword(ctx, db.DB, mk, ""); err != nil {
		t.Fatal(err)
	}
	if has, _ := repo.HasUniversalProxyPassword(ctx, db.DB); has {
		t.Fatal("Has after clear = true, want false")
	}
	if pwd, _ := repo.LoadUniversalProxyPassword(ctx, db.DB, mk); pwd != "" {
		t.Fatalf("Load after clear = %q, want \"\"", pwd)
	}
}

// TestUniversalProxyPassword_WrongKeyFails confirms the value is bound to the
// master key (AES-GCM): decrypting with a different key fails closed rather
// than returning garbage.
func TestUniversalProxyPassword_WrongKeyFails(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	mk := mustKey(t)
	ctx := context.Background()

	if err := repo.SetUniversalProxyPassword(ctx, db.DB, mk, "s3cret-master"); err != nil {
		t.Fatal(err)
	}

	wrong := make([]byte, len(mk))
	for i := range wrong {
		wrong[i] = mk[i] ^ 0xFF
	}
	if _, err := repo.LoadUniversalProxyPassword(ctx, db.DB, wrong); err == nil {
		t.Fatal("Load with wrong key returned nil error, want decryption failure")
	}
}
