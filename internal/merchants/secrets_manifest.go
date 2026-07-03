package merchants

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrManifestSecretsReadOnly is returned for runtime writes against the MODE-1
// credential plane (#723): in merchant_source=manifest the boot YAML is the
// truth — rotate the value in the manifest/secret files and reboot.
var ErrManifestSecretsReadOnly = fmt.Errorf("merchants: merchant_source=manifest holds credentials in memory from the boot manifest; edit the YAML/secret files and reboot (#723)")

// ManifestSecretStore is the MODE-1 (#723) credential plane: an in-memory,
// merchant-namespaced store seeded from the boot manifest (env + secret-file
// overlays applied). It implements the SAME MerchantSecretStore interface every
// consumer (checkout, webhooks, #699 pulls, #725/#730 charge resolvers) already
// reads, so "store wins" is vacuously true — the manifest IS the store.
// Runtime writes are refused (ErrManifestSecretsReadOnly); only the manifest
// provisioning path writes, through Seeder().
type ManifestSecretStore struct {
	mem MerchantSecretStore
}

// NewManifestSecretStore builds an empty MODE-1 store; boot provisioning seeds it.
func NewManifestSecretStore() *ManifestSecretStore {
	return &ManifestSecretStore{mem: NewMemorySecretStore()}
}

func (s *ManifestSecretStore) Get(ctx context.Context, merchantID merchant.ID, name string) (Secret, error) {
	return s.mem.Get(ctx, merchantID, name)
}

func (s *ManifestSecretStore) List(ctx context.Context, merchantID merchant.ID) ([]string, error) {
	return s.mem.List(ctx, merchantID)
}

func (s *ManifestSecretStore) Put(_ context.Context, _ merchant.ID, name, _ string) (Secret, error) {
	return Secret{}, fmt.Errorf("put %s: %w", name, ErrManifestSecretsReadOnly)
}

func (s *ManifestSecretStore) Delete(_ context.Context, _ merchant.ID, name string) error {
	return fmt.Errorf("delete %s: %w", name, ErrManifestSecretsReadOnly)
}

// Seeder returns the write-capable facet for the manifest provisioning path
// ONLY (boot / UpsertMerchantConfig re-runs). Runtime code consumes the store
// itself and is read-only.
func (s *ManifestSecretStore) Seeder() MerchantSecretStore { return s.mem }
