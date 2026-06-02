package tenancy

import (
	"context"
	"errors"
	"testing"

	"github.com/open-rails/openrails/pkg/tenant"
)

func TestMemSecretStore_RoundTripPerTenant(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySecretStore()

	a := tenant.DefaultID
	b, err := tenant.ParseID("22222222-2222-2222-2222-222222222222")
	if err != nil {
		t.Fatal(err)
	}

	// Put for tenant A, then Get round-trips.
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

	// Tenant B does NOT see tenant A's secret (namespacing).
	if _, err := store.Get(ctx, b, SecretStripeSecretKey); !errors.Is(err, ErrSecretNotFound) {
		t.Fatalf("get B before put = %v, want ErrSecretNotFound", err)
	}

	// Put for tenant B with a DIFFERENT value; A's value is unchanged.
	if _, err := store.Put(ctx, b, SecretStripeSecretKey, "sk_b"); err != nil {
		t.Fatalf("put B: %v", err)
	}
	gotA, _ := store.Get(ctx, a, SecretStripeSecretKey)
	gotB, _ := store.Get(ctx, b, SecretStripeSecretKey)
	if gotA.Value != "sk_a" || gotB.Value != "sk_b" {
		t.Fatalf("cross-tenant leak: A=%q B=%q", gotA.Value, gotB.Value)
	}
}

func TestMemSecretStore_RotationVersioning(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySecretStore()
	id := tenant.DefaultID

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
	id := tenant.DefaultID

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

func TestMemSecretStore_ZeroTenantRejected(t *testing.T) {
	ctx := context.Background()
	store := NewMemorySecretStore()
	if _, err := store.Get(ctx, tenant.ID{}, SecretStripeSecretKey); err == nil {
		t.Fatal("get with zero tenant should error")
	}
	if _, err := store.Put(ctx, tenant.ID{}, SecretStripeSecretKey, "x"); err == nil {
		t.Fatal("put with zero tenant should error")
	}
}

// fakeVaultKV is an in-memory VaultKV for unit-testing the Vault adapter WITHOUT
// a live Vault.
type fakeVaultKV struct {
	data map[string]map[string]string
}

func newFakeVaultKV() *fakeVaultKV { return &fakeVaultKV{data: map[string]map[string]string{}} }

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
	store := NewVaultSecretStore("secret", nil) // no live client
	id := tenant.DefaultID
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
	store := NewVaultSecretStore("secret", fake)
	id := tenant.DefaultID

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
	// Tenant-scoped path isolation: value stored under the tenant subtree.
	if len(fake.data) != 1 {
		t.Fatalf("expected one vault path, got %d", len(fake.data))
	}
	for p := range fake.data {
		if want := "secret/openrails/tenants/" + id.String() + "/" + SecretStripeSecretKey; p != want {
			t.Fatalf("vault path = %q, want %q", p, want)
		}
	}
}
