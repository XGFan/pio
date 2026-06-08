package repo_test

import (
	"context"
	"testing"

	"github.com/guofan/pio/internal/repo"
	"github.com/guofan/pio/internal/store"
)

func TestSettings_SubscriptionRoundTrip(t *testing.T) {
	db := store.MustOpenInMemoryTest(t)
	ctx := context.Background()

	// Fresh DB defaults: disabled, empty host.
	st, err := repo.LoadSettings(ctx, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if st.SubscriptionEnabled || st.SubscriptionHost != "" {
		t.Fatalf("fresh subscription settings = (%v, %q), want (false, \"\")", st.SubscriptionEnabled, st.SubscriptionHost)
	}

	st.SubscriptionEnabled = true
	st.SubscriptionHost = "proxy.example.com"
	if err := repo.UpdateSettings(ctx, db.DB, st); err != nil {
		t.Fatal(err)
	}

	got, err := repo.LoadSettings(ctx, db.DB)
	if err != nil {
		t.Fatal(err)
	}
	if !got.SubscriptionEnabled {
		t.Error("SubscriptionEnabled did not persist")
	}
	if got.SubscriptionHost != "proxy.example.com" {
		t.Errorf("SubscriptionHost = %q, want proxy.example.com", got.SubscriptionHost)
	}
	// Sanity: the unified proxy port still round-trips alongside.
	if got.ProxyPort != st.ProxyPort {
		t.Errorf("ProxyPort drifted: %d vs %d", got.ProxyPort, st.ProxyPort)
	}
}
