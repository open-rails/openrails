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
		MerchantID: dbtest.TestTenantID,
		Resources:  []authcore.ServiceTokenResource{MerchantResource(dbtest.TestTenantID)},
	}
	require.True(t, tenantWide.AllowsCustomer(subject))

	subjectScoped := &ResolvedServiceToken{
		MerchantID: dbtest.TestTenantID,
		Resources: []authcore.ServiceTokenResource{
			MerchantResource(dbtest.TestTenantID),
			CustomerResource(subject),
		},
	}
	require.True(t, subjectScoped.AllowsCustomer(subject))
	require.False(t, subjectScoped.AllowsCustomer(other))
}

func TestValidateServiceTokenResourcesRequiresTenantAndKnownKinds(t *testing.T) {
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, nil), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		CustomerResource(uuid.New()),
	}), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		MerchantResource(dbtest.TestTenantID),
		{Kind: "openrails.unknown", ID: "x"},
	}), ErrServiceTokenScopeDenied)
	require.NoError(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		MerchantResource(dbtest.TestTenantID),
		CustomerResource(uuid.New()),
	}))
}
