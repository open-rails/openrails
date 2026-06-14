package controlplane

import (
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestResolvedServiceTokenAllowsMerchantSubjectScopes(t *testing.T) {
	subject := uuid.New()
	other := uuid.New()

	tenantWide := &ResolvedServiceToken{
		MerchantID:  dbtest.TestTenantID,
		Resources: []authcore.ServiceTokenResource{MerchantResource(dbtest.TestTenantID)},
	}
	require.True(t, tenantWide.AllowsMerchantSubject(subject))

	subjectScoped := &ResolvedServiceToken{
		MerchantID: dbtest.TestTenantID,
		Resources: []authcore.ServiceTokenResource{
			MerchantResource(dbtest.TestTenantID),
			MerchantSubjectResource(subject),
		},
	}
	require.True(t, subjectScoped.AllowsMerchantSubject(subject))
	require.False(t, subjectScoped.AllowsMerchantSubject(other))
}

func TestValidateServiceTokenResourcesRequiresTenantAndKnownKinds(t *testing.T) {
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, nil), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		MerchantSubjectResource(uuid.New()),
	}), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		MerchantResource(dbtest.TestTenantID),
		{Kind: "openrails.unknown", ID: "x"},
	}), ErrServiceTokenScopeDenied)
	require.NoError(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		MerchantResource(dbtest.TestTenantID),
		MerchantSubjectResource(uuid.New()),
	}))
}
