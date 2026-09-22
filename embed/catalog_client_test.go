package embed

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCatalogClientScopeCannotExpand(t *testing.T) {
	mid := merchant.ID(uuid.New())
	graph := &app.Runtime{}
	graph.SetConfiguredMerchant(mid)
	rt := &Runtime{app: &app.App{Runtime: graph}}
	product := openrails.ProductID(uuid.New())
	const subject = "作者 / external:123"
	calls := 0
	rt.handlerOnce.Do(func() {
		rt.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			require.Equal(t, "/v1/catalog/products/"+product.String(), r.URL.Path)
			owner, err := base64.RawURLEncoding.DecodeString(r.Header.Get("OpenRails-Catalog-Owner"))
			require.NoError(t, err)
			require.Equal(t, subject, string(owner))
			principal, ok := requestauth.HostPrincipalFromContext(r.Context())
			require.True(t, ok)
			require.Equal(t, mid, principal.MerchantID)
			require.Empty(t, principal.Subject, "the selector must never impersonate the authenticated actor")
			require.NoError(t, json.NewEncoder(w).Encode(openrails.CatalogProduct{ID: product}))
		})
	})
	admin, err := rt.Client()
	require.NoError(t, err)
	owner, err := admin.ForCatalogOwner(subject)
	require.NoError(t, err)
	_, err = owner.GetProduct(t.Context(), product)
	require.NoError(t, err)
	_, err = owner.ListCatalogs(t.Context(), openrails.PageOptions{})
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = owner.EnsureCatalogForOwner(t.Context(), "another")
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = owner.ForCatalogOwner("another")
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = admin.ForCatalogOwner("")
	require.Error(t, err)
	require.Equal(t, 1, calls, "admin methods must never reach the scoped transport")
}
