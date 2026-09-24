package embed

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/requestauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// A typo'd key must fail loudly instead of provisioning a merchant with no rails.
func TestParseMerchantConfigIsStrict(t *testing.T) {
	m, err := ParseMerchantConfig([]byte(`
display_name: Host One
psps:
  mobius:
    nmi:
      account_id: "100001"
      secrets: {security_key: sk, webhook_signing_secret: whs}
`))
	require.NoError(t, err)
	require.Equal(t, "100001", m.PSPs["mobius"]["nmi"].AccountID)

	for doc, want := range map[string]string{
		"display_name: X\nacounts: {}\n":                "acounts",
		"display_name: X\nrail_merchant_accounts: {}\n": "rail_merchant_accounts was renamed to psps",
		"display_name: X\nprovider_accounts: {}\n":      "provider_accounts was renamed to psps",
	} {
		_, err := ParseMerchantConfig([]byte(doc))
		require.ErrorContains(t, err, want)
	}
}

// A catalog-owner clone is attenuated: the selector travels as a header, the
// transport principal never becomes that subject, and nothing the clone
// exposes can widen scope or reach admin operations.
func TestCatalogOwnerClientCannotExpandScope(t *testing.T) {
	mid := merchant.ID(uuid.New())
	product := openrails.ProductID(uuid.New())
	const subject = "作者 / external:123"
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "/v1/catalog/products/"+product.String(), r.URL.Path)
		owner, err := base64.RawURLEncoding.DecodeString(r.Header.Get("OpenRails-Catalog-Owner"))
		require.NoError(t, err)
		require.Equal(t, subject, string(owner))
		principal, ok := requestauth.HostPrincipalFromContext(r.Context())
		require.True(t, ok)
		require.Equal(t, mid, principal.MerchantID)
		require.Empty(t, principal.Subject)
		require.NoError(t, json.NewEncoder(w).Encode(openrails.CatalogProduct{ID: product}))
	})
	transport, capability := inprocess.NewTransport(handler, func() merchant.ID { return mid })
	admin, err := openrails.NewRemote(inprocessBaseURL, openrails.WithMerchantID(mid), openrails.WithHTTPClient(&http.Client{Transport: transport}),
		openrails.WithTokenProvider(func(context.Context) (string, error) { return capability, nil }))
	require.NoError(t, err)
	_, err = admin.ForCatalogOwner("")
	require.Error(t, err)
	owner, err := admin.ForCatalogOwner(subject)
	require.NoError(t, err)
	_, err = owner.Products.Retrieve(t.Context(), product.String())
	require.NoError(t, err)

	_, err = owner.ProductAccess.Check(t.Context(), &openrails.ProductAccessCheckParams{CustomerID: uuid.NewString(), ProductID: product.String()})
	require.ErrorIs(t, err, openrails.ErrDenied, "resource handles bind to the attenuated clone")
	_, err = owner.ListCatalogs(t.Context(), openrails.PageOptions{})
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = owner.EnsureCatalogForOwner(t.Context(), "another")
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = owner.ForCatalogOwner("another")
	require.ErrorIs(t, err, openrails.ErrDenied)
	require.Equal(t, 1, calls, "denied operations never reach the transport")
}
