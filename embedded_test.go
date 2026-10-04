package openrails

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/http/inprocess"
	"github.com/open-rails/openrails/internal/requestauth"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// River decodes a job by kind into the worker's args type, so the public args
// must mirror the engine job field for field or host inserts silently drop data.
func TestInvoiceSweepArgsMirrorEngineInvoiceJob(t *testing.T) {
	public, internal := reflect.TypeOf(InvoiceSweepArgs{}), reflect.TypeOf(riverjobs.InvoiceArgs{})
	require.Equal(t, internal.NumField(), public.NumField())
	for i := range internal.NumField() {
		want := internal.Field(i)
		got, ok := public.FieldByName(want.Name)
		require.True(t, ok, want.Name)
		require.Equal(t, want.Type, got.Type, want.Name)
		require.Equal(t, want.Tag, got.Tag, want.Name)
	}
	require.Equal(t, riverjobs.InvoiceArgs{}.Kind(), InvoiceSweepArgs{}.Kind())
	require.Equal(t, QueueBilling, InvoiceSweepArgs{}.InsertOpts().Queue)
}

// Hosting operations refuse a remote client instead of pretending.
func TestRemoteClientRefusesHostingOperations(t *testing.T) {
	c, err := NewRemote("https://billing.example", WithAPIKey("k"))
	require.NoError(t, err)
	require.ErrorIs(t, c.Start(t.Context()), ErrRemoteClient)
	_, err = c.Routes()
	require.ErrorIs(t, err, ErrRemoteClient)
	require.False(t, c.RoutesRequireRoot())
	require.Nil(t, c.Probes())
	_, err = c.ProvisionMerchant(t.Context(), billing.ProvisionMerchantRequest{Slug: "x"})
	require.ErrorIs(t, err, ErrRemoteClient)
	_, err = c.DeclarePSP(t.Context(), billing.MerchantID(uuid.New()), billing.PSPDeclaration{})
	require.ErrorIs(t, err, ErrRemoteClient)
	require.Nil(t, c.AuthKit())
	require.NoError(t, c.Close(t.Context()))
}

// New authenticates as the host over its own transport: the options that
// configure a remote client's credential or transport are refused before
// anything opens, never dropped.
func TestNewRefusesRemoteOptions(t *testing.T) {
	token := func(context.Context) (string, error) { return "t", nil }
	for name, opt := range map[string]ClientOption{
		"WithAPIKey":             WithAPIKey("k"),
		"WithTokenProvider":      WithTokenProvider(token),
		"WithHTTPClient":         WithHTTPClient(&http.Client{}),
		"WithCredentialProvider": WithCredentialProvider(func(context.Context, CredentialTarget) (string, error) { return "t", nil }),
	} {
		_, err := New(t.Context(), Config{}, Deps{}, opt)
		require.ErrorContains(t, err, "New runs in process", name)
	}
	_, err := New(t.Context(), Config{}, Deps{}, WithMerchantID(billing.MerchantID{}))
	require.ErrorContains(t, err, "merchant ID must not be zero", "an invalid option fails before the engine is built")
}

// Only the Client a constructor returned owns the engine and transport.
func TestDerivedClientCannotClose(t *testing.T) {
	c, err := NewRemote("https://billing.example", WithAPIKey("k"))
	require.NoError(t, err)
	derived, err := c.With(WithTimeout(1))
	require.NoError(t, err)
	owner, err := c.ForCatalogOwner("author")
	require.NoError(t, err)
	again, err := derived.With()
	require.NoError(t, err)
	for _, d := range []*Client{derived, owner, again} {
		require.ErrorContains(t, d.Close(t.Context()), "derived client")
	}
	require.NoError(t, c.Close(t.Context()))
}

// A catalog-owner clone is attenuated: the selector travels as a header, the
// transport principal never becomes that subject, and nothing the clone
// exposes can widen scope or reach admin operations.
func TestCatalogOwnerClientCannotExpandScope(t *testing.T) {
	mid := billing.MerchantID(uuid.New())
	product := billing.ProductID(uuid.New())
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
		require.NoError(t, json.NewEncoder(w).Encode(billing.CatalogProduct{ID: product}))
	})
	transport, capability := inprocess.NewTransport(handler, func() billing.MerchantID { return mid })
	admin, err := NewRemote(engine.InprocessBaseURL, WithMerchantID(mid), WithHTTPClient(&http.Client{Transport: transport}),
		WithTokenProvider(func(context.Context) (string, error) { return capability, nil }))
	require.NoError(t, err)
	_, err = admin.ForCatalogOwner("")
	require.Error(t, err)
	owner, err := admin.ForCatalogOwner(subject)
	require.NoError(t, err)
	_, err = owner.Products.Retrieve(t.Context(), product.String())
	require.NoError(t, err)

	_, err = owner.ProductAccess.Check(t.Context(), &billing.ProductAccessCheckParams{CustomerID: uuid.NewString(), ProductID: product.String()})
	require.ErrorIs(t, err, billing.ErrDenied, "resource handles bind to the attenuated clone")
	_, err = owner.ListCatalogs(t.Context(), billing.PageOptions{})
	require.ErrorIs(t, err, billing.ErrDenied)
	_, err = owner.EnsureCatalogForOwner(t.Context(), "another")
	require.ErrorIs(t, err, billing.ErrDenied)
	_, err = owner.ForCatalogOwner("another")
	require.ErrorIs(t, err, billing.ErrDenied)
	require.Equal(t, 1, calls, "denied operations never reach the transport")
}
