package embed

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCatalogClientCannotInheritAdministratorAuthority(t *testing.T) {
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
			principal, ok := requestauth.HostPrincipalFromContext(r.Context())
			require.True(t, ok)
			require.Equal(t, subject, principal.Subject)
			require.Equal(t, mid, principal.MerchantID)
			require.ElementsMatch(t, []string{permissions.MerchantCatalogOwnRead, permissions.MerchantCatalogOwnUpdate}, principal.Permissions)
			require.False(t, billingauth.HasPermission(principal.Permissions, permissions.MerchantCatalogUpdate))
			// Request-local mutations cannot upgrade the next request's grant set.
			principal.Permissions[0] = permissions.MerchantAll
			require.NoError(t, json.NewEncoder(w).Encode(openrails.CatalogProduct{ID: product}))
		})
	})
	admin, err := rt.Client()
	require.NoError(t, err)
	unsafeOptionUsed := false
	owner, err := rt.CatalogClient(subject,
		func(c *openrails.Client) { *c = *admin },
		openrails.WithHTTPClient(&http.Client{Transport: catalogOptionTransport(func(*http.Request) (*http.Response, error) {
			unsafeOptionUsed = true
			return nil, errors.New("unsafe transport override")
		})}),
		openrails.WithTokenProvider(func(context.Context) (string, error) { unsafeOptionUsed = true; return "administrator-credential", nil }),
	)
	require.NoError(t, err)
	ambient := requestauth.WithHostPrincipal(t.Context(), &requestauth.HostPrincipal{MerchantID: mid, Subject: "forged owner", Permissions: []string{permissions.MerchantAll}})
	for range 2 {
		_, err := owner.GetProduct(ambient, product)
		require.NoError(t, err)
	}
	require.Equal(t, 2, calls)
	require.False(t, unsafeOptionUsed)
	_, err = rt.CatalogClient(subject, openrails.WithMerchantID(merchant.ID(uuid.New())))
	require.Error(t, err, "merchant options remain assertions, not authority to switch a bound runtime")
	_, err = rt.CatalogClient("")
	require.Error(t, err, "empty subject cannot select default administrator authority")
}

type catalogOptionTransport func(*http.Request) (*http.Response, error)

func (f catalogOptionTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
