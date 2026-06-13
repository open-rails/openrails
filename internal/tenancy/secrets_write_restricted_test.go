package tenancy

import (
	"context"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestWriteRestrictedSecretStoreBlocksRestrictedPut(t *testing.T) {
	store := NewWriteRestrictedSecretStore(NewMemorySecretStore(), map[string]string{
		"solana/private_key": "encryption required",
	})

	_, err := store.Put(context.Background(), dbtest.TestTenantID, "solana/private_key", "secret")
	if err == nil {
		t.Fatal("expected restricted write to fail")
	}
	if !strings.Contains(err.Error(), "encryption required") {
		t.Fatalf("error = %q, want encryption reason", err.Error())
	}
}

func TestWriteRestrictedSecretStoreAllowsOtherSecrets(t *testing.T) {
	store := NewWriteRestrictedSecretStore(NewMemorySecretStore(), map[string]string{
		"solana/private_key": "encryption required",
	})

	const value = "sk_test_123"
	if _, err := store.Put(context.Background(), dbtest.TestTenantID, SecretStripeSecretKey, value); err != nil {
		t.Fatalf("put unrestricted secret: %v", err)
	}
	got, err := store.Get(context.Background(), dbtest.TestTenantID, SecretStripeSecretKey)
	if err != nil {
		t.Fatalf("get unrestricted secret: %v", err)
	}
	if got.Value != value {
		t.Fatalf("value = %q, want %q", got.Value, value)
	}
}

func TestWriteRestrictedSecretStoreAllowsExistingReads(t *testing.T) {
	inner := NewMemorySecretStore()
	const value = "existing-solana-key"
	if _, err := inner.Put(context.Background(), dbtest.TestTenantID, "solana/private_key", value); err != nil {
		t.Fatalf("pre-put: %v", err)
	}
	store := NewWriteRestrictedSecretStore(inner, map[string]string{
		"solana/private_key": "encryption required",
	})

	got, err := store.Get(context.Background(), dbtest.TestTenantID, "solana/private_key")
	if err != nil {
		t.Fatalf("get existing restricted secret: %v", err)
	}
	if got.Value != value {
		t.Fatalf("value = %q, want %q", got.Value, value)
	}
}
