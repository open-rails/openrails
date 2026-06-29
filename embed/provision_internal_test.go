package embed

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/app"
)

func TestValidateCredentialMutationMode(t *testing.T) {
	err := validateCredentialMutationMode(app.CredentialModeFixed, ManifestMerchant{
		ProviderAccounts: []ManifestProviderAccount{{ProviderType: "nmi", AccountID: "mobius"}},
	})
	require.ErrorContains(t, err, "mutable_credentials")

	require.NoError(t, validateCredentialMutationMode(app.CredentialModeMutable, ManifestMerchant{
		ProviderAccounts: []ManifestProviderAccount{{ProviderType: "nmi", AccountID: "mobius"}},
	}))
	require.NoError(t, validateCredentialMutationMode(app.CredentialModeFixed, ManifestMerchant{}))
}
