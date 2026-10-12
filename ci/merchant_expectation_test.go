//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

func TestMerchantExpectationPreventsMisfiledAndRotatedCredentialWrites(t *testing.T) {
	f := newFixture(t)
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"a": {DisplayName: "A"}, "b": {DisplayName: "B"}}
	engine, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
	a, err := engine.MerchantBySlug("a")
	require.NoError(t, err)
	b, err := engine.MerchantBySlug("b")
	require.NoError(t, err)
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, engine, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))
	server := httptest.NewServer(mux)
	defer server.Close()
	keyA, keyB := bind(auth, a.MerchantID()).key, bind(auth, b.MerchantID()).key
	token := keyB
	remote, err := openrails.NewRemote(server.URL, openrails.RemoteOptions{Credentials: map[billing.MerchantID]openrails.RemoteCredential{
		a.MerchantID(): {Token: func(context.Context) (string, error) { return token, nil }},
	}})
	require.NoError(t, err)
	client, err := remote.Merchant(a.MerchantID())
	require.NoError(t, err)
	customer := billing.CustomerID(uuid.New())
	params := []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 10_000, Source: "test", SourceID: "expectation"}}
	var mismatch *openrails.BoundMerchantError
	_, err = client.CreateCreditGrants(t.Context(), params)
	require.ErrorAs(t, err, &mismatch, "first call's known merchant guards a misfiled credential")
	require.Equal(t, b.MerchantID(), mismatch.Bound)
	for _, shop := range []*openrails.Client{a, b} {
		grants, err := shop.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Empty(t, grants.Items, "neither merchant received the refused grant")
	}

	token = keyA
	_, err = client.CreateCreditGrants(t.Context(), params)
	require.NoError(t, err)
	// A fresh per-call token cannot switch the client to another merchant.
	token = keyB
	params[0].SourceID = "after-rotation"
	_, err = client.CreateCreditGrants(t.Context(), params)
	require.ErrorAs(t, err, &mismatch)
	grants, err := b.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Empty(t, grants.Items)
	require.EqualValues(t, 10_000, balanceOf(t, a, customer))

	// The single-merchant constructor adopts from a read, then guards writes.
	token = keyA
	single, err := openrails.NewRemoteMerchant(server.URL, openrails.RemoteCredential{Token: func(context.Context) (string, error) { return token, nil }})
	require.NoError(t, err)
	_, err = single.GetAdminAccess(t.Context())
	require.NoError(t, err)
	require.Equal(t, a.MerchantID(), single.MerchantID())
	token = keyB
	_, err = single.CreateCreditGrants(t.Context(), params)
	require.ErrorAs(t, err, &mismatch)
	grants, err = b.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Empty(t, grants.Items)
}
