package merchants

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrSecretStoreReadOnly identifies an explicitly read-only managed store.
// Backend outages and unexpected ACL failures retain their separate error class.
var ErrSecretStoreReadOnly = apperr.New(http.StatusForbidden, "credential_store_read_only", "credential store is read-only")

type readOnlySecretStore struct{ MerchantSecretStore }

func NewReadOnlySecretStore(inner MerchantSecretStore) MerchantSecretStore {
	return &readOnlySecretStore{inner}
}
func (s *readOnlySecretStore) Put(context.Context, merchant.ID, string, string) (Secret, error) {
	return Secret{}, ErrSecretStoreReadOnly
}
func (s *readOnlySecretStore) Delete(context.Context, merchant.ID, string) error {
	return ErrSecretStoreReadOnly
}
func (s *readOnlySecretStore) GetVersion(ctx context.Context, id merchant.ID, name string, version int) (Secret, error) {
	return ReadSecretRef(ctx, s.MerchantSecretStore, id, SecretRef{Name: name, MinVersion: version})
}

// credentialWriteRefusal preserves the actionable snapshot instruction while
// classifying an intentionally read-only managed backend as a permission refusal.
func credentialWriteRefusal(store MerchantSecretStore) error {
	custody := SecretCustodyIdentity(store)
	if custody == "snapshot" || strings.HasPrefix(custody, "snapshot:") {
		return ErrManifestSecretsReadOnly
	}
	return ErrSecretStoreReadOnly
}
