//go:build integration

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type catalogBoundaryTransport func(*http.Request) (*http.Response, error)

func (f catalogBoundaryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCatalogProviderPreparationAndPropagationOutsideTransaction(t *testing.T) {
	s, ctx := applicationService(t)
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	owner := dbtest.SharedSuperuserPGXPool(t)
	s.rt.Config.TestMode = config.CredentialPostureSandbox
	s.rt.Config.ProviderWriteMode = config.ProviderWriteModeFull
	s.rt.Config.NewSubscriptionCollectionPolicy = ""
	s.rt.RailConfigs = railresolve.FixedSet{"stripe": {Rail: models.RailStripe, Stripe: &config.StripeRailConfig{SecretKey: "sk_test_catalog_boundary"}}}
	_, err = owner.Exec(ctx, "INSERT INTO billing.psps(merchant_id,id,key,rail,environment,account_id) VALUES($1,$2,'stripe','stripe','test','acct_boundary')", mid.UUID(), uuid.New())
	require.NoError(t, err)
	product, err := s.CreateProduct(ctx, CreateProductRequest{Key: "boundary", DisplayName: "Boundary"})
	require.NoError(t, err)
	calls, posts := 0, 0
	var onCall func(*http.Request)
	cleanup := stripeapi.InstallBaseTransport(catalogBoundaryTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method == http.MethodPost {
			posts++
		}
		probe, err := owner.Begin(ctx)
		require.NoError(t, err)
		var locked uuid.UUID
		err = probe.QueryRow(ctx, "SELECT id FROM billing.merchants WHERE id=$1 FOR UPDATE NOWAIT", mid.UUID()).Scan(&locked)
		require.NoError(t, err, "provider traffic must not run while local transaction holds merchant lock")
		require.NoError(t, probe.Rollback(ctx))
		if onCall != nil {
			onCall(r)
		}
		payload := `{"id":"prod_boundary"}`
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/products/search":
			payload = `{"data":[]}`
		case r.Method == http.MethodGet && r.URL.Path == "/v1/prices":
			payload = `{"data":[]}`
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/features"):
			payload = `{"data":[]}`
		case strings.HasPrefix(r.URL.Path, "/v1/prices"):
			id := "price_boundary"
			if strings.Contains(r.URL.Path, "price_rotated") {
				id = "price_rotated"
			}
			payload = fmt.Sprintf(`{"id":%q,"product":"prod_boundary","unit_amount":100,"currency":"usd","active":true}`, id)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(payload)), Request: r}, nil
	}))
	t.Cleanup(cleanup)
	price, err := s.CreatePrice(ctx, CreatePriceRequest{ProductID: product.ID, Key: "boundary-price", Currency: "USD", UnitAmount: 1000000, PSPs: []string{"stripe"}})
	require.NoError(t, err)
	require.Greater(t, posts, 0, "exercise provider creation, not only provider reads")
	require.Equal(t, "price_boundary", price.Providers["stripe"].IDs["price_id"])
	onCall = func(r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/prices/") {
			var archived bool
			require.NoError(t, owner.QueryRow(ctx, "SELECT archived FROM billing.prices WHERE merchant_id=$1 AND id=$2", mid.UUID(), price.ID.UUID()).Scan(&archived))
			require.True(t, archived, "propagation sees committed local archive")
		}
	}
	_, err = s.DeactivatePrice(ctx, price.ID)
	require.NoError(t, err)
	onCall = func(r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/products/") {
			var name string
			require.NoError(t, owner.QueryRow(ctx, "SELECT display_name FROM billing.products WHERE merchant_id=$1 AND id=$2", mid.UUID(), product.ID.UUID()).Scan(&name))
			require.Equal(t, "Committed title", name)
		}
	}
	title := "Committed title"
	_, err = s.UpdateProduct(ctx, product.ID, UpdateProductRequest{DisplayName: &title})
	require.NoError(t, err)
	// Provider verification can overlap a separately committed API edit. The
	// prepared old snapshot must not overwrite that newly accepted state.
	onCall = func(r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "price_rotated") {
			_, e := owner.Exec(ctx, "UPDATE billing.prices SET key='concurrent-key' WHERE merchant_id=$1 AND id=$2", mid.UUID(), price.ID.UUID())
			require.NoError(t, e)
		}
	}
	before := calls
	_, err = s.UpdatePrice(ctx, price.ID, UpdatePriceRequest{PSPLinks: map[string]map[string]string{"stripe": {"price_id": "price_rotated"}}})
	require.ErrorIs(t, err, openrails.ErrConflict)
	require.Equal(t, before+1, calls, "a failed local revalidation never repeats provider preparation")
	preserved, err := s.GetPrice(ctx, price.ID)
	require.NoError(t, err)
	require.Equal(t, "concurrent-key", preserved.Key)
	require.Equal(t, "price_boundary", preserved.Providers["stripe"].IDs["price_id"])
	onCall = nil
	_, err = owner.Exec(ctx, `CREATE FUNCTION billing.reject_product_boundary() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.display_name='Reject this title' THEN RAISE EXCEPTION 'local boundary rejection'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_product_boundary BEFORE UPDATE ON billing.products FOR EACH ROW EXECUTE FUNCTION billing.reject_product_boundary();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), "DROP TRIGGER reject_product_boundary ON billing.products; DROP FUNCTION billing.reject_product_boundary();")
	})
	before = calls
	bad := "Reject this title"
	_, err = s.UpdateProduct(ctx, product.ID, UpdateProductRequest{DisplayName: &bad})
	require.ErrorContains(t, err, "local boundary rejection")
	require.Equal(t, before, calls, "failed local transaction must discard propagation")
}
