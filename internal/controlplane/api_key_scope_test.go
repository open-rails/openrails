package controlplane

import (
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestResolvedServiceCredentialAllowsCustomerScopes(t *testing.T) {
	subject := uuid.New()
	other := uuid.New()

	merchantWide := &ResolvedServiceCredential{
		MerchantID: dbtest.TestMerchantID,
		Resources:  []authcore.APIKeyResource{MerchantResource(dbtest.TestMerchantID)},
	}
	require.True(t, merchantWide.AllowsCustomer(subject))

	subjectScoped := &ResolvedServiceCredential{
		MerchantID: dbtest.TestMerchantID,
		Resources: []authcore.APIKeyResource{
			MerchantResource(dbtest.TestMerchantID),
			CustomerResource(subject),
		},
	}
	require.True(t, subjectScoped.AllowsCustomer(subject))
	require.False(t, subjectScoped.AllowsCustomer(other))
}

func TestValidateAPIKeyResourcesRequiresMerchantAndKnownKinds(t *testing.T) {
	require.ErrorIs(t, validateAPIKeyResources(dbtest.TestMerchantID, nil), ErrServiceCredentialScopeDenied)
	require.ErrorIs(t, validateAPIKeyResources(dbtest.TestMerchantID, []authcore.APIKeyResource{
		CustomerResource(uuid.New()),
	}), ErrServiceCredentialScopeDenied)
	require.ErrorIs(t, validateAPIKeyResources(dbtest.TestMerchantID, []authcore.APIKeyResource{
		MerchantResource(dbtest.TestMerchantID),
		{Kind: "openrails.unknown", ID: "x"},
	}), ErrServiceCredentialScopeDenied)
	require.NoError(t, validateAPIKeyResources(dbtest.TestMerchantID, []authcore.APIKeyResource{
		MerchantResource(dbtest.TestMerchantID),
		CustomerResource(uuid.New()),
	}))
}
