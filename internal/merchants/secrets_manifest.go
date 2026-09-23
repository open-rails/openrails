package merchants

import (
	"context"
	"fmt"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrManifestSecretsReadOnly is returned for runtime writes against the MODE-1
// credential plane (#723): in merchant_config_source=manifest the boot YAML is the
// truth — rotate the value in the manifest/secret files and reboot.
var ErrManifestSecretsReadOnly = fmt.Errorf("merchants: host-owned snapshot credentials are read-only; supply a new host snapshot")

// ManifestSecretStore is the MODE-1 (#723) credential plane: an in-memory,
// merchant-namespaced store seeded from the boot manifest (env + secret-file
// overlays applied). It implements the SAME MerchantSecretStore interface every
// consumer (checkout, webhooks, #699 pulls, #725/#730 charge resolvers) already
// reads, so "store wins" is vacuously true — the manifest IS the store.
// Runtime writes are refused (ErrManifestSecretsReadOnly); only the manifest
// provisioning path writes, through Seeder().
type ManifestSecretStore struct {
	mem      MerchantSecretStore
	identity string
}

// NewManifestSecretStore builds an empty MODE-1 store; boot provisioning seeds it.
func NewManifestSecretStore() *ManifestSecretStore {
	return &ManifestSecretStore{mem: NewMemorySecretStore(), identity: "snapshot"}
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
// ONLY (boot / constructor restarts). Runtime code consumes the store
// itself and is read-only.
func (s *ManifestSecretStore) Seeder() MerchantSecretStore { return s.mem }

// NewManifestSecretStoreWithIdentity labels custody supplied and retained by the
// host. The ID does not prove an external file exists; every restart must supply
// the same labeled snapshot and all credentials required by published references.
func NewManifestSecretStoreWithIdentity(identity string) (*ManifestSecretStore, error) {
	store := NewManifestSecretStore()
	if identity == "" {
		return store, nil
	}
	id, err := uuid.Parse(identity)
	if err != nil || id == uuid.Nil || id.String() != identity {
		return nil, fmt.Errorf("snapshot identity must be a canonical nonzero UUID")
	}
	store.identity = "snapshot:" + identity
	return store, nil
}
