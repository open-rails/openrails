package service

import (
	"testing"

	"github.com/open-rails/openrails/billing"
	catalogwire "github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestProductEntitlementPatchPresence(t *testing.T) {
	omitted, err := productPatch(billing.UpdateProductParams{})
	require.NoError(t, err)
	require.False(t, omitted.SetEntitlements)
	empty, err := productPatch(billing.UpdateProductParams{Entitlements: catalogwire.Value([]string{})})
	require.NoError(t, err)
	require.True(t, empty.SetEntitlements)
	require.NotNil(t, empty.Entitlements)
	require.Empty(t, empty.Entitlements)
	for _, value := range []catalogwire.Field[[]string]{catalogwire.Null[[]string](), catalogwire.Value[[]string](nil)} {
		_, err := productPatch(billing.UpdateProductParams{Entitlements: value})
		require.Error(t, err)
	}
}
