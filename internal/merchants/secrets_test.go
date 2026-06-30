package merchants

import (
	"context"
	"errors"
	"testing"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestMemSecretStore_RoundTripPerMerchant(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySecretStore()

	a := dbtest.TestMerchantID
	b, err := merchant.ParseID("22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatal(err)
	}

	// Put for merchant A, then Get round-trips.
	if _, err := store.Put(ctx, a, SecretStripeSecretKey, "sk_a"); err != nil {
		t.Fatalf("put A: %v", err)
	}
	got, err := store.Get(ctx, a, SecretStripeSecretKey)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	if got.Value != "sk_a" {
		t.Fatalf("get A value = %q, want sk_a", got.Value)
	}

	// Merchant B does NOT see merchant A's secret (namespacing).
	if _, err := store.Get(ctx, b, SecretStripeSecretKey); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("get B before put = %v, want ErrSecretNotFound", err)
	}

	// Put for merchant B with a DIFFERENT value; A's value is unchanged.
	if _, err := store.Put(ctx, b, SecretStripeSecretKey, "sk_b"); err != nil {
		t.Fatalf("put B: %v", err)
	}
	gotA, _ := store.Get(ctx, a, SecretStripeSecretKey)
	gotB, _ := store.Get(ctx, b, SecretStripeSecretKey)
	if gotA.Value != "sk_a" || gotB.Value != "sk_b" {
		t.Fatalf("cross-merchant leak: A=%q B=%q", gotA.Value, gotB.Value)
	}
}

func TestMemSecretStore_RotationVersioning(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySecretStore()
	id := dbtest.TestMerchantID

	v1, _ := store.Put(ctx, id, SecretStripeWebhookSigning, "whsec_1")
	if v1.Version != 1 {
		t.Fatalf("first put version = %d, want 1", v1.Version)
	}
	// Same value = idempotent no-op rotation (version unchanged).
	v1again, _ := store.Put(ctx, id, SecretStripeWebhookSigning, "whsec_1")
	if v1again.Version != 1 {
		t.Fatalf("idempotent put version = %d, want 1", v1again.Version)
	}
	// New value bumps version.
	v2, _ := store.Put(ctx, id, SecretStripeWebhookSigning, "whsec_2")
	if v2.Version != 2 {
		t.Fatalf("rotated put version = %d, want 2", v2.Version)
	}
}

func TestMemSecretStore_DeleteAndList(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySecretStore()
	id := dbtest.TestMerchantID

	_, _ = store.Put(ctx, id, SecretStripeSecretKey, "sk")
	_, _ = store.Put(ctx, id, SecretStripeWebhookSigning, "wh")

	names, err := store.List(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("list len = %d, want 2 (%v)", len(names), names)
	}

	// Delete is idempotent.
	if err := store.Delete(ctx, id, SecretStripeSecretKey); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := store.Delete(ctx, id, SecretStripeSecretKey); err != nil {
		t.Fatalf("delete (idempotent): %v", err)
	}
	if _, err := store.Get(ctx, id, SecretStripeSecretKey); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("get after delete = %v, want ErrSecretNotFound", err)
	}
}

func TestMemSecretStore_ZeroMerchantRejected(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySecretStore()
	if _, err := store.Get(ctx, merchant.ID{}, SecretStripeSecretKey); err == nil {
		t.Fatal("get with zero merchant should error")
	}
	if _, err := store.Put(ctx, merchant.ID{}, SecretStripeSecretKey, "x"); err == nil {
		t.Fatal("put with zero merchant should error")
	}
}

func TestServiceCredentialManagement_WriteOnlyStatusAndDelete(t *testing.T) {
	ctx := context.Background()
	id := dbtest.TestMerchantID
	store := NewMemorySecretStore()
	svc := &Service{secrets: store}

	sec, err := svc.PutCredential(ctx, id, SecretStripeWebhookSigning, "whsec_123")
	if err != nil {
		t.Fatalf("put credential: %v", err)
	}
	if sec.Value != "whsec_123" || sec.Version != 1 {
		t.Fatalf("stored secret = %+v", sec)
	}

	statuses, err := svc.ListSecretStatuses(ctx, id)
	if err != nil {
		t.Fatalf("list statuses: %v", err)
	}
	found := false
	for _, st := range statuses {
		if st.Name != SecretStripeWebhookSigning {
			continue
		}
		found = true
		if !st.Configured || st.Version != 1 {
			t.Fatalf("status = %+v, want configured version 1", st)
		}
	}
	if !found {
		t.Fatalf("status for %s not found", SecretStripeWebhookSigning)
	}

	if err := svc.DeleteCredential(ctx, id, SecretStripeWebhookSigning); err != nil {
		t.Fatalf("delete credential: %v", err)
	}
	if _, err := store.Get(ctx, id, SecretStripeWebhookSigning); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("get after delete = %v, want ErrSecretNotFound", err)
	}
}

func TestServiceCredentialManagement_ValidateOnlyDoesNotSave(t *testing.T) {
	ctx := context.Background()
	id := dbtest.TestMerchantID
	store := NewMemorySecretStore()
	svc := &Service{secrets: store}

	called := false
	err := svc.ValidateCredential(ctx, id, SecretStripeSecretKey, "sk_test_123", func(context.Context, string) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("validate supplied credential: %v", err)
	}
	if !called {
		t.Fatal("stripe tester was not called")
	}
	if _, err := store.Get(ctx, id, SecretStripeSecretKey); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("validate-only saved secret: %v", err)
	}
}

func TestServiceCredentialManagement_RejectsUnknownAndInvalidSecrets(t *testing.T) {
	ctx := context.Background()
	svc := &Service{secrets: NewMemorySecretStore()}

	if _, err := svc.PutCredential(ctx, dbtest.TestMerchantID, "unknown/provider_key", "secret"); err == nil {
		t.Fatal("unknown secret should be rejected")
	}
	if _, err := svc.PutCredential(ctx, dbtest.TestMerchantID, SecretStripeSecretKey, "not-stripe"); err == nil {
		t.Fatal("invalid Stripe secret should be rejected")
	}
	if _, err := svc.PutCredential(ctx, dbtest.TestMerchantID, SecretNMITokenizationURL, "http://example.test/Collect.js"); err == nil {
		t.Fatal("invalid NMI tokenization URL should be rejected")
	}
}

func TestServiceLoadNMITokenizationConfig_Mobius(t *testing.T) {
	ctx := context.Background()
	id := dbtest.TestMerchantID
	store := NewMemorySecretStore()
	svc := &Service{secrets: store}

	cfg, err := svc.LoadNMITokenizationConfig(ctx, id, "mobius")
	if err != nil {
		t.Fatalf("load missing tokenization config: %v", err)
	}
	if cfg.TokenizationKey != "" || cfg.CollectJSURL != DefaultNMICollectJSURL {
		t.Fatalf("missing config = %+v, want empty key and default URL", cfg)
	}

	// Broad merchant-level NMI secrets are no longer runtime credential sources;
	// the loader needs a provider_accounts row so it can read the account-scoped
	// secret path.
}

// fakeVaultKV is an in-memory VaultKV for unit-testing the Vault adapter WITHOUT
// a live Vault.
type fakeVaultKV struct {
	data map[string]map[string]string
}

func newFakeVaultKV() *fakeVaultKV { return &fakeVaultKV{data: map[string]map[string]string{}} }

type staticMerchantSlugResolver map[string]string

func (s staticMerchantSlugResolver) MerchantSlug(_ context.Context, id merchant.ID) (string, error) {
	return s[id.String()], nil
}

func (f *fakeVaultKV) ReadSecret(_ context.Context, path string) (map[string]string, error) {
	d, ok := f.data[path]
	if !ok {
		return nil, nil
	}
	return d, nil
}
func (f *fakeVaultKV) WriteSecret(_ context.Context, path string, data map[string]string) error {
	f.data[path] = data
	return nil
}
func (f *fakeVaultKV) DeleteSecret(_ context.Context, path string) error {
	delete(f.data, path)
	return nil
}
func (f *fakeVaultKV) ListSecrets(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

func TestVaultSecretStore_StubFailsClosed(t *testing.T) {
	ctx := context.Background()
	store := NewVaultSecretStore("secret", nil, staticMerchantSlugResolver{dbtest.TestMerchantID.String(): dbtest.TestMerchantSlug}) // no live client
	id := dbtest.TestMerchantID
	if _, err := store.Get(ctx, id, SecretStripeSecretKey); !errors.Is(err, ErrVaultNotConfigured) {
		t.Fatalf("stub Get = %v, want ErrVaultNotConfigured", err)
	}
	if _, err := store.Put(ctx, id, SecretStripeSecretKey, "x"); !errors.Is(err, ErrVaultNotConfigured) {
		t.Fatalf("stub Put = %v, want ErrVaultNotConfigured", err)
	}
}

func TestVaultSecretStore_RoundTripWithFakeClient(t *testing.T) {
	ctx := context.Background()
	fake := newFakeVaultKV()
	store := NewVaultSecretStore("secret", fake, staticMerchantSlugResolver{dbtest.TestMerchantID.String(): "cozy-art"})
	id := dbtest.TestMerchantID

	if _, err := store.Put(ctx, id, SecretStripeSecretKey, "sk_live"); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.Get(ctx, id, SecretStripeSecretKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Value != "sk_live" {
		t.Fatalf("value = %q, want sk_live", got.Value)
	}
	// Merchant-scoped path isolation: value stored under the merchant subtree.
	if len(fake.data) != 1 {
		t.Fatalf("expected one vault path, got %d", len(fake.data))
	}
	for p := range fake.data {
		if want := "secret/openrails/merchants/cozy-art/" + SecretStripeSecretKey; p != want {
			t.Fatalf("vault path = %q, want %q", p, want)
		}
	}
}
