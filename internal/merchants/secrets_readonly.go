package merchants

import (
	"context"
	"github.com/open-rails/openrails/pkg/merchant"
)

type readOnlySecretStore struct{ MerchantSecretStore }

func NewReadOnlySecretStore(inner MerchantSecretStore) MerchantSecretStore {
	return &readOnlySecretStore{inner}
}
func (s *readOnlySecretStore) Put(context.Context, merchant.ID, string, string) (Secret, error) {
	return Secret{}, ErrManifestSecretsReadOnly
}
func (s *readOnlySecretStore) Delete(context.Context, merchant.ID, string) error {
	return ErrManifestSecretsReadOnly
}
func (s *readOnlySecretStore) GetVersion(ctx context.Context, id merchant.ID, name string, version int) (Secret, error) {
	return ReadSecretRef(ctx, s.MerchantSecretStore, id, SecretRef{Name: name, MinVersion: version})
}
