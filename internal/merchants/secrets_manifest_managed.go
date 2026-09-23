package merchants

import (
	"context"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/pkg/merchant"
)

// manifestManagedSecretStore keeps provider credentials in the read-only
// manifest while operator-owned webhook URLs use the configured durable store.
// The managed store is never a fallback for missing manifest provider names.
type manifestManagedSecretStore struct {
	manifest *ManifestSecretStore
	managed  MerchantSecretStore
}

func NewManifestManagedSecretStore(manifest *ManifestSecretStore, managed MerchantSecretStore) MerchantSecretStore {
	return &manifestManagedSecretStore{manifest: manifest, managed: managed}
}

func isAlertWebhookSecret(name string) bool {
	raw := strings.TrimSuffix(strings.TrimPrefix(name, "alert_webhooks/"), "/url")
	id, err := uuid.Parse(raw)
	return err == nil && AlertWebhookURLSecretName(id) == name
}

func (s *manifestManagedSecretStore) Get(ctx context.Context, id merchant.ID, name string) (Secret, error) {
	if isAlertWebhookSecret(name) {
		return s.managed.Get(ctx, id, name)
	}
	if s.manifest == nil {
		return Secret{}, ErrSecretNotFound
	}
	return s.manifest.Get(ctx, id, name)
}
func (s *manifestManagedSecretStore) GetAtLeastVersion(ctx context.Context, id merchant.ID, name string, version int) (Secret, error) {
	if isAlertWebhookSecret(name) {
		return ReadSecretRef(ctx, s.managed, id, SecretRef{Name: name, MinVersion: version})
	}
	// Manifest providers are the latest boot-seeded authority and have no
	// durable rotation watermark or read cache; preserve their existing contract.
	return s.Get(ctx, id, name)
}
func (s *manifestManagedSecretStore) Put(ctx context.Context, id merchant.ID, name, value string) (Secret, error) {
	if !isAlertWebhookSecret(name) {
		return Secret{}, ErrManifestSecretsReadOnly
	}
	return s.managed.Put(ctx, id, name, value)
}
func (s *manifestManagedSecretStore) Delete(ctx context.Context, id merchant.ID, name string) error {
	if !isAlertWebhookSecret(name) {
		return ErrManifestSecretsReadOnly
	}
	return s.managed.Delete(ctx, id, name)
}
func (s *manifestManagedSecretStore) List(ctx context.Context, id merchant.ID) ([]string, error) {
	names, err := s.managed.List(ctx, id)
	if err != nil {
		return nil, err
	}
	names = slices.DeleteFunc(names, func(name string) bool { return !isAlertWebhookSecret(name) })
	if s.manifest != nil {
		provider, err := s.manifest.List(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, name := range provider {
			if !isAlertWebhookSecret(name) {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// mutableSecretView excludes read-only manifest entries from durable purge.
// Their source remains the host-owned manifest; only managed URL secrets can be
// deleted by the runtime. List also filters unrelated names in that backend.
func mutableSecretView(store MerchantSecretStore) MerchantSecretStore {
	original := store
	for {
		switch wrapped := store.(type) {
		case *manifestManagedSecretStore:
			return &manifestManagedSecretStore{managed: wrapped.managed}
		case *lifecycleSecretStore:
			store = wrapped.MerchantSecretStore
		case *cachedSecretStore:
			store = wrapped.inner
		case *encryptedSecretStore:
			store = wrapped.inner
		case *writeRestrictedSecretStore:
			store = wrapped.inner
		default:
			return original
		}
	}
}

func (s *manifestManagedSecretStore) GetVersion(ctx context.Context, id merchant.ID, name string, version int) (Secret, error) {
	if isAlertWebhookSecret(name) {
		return ReadSecretRef(ctx, s.managed, id, SecretRef{Name: name, MinVersion: version})
	}
	return ReadSecretRef(ctx, s.manifest, id, SecretRef{Name: name, MinVersion: version})
}
