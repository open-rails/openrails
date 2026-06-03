package recurring

import (
	"context"
	"testing"

	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/tenancy"
	"github.com/open-rails/openrails/pkg/tenant"
)

func TestSeedDefaultTenantSolanaSecret_SeedsWhenAbsent(t *testing.T) {
	store := tenancy.NewMemorySecretStore()
	const key = "5HueCGU8rMjxEXxiPuD5BDku4MkFqeZyd4dZ1jvhTVqvbTLvyTJ"

	if err := SeedDefaultTenantSolanaSecret(context.Background(), store, key); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := store.Get(context.Background(), tenant.DefaultID, solanaint.SecretSolanaPrivateKey)
	if err != nil {
		t.Fatalf("get after seed: %v", err)
	}
	if got.Value != key {
		t.Fatalf("seeded value = %q, want %q", got.Value, key)
	}
}

func TestSeedDefaultTenantSolanaSecret_NoOpWhenPresent(t *testing.T) {
	store := tenancy.NewMemorySecretStore()
	const existing = "EXISTING_TENANT_KEY"
	if _, err := store.Put(context.Background(), tenant.DefaultID, solanaint.SecretSolanaPrivateKey, existing); err != nil {
		t.Fatalf("pre-put: %v", err)
	}

	// Global config has a DIFFERENT key; seeding must NOT overwrite the existing one.
	if err := SeedDefaultTenantSolanaSecret(context.Background(), store, "GLOBAL_CONFIG_KEY"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := store.Get(context.Background(), tenant.DefaultID, solanaint.SecretSolanaPrivateKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Value != existing {
		t.Fatalf("existing secret was overwritten: got %q, want %q", got.Value, existing)
	}
}

func TestSeedDefaultTenantSolanaSecret_NoOpWhenConfigEmpty(t *testing.T) {
	store := tenancy.NewMemorySecretStore()

	for _, cfg := range []string{"", "   "} {
		if err := SeedDefaultTenantSolanaSecret(context.Background(), store, cfg); err != nil {
			t.Fatalf("seed(%q): %v", cfg, err)
		}
	}
	if _, err := store.Get(context.Background(), tenant.DefaultID, solanaint.SecretSolanaPrivateKey); err == nil {
		t.Fatal("expected no secret seeded when config key is empty")
	}
}
