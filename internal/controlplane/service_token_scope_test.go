package controlplane

import (
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestResolvedServiceTokenAllowsCustomerScopes(t *testing.T) {
	subject := uuid.New()
	other := uuid.New()

	tenantWide := &ResolvedServiceToken{
		MerchantID: dbtest.TestMerchantID,
		Resources:  []authcore.ServiceTokenResource{MerchantResource(dbtest.TestMerchantID)},
	}
	require.True(t, tenantWide.AllowsCustomer(subject))

	subjectScoped := &ResolvedServiceToken{
		MerchantID: dbtest.TestMerchantID,
		Resources: []authcore.ServiceTokenResource{
			MerchantResource(dbtest.TestMerchantID),
			CustomerResource(subject),
		},
	}
	require.True(t, subjectScoped.AllowsCustomer(subject))
	require.False(t, subjectScoped.AllowsCustomer(other))
}

func TestValidateServiceTokenResourcesRequiresTenantAndKnownKinds(t *testing.T) {
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestMerchantID, nil), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestMerchantID, []authcore.ServiceTokenResource{
		CustomerResource(uuid.New()),
	}), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestMerchantID, []authcore.ServiceTokenResource{
		MerchantResource(dbtest.TestMerchantID),
		{Kind: "openrails.unknown", ID: "x"},
	}), ErrServiceTokenScopeDenied)
	require.NoError(t, validateServiceTokenResources(dbtest.TestMerchantID, []authcore.ServiceTokenResource{
		MerchantResource(dbtest.TestMerchantID),
		CustomerResource(uuid.New()),
	}))
}
