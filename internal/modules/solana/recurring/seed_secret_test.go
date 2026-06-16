package recurring

import (
	"context"
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestSeedConfiguredTenantSolanaSecret_SeedsWhenAbsent(t *testing.T) {
	store := merchants.NewMemorySecretStore()
	const key = "5HueCGU8rMjxEXxiPuD5BDku4MkFqeZyd4dZ1jvhTVqvbTLvyTJ"

	if err := SeedConfiguredTenantSolanaSecret(context.Background(), store, dbtest.TestMerchantID, key); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := store.Get(context.Background(), dbtest.TestMerchantID, solanaint.SecretSolanaPrivateKey)
	if err != nil {
		t.Fatalf("get after seed: %v", err)
	}
	if got.Value != key {
		t.Fatalf("seeded value = %q, want %q", got.Value, key)
	}
}

func TestSeedTenantSolanaSecret_SeedsRequestedTenant(t *testing.T) {
	store := merchants.NewMemorySecretStore()
	tenantID, err := merchant.ParseID("019e986d-145c-7a4d-ab33-e7e087f4ce0d")
	if err != nil {
		t.Fatalf("parse merchant id: %v", err)
	}
	const key = "TENANT_SOLANA_KEY"

	if err := SeedTenantSolanaSecret(context.Background(), store, tenantID, key); err != nil {
		t.Fatalf("seed merchant: %v", err)
	}
	got, err := store.Get(context.Background(), tenantID, solanaint.SecretSolanaPrivateKey)
	if err != nil {
		t.Fatalf("get merchant key: %v", err)
	}
	if got.Value != key {
		t.Fatalf("merchant key = %q, want %q", got.Value, key)
	}
	if _, err := store.Get(context.Background(), dbtest.TestMerchantID, solanaint.SecretSolanaPrivateKey); err == nil {
		t.Fatal("did not expect requested merchant seed to write the test merchant")
	}
}

func TestSeedConfiguredTenantSolanaSecret_NoOpWhenPresent(t *testing.T) {
	store := merchants.NewMemorySecretStore()
	const existing = "EXISTING_TENANT_KEY"
	if _, err := store.Put(context.Background(), dbtest.TestMerchantID, solanaint.SecretSolanaPrivateKey, existing); err != nil {
		t.Fatalf("pre-put: %v", err)
	}

	// Global config has a DIFFERENT key; seeding must NOT overwrite the existing one.
	if err := SeedConfiguredTenantSolanaSecret(context.Background(), store, dbtest.TestMerchantID, "GLOBAL_CONFIG_KEY"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := store.Get(context.Background(), dbtest.TestMerchantID, solanaint.SecretSolanaPrivateKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Value != existing {
		t.Fatalf("existing secret was overwritten: got %q, want %q", got.Value, existing)
	}
}

func TestSeedConfiguredTenantSolanaSecret_NoOpWhenConfigEmpty(t *testing.T) {
	store := merchants.NewMemorySecretStore()

	for _, cfg := range []string{"", "   "} {
		if err := SeedConfiguredTenantSolanaSecret(context.Background(), store, dbtest.TestMerchantID, cfg); err != nil {
			t.Fatalf("seed(%q): %v", cfg, err)
		}
	}
	if _, err := store.Get(context.Background(), dbtest.TestMerchantID, solanaint.SecretSolanaPrivateKey); err == nil {
		t.Fatal("expected no secret seeded when config key is empty")
	}
}
