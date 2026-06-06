package controlplane

import (
	"testing"

	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/pkg/tenant"
)

func TestResolvedOATAllowsTenantSubjectScopes(t *testing.T) {
	payer := uuid.New()
	other := uuid.New()

	tenantWide := &ResolvedOAT{
		TenantID:  tenant.DefaultID,
		Resources: []authcore.OrgAccessTokenResource{TenantResource(tenant.DefaultID)},
	}
	require.True(t, tenantWide.AllowsTenantSubject(payer))

	payerScoped := &ResolvedOAT{
		TenantID: tenant.DefaultID,
		Resources: []authcore.OrgAccessTokenResource{
			TenantResource(tenant.DefaultID),
			TenantSubjectResource(payer),
		},
	}
	require.True(t, payerScoped.AllowsTenantSubject(payer))
	require.False(t, payerScoped.AllowsTenantSubject(other))
}

func TestValidateOATResourcesRequiresTenantAndKnownKinds(t *testing.T) {
	require.ErrorIs(t, validateOATResources(tenant.DefaultID, nil), ErrOATScopeDenied)
	require.ErrorIs(t, validateOATResources(tenant.DefaultID, []authcore.OrgAccessTokenResource{
		TenantSubjectResource(uuid.New()),
	}), ErrOATScopeDenied)
	require.ErrorIs(t, validateOATResources(tenant.DefaultID, []authcore.OrgAccessTokenResource{
		TenantResource(tenant.DefaultID),
		{Kind: "openrails.unknown", ID: "x"},
	}), ErrOATScopeDenied)
	require.NoError(t, validateOATResources(tenant.DefaultID, []authcore.OrgAccessTokenResource{
		TenantResource(tenant.DefaultID),
		TenantSubjectResource(uuid.New()),
	}))
}
