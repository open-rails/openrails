package merchants

import (
	"context"
	"path"
	"strings"
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
)

// The restricted name is DERIVED from the canonical builder, never hand-written
// (SEC-20): the previous literal `rail_merchant_accounts/…` here matched the
// equally stale guard pattern, so both agreed on a path production had not
// emitted since the psps rename and the test stayed green while the guard was
// disarmed. A future rename now breaks this test instead.
func restrictedSolanaPrivateKey(t *testing.T) string {
	t.Helper()
	name, err := PSPSecretName("solana", "live", "AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9", "private_key")
	if err != nil {
		t.Fatalf("build canonical solana private-key name: %v", err)
	}
	return name
}

// The guard pattern must actually match what the builder emits.
func TestSolanaPrivateKeyWritePatternMatchesCanonicalName(t *testing.T) {
	pattern := SolanaPrivateKeyWritePattern()
	for _, env := range []string{"live", "test"} {
		name, err := PSPSecretName("solana", env, "AKnL4NNf3DGWZJS6cPknBuEGnVsV4A4m5tgebLHaRSZ9", "private_key")
		if err != nil {
			t.Fatalf("build %s name: %v", env, err)
		}
		matched, err := path.Match(pattern, name)
		if err != nil || !matched {
			t.Fatalf("pattern %q does not match %q (err=%v)", pattern, name, err)
		}
	}
	// It must not swallow unrelated PSP secrets.
	other, err := PSPSecretName("stripe", "live", "acct_123", "secret_key")
	if err != nil {
		t.Fatalf("build stripe name: %v", err)
	}
	if matched, _ := path.Match(pattern, other); matched {
		t.Fatalf("pattern %q must not match %q", pattern, other)
	}
}

func TestWriteRestrictedSecretStoreBlocksRestrictedPut(t *testing.T) {
	store := NewWriteRestrictedSecretStore(NewMemorySecretStore(), map[string]string{
		SolanaPrivateKeyWritePattern(): "encryption required",
	})

	_, err := store.Put(context.Background(), dbtest.TestMerchantID, restrictedSolanaPrivateKey(t), "secret")
	if err == nil {
		t.Fatal("expected restricted write to fail")
	}
	if !strings.Contains(err.Error(), "encryption required") {
		t.Fatalf("error = %q, want encryption reason", err.Error())
	}
}

func TestWriteRestrictedSecretStoreAllowsOtherSecrets(t *testing.T) {
	store := NewWriteRestrictedSecretStore(NewMemorySecretStore(), map[string]string{
		SolanaPrivateKeyWritePattern(): "encryption required",
	})

	const value = "sk_test_123"
	if _, err := store.Put(context.Background(), dbtest.TestMerchantID, SecretStripeSecretKey, value); err != nil {
		t.Fatalf("put unrestricted secret: %v", err)
	}
	got, err := store.Get(context.Background(), dbtest.TestMerchantID, SecretStripeSecretKey)
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
	name := restrictedSolanaPrivateKey(t)
	if _, err := inner.Put(context.Background(), dbtest.TestMerchantID, name, value); err != nil {
		t.Fatalf("pre-put: %v", err)
	}
	store := NewWriteRestrictedSecretStore(inner, map[string]string{
		SolanaPrivateKeyWritePattern(): "encryption required",
	})

	got, err := store.Get(context.Background(), dbtest.TestMerchantID, name)
	if err != nil {
		t.Fatalf("get existing restricted secret: %v", err)
	}
	if got.Value != value {
		t.Fatalf("value = %q, want %q", got.Value, value)
	}
}
