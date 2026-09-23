package merchants

import (
	"net/http"

	"github.com/open-rails/openrails/internal/shared/apperr"
)

var ErrCredentialCustodyTransitionRequired = apperr.New(http.StatusConflict, "credential_custody_transition_required", "credential custody differs from the published backend; a qualified custody transition is required")

// SecretCustodyIdentity binds published references to infrastructure selected by
// the host. It contains no token/password and is never returned by credential
// configuration APIs. Selecting a different backend is not a custody migration.
func SecretCustodyIdentity(store MerchantSecretReader) string {
	switch s := store.(type) {
	case *cachedSecretStore:
		return SecretCustodyIdentity(s.inner)
	case *lifecycleSecretStore:
		return SecretCustodyIdentity(s.MerchantSecretStore)
	case *encryptedSecretStore:
		return SecretCustodyIdentity(s.inner)
	case *readOnlySecretStore:
		return SecretCustodyIdentity(s.MerchantSecretStore)
	case *ManifestSecretStore:
		if s.identity == "" {
			return "snapshot"
		}
		return s.identity
	case *manifestManagedSecretStore:
		return SecretCustodyIdentity(s.manifest)
	case *dbSecretStore:
		return "db:" + s.database.DataPool().Schema()
	case *vaultSecretStore:
		if identity, ok := s.client.(interface{ BackendIdentity() string }); ok && identity.BackendIdentity() != "" {
			return "vault:" + identity.BackendIdentity() + "/" + s.mount + "/" + s.prefix
		}
	}
	return ""
}
