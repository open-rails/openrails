package controlplane

import (
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestResolvedServiceTokenAllowsTenantSubjectScopes(t *testing.T) {
	subject := uuid.New()
	other := uuid.New()

	tenantWide := &ResolvedServiceToken{
		TenantID:  dbtest.TestTenantID,
		Resources: []authcore.ServiceTokenResource{TenantResource(dbtest.TestTenantID)},
	}
	require.True(t, tenantWide.AllowsTenantSubject(subject))

	subjectScoped := &ResolvedServiceToken{
		TenantID: dbtest.TestTenantID,
		Resources: []authcore.ServiceTokenResource{
			TenantResource(dbtest.TestTenantID),
			TenantSubjectResource(subject),
		},
	}
	require.True(t, subjectScoped.AllowsTenantSubject(subject))
	require.False(t, subjectScoped.AllowsTenantSubject(other))
}

func TestValidateServiceTokenResourcesRequiresTenantAndKnownKinds(t *testing.T) {
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, nil), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		TenantSubjectResource(uuid.New()),
	}), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		TenantResource(dbtest.TestTenantID),
		{Kind: "openrails.unknown", ID: "x"},
	}), ErrServiceTokenScopeDenied)
	require.NoError(t, validateServiceTokenResources(dbtest.TestTenantID, []authcore.ServiceTokenResource{
		TenantResource(dbtest.TestTenantID),
		TenantSubjectResource(uuid.New()),
	}))
}
