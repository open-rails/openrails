package controlplane

import (
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/tenant"
)

func TestResolvedServiceTokenAllowsTenantSubjectScopes(t *testing.T) {
	subject := uuid.New()
	other := uuid.New()

	tenantWide := &ResolvedServiceToken{
		TenantID:  tenant.DefaultID,
		Resources: []authcore.ServiceTokenResource{TenantResource(tenant.DefaultID)},
	}
	require.True(t, tenantWide.AllowsTenantSubject(subject))

	subjectScoped := &ResolvedServiceToken{
		TenantID: tenant.DefaultID,
		Resources: []authcore.ServiceTokenResource{
			TenantResource(tenant.DefaultID),
			TenantSubjectResource(subject),
		},
	}
	require.True(t, subjectScoped.AllowsTenantSubject(subject))
	require.False(t, subjectScoped.AllowsTenantSubject(other))
}

func TestValidateOATResourcesRequiresTenantAndKnownKinds(t *testing.T) {
	require.ErrorIs(t, validateServiceTokenResources(tenant.DefaultID, nil), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(tenant.DefaultID, []authcore.ServiceTokenResource{
		TenantSubjectResource(uuid.New()),
	}), ErrServiceTokenScopeDenied)
	require.ErrorIs(t, validateServiceTokenResources(tenant.DefaultID, []authcore.ServiceTokenResource{
		TenantResource(tenant.DefaultID),
		{Kind: "openrails.unknown", ID: "x"},
	}), ErrServiceTokenScopeDenied)
	require.NoError(t, validateServiceTokenResources(tenant.DefaultID, []authcore.ServiceTokenResource{
		TenantResource(tenant.DefaultID),
		TenantSubjectResource(uuid.New()),
	}))
}
