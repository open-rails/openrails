package contract

import (
	"reflect"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestCatalogEntitlementsAreNonnullableLists(t *testing.T) {
	for _, shape := range []reflect.Type{
		reflect.TypeFor[catalog.ApplyProduct](), reflect.TypeFor[billing.CreateProductParams](),
		reflect.TypeFor[billing.UpdateProductParams](), reflect.TypeFor[billing.Product](),
	} {
		found := false
		for _, field := range fieldsOf(shape) {
			require.NotEqual(t, "entitlements_spec", field.name)
			if field.name == "entitlements" {
				found = true
				require.False(t, field.nullable, shape.Name())
				require.Equal(t, reflect.TypeFor[[]string](), elem(field.t), shape.Name())
			}
		}
		require.True(t, found, shape.Name())
	}
}
