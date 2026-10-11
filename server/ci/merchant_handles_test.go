//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/server"
)

// handle is the server engine's handle of merchant mid.
func handle(t *testing.T, srv *server.Server, mid billing.MerchantID) *openrails.Client {
	t.Helper()
	h, err := srv.Engine().Merchant(mid)
	require.NoError(t, err)
	return h
}

// productKeys are the keys of the products c's merchant has.
func productKeys(t *testing.T, c *openrails.Client) []string {
	t.Helper()
	page, err := c.ListProducts(t.Context(), billing.ProductListParams{})
	require.NoError(t, err)
	keys := []string{}
	for _, p := range page.Items {
		keys = append(keys, p.Key)
	}
	return keys
}

// The server's engine acts for each of its merchants through that merchant's
// handle: what one writes the other never reads.
func TestServerEngineHandles(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	a, b := provision(t, srv, uniqueName("handle-a")), provision(t, srv, uniqueName("handle-b"))
	ha, hb := handle(t, srv, a), handle(t, srv, b)
	require.Equal(t, a, ha.MerchantID())
	require.Equal(t, b, hb.MerchantID())

	for key, h := range map[string]*openrails.Client{"only-a": ha, "only-b": hb} {
		_, err := h.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key})
		require.NoError(t, err)
	}
	require.Equal(t, []string{"only-a"}, productKeys(t, ha))
	require.Equal(t, []string{"only-b"}, productKeys(t, hb))
	// In turn, on one engine: the first merchant again after the second.
	_, err := ha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "also-a", DisplayName: "Also A"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"only-a", "also-a"}, productKeys(t, ha))
	require.Equal(t, []string{"only-b"}, productKeys(t, hb))

	_, err = srv.Engine().MerchantBySlug("anything")
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved, "a server's Config declares no merchant: its handles are by ID")
}

// A merchant provisioned while the server runs is served at once, with no
// Config entry and no restart; deleting it ends its handle's calls, and
// restoring it brings them back.
func TestHandleForAMerchantProvisionedAtRunTime(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	ctx := t.Context()
	// A handle made before its merchant exists: making one reads nothing.
	early := handle(t, srv, billing.MerchantID(uuid.New()))
	_, err := early.GetCatalogRevision(ctx)
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved)

	mid := provision(t, srv, uniqueName("runtime"))
	shop := handle(t, srv, mid)
	_, err = shop.CreateProduct(ctx, billing.CreateProductParams{Key: "pro", DisplayName: "Pro"})
	require.NoError(t, err)
	require.Equal(t, []string{"pro"}, productKeys(t, shop))

	_, err = srv.DeleteMerchant(ctx, mid)
	require.NoError(t, err)
	_, err = shop.ListProducts(ctx, billing.ProductListParams{})
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved)
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusForbidden, refusal.Status)
	require.Equal(t, billing.CodeMerchantUnresolved, refusal.Code)
	_, err = shop.CreateProduct(ctx, billing.CreateProductParams{Key: "late", DisplayName: "Late"})
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved, "a deleted merchant takes no write")

	_, err = srv.RestoreMerchant(ctx, mid)
	require.NoError(t, err)
	require.Equal(t, []string{"pro"}, productKeys(t, shop), "the same handle works again")
}

// recorded is a real HTTP listener in front of the server that keeps every
// request's headers.
type recorded struct {
	*httptest.Server
	mu      sync.Mutex
	headers []http.Header
}

func record(t *testing.T, handler http.Handler) *recorded {
	t.Helper()
	r := &recorded{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.headers = append(r.headers, req.Header.Clone())
		r.mu.Unlock()
		handler.ServeHTTP(w, req)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *recorded) seen() []http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]http.Header(nil), r.headers...)
}

// grantTo credits customer at c's merchant, which makes it a customer there.
func grantTo(t *testing.T, c *openrails.Client, customer billing.CustomerID) {
	t.Helper()
	_, err := c.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: "handles"}})
	require.NoError(t, err)
}

// Over HTTP a handle is its credential: with one API key per merchant, each
// handle reaches its own merchant's customers and no other's, and no request
// names a merchant.
func TestRemoteHandlesActAsTheirCredential(t *testing.T) {
	f := newFixture(t)
	srv := f.newServer(t, nil)
	a, b := provision(t, srv, uniqueName("remote-a")), provision(t, srv, uniqueName("remote-b"))
	handler, err := standaloneHandler(srv)
	require.NoError(t, err)
	api := record(t, handler)
	keyA, keyB := merchantKey(t, srv, a, "owner").Secret, merchantKey(t, srv, b, "owner").Secret

	remote, err := openrails.NewRemote(api.URL, openrails.RemoteOptions{
		Credentials: map[billing.MerchantID]openrails.RemoteCredential{a: {APIKey: keyA}, b: {APIKey: keyB}},
		Timeout:     30 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = remote.Close(context.Background()) })
	ra, err := remote.Merchant(a)
	require.NoError(t, err)
	rb, err := remote.Merchant(b)
	require.NoError(t, err)
	_, err = remote.Merchant(provision(t, srv, uniqueName("remote-c")))
	require.ErrorIs(t, err, billing.ErrMerchantUnresolved, "a live merchant with no credential here")

	alice, bob := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	grantTo(t, ra, alice)
	grantTo(t, rb, bob)
	customers := func(c *openrails.Client) []billing.CustomerID {
		t.Helper()
		page, err := c.ListCustomers(t.Context(), billing.CustomerListParams{})
		require.NoError(t, err)
		ids := []billing.CustomerID{}
		for _, customer := range page.Items {
			ids = append(ids, customer.ID)
		}
		return ids
	}
	require.Equal(t, []billing.CustomerID{alice}, customers(ra))
	require.Equal(t, []billing.CustomerID{bob}, customers(rb))
	_, err = ra.GetCustomer(t.Context(), bob)
	require.ErrorIs(t, err, billing.ErrNotFound, "another merchant's customer is not this handle's")
	// The engine's own handles agree with what each key wrote.
	require.Equal(t, []billing.CustomerID{alice}, customers(handle(t, srv, a)))

	single, err := openrails.NewRemoteMerchant(api.URL, openrails.RemoteCredential{APIKey: keyA})
	require.NoError(t, err)
	require.Equal(t, []billing.CustomerID{alice}, customers(single))

	requests := api.seen()
	require.NotEmpty(t, requests)
	for _, header := range requests {
		require.Empty(t, header.Values("OpenRails-Merchant"), "no Go client names a merchant")
	}
}
