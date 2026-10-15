package merchantbinding

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestMerchantBindingRequiresCanonicalIndependentIdentities(t *testing.T) {
	binding := billing.MerchantBinding{ID: billing.MerchantID(uuid.New()), TenantID: uuid.NewString()}
	require.NoError(t, Validate(binding))
	for _, tenant := range []string{"", "not-a-uuid", uuid.Nil.String(), " " + binding.TenantID} {
		bad := binding
		bad.TenantID = tenant
		require.Error(t, Validate(bad), tenant)
	}
	bad := binding
	bad.ID = billing.MerchantID{}
	require.Error(t, Validate(bad))
}

func TestMerchantBindingChecksAllLiveGroupMetadata(t *testing.T) {
	binding := billing.MerchantBinding{ID: billing.MerchantID(uuid.New()), TenantID: uuid.NewString()}
	group := billing.MerchantGroup{ID: binding.ID.String(), TenantID: binding.TenantID, Persona: billing.MerchantGroupPersona}
	require.NoError(t, ValidateGroup(binding, group))
	for name, change := range map[string]func(*billing.MerchantGroup){
		"foreign ID":     func(g *billing.MerchantGroup) { g.ID = uuid.NewString() },
		"foreign tenant": func(g *billing.MerchantGroup) { g.TenantID = uuid.NewString() },
		"wrong persona":  func(g *billing.MerchantGroup) { g.Persona = "app" },
		"deleted":        func(g *billing.MerchantGroup) { g.Deleted = true },
	} {
		t.Run(name, func(t *testing.T) {
			bad := group
			change(&bad)
			require.Error(t, ValidateGroup(binding, bad))
		})
	}
}

func TestRecordedTenantCannotLackItsMerchantGroup(t *testing.T) {
	tenant := uuid.New()
	binding := billing.MerchantBinding{ID: billing.MerchantID(uuid.New()), TenantID: tenant.String()}
	require.True(t, conflictingBinding(binding, nil, &tenant))
	group := binding.ID.String()
	require.False(t, conflictingBinding(binding, &group, &tenant))
	require.False(t, conflictingBinding(binding, nil, nil))
}
