//go:build e2e && integration

package destructive_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/fxfake"
	"github.com/stretchr/testify/require"
)

type failingStripe struct{ calls atomic.Int64 }

func (s *failingStripe) RoundTrip(*http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return nil, errors.New("simulated Stripe outage")
}

// A catalog edit whose provider call fails commits nothing, so it releases its
// unit: the actor keeps its whole allowance.
func TestDestructiveFailedProviderCatalogEditReleasesItsUnit(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	if dsn == "" {
		t.Fatal("OPENRAILS_E2E_DSN must point at a disposable PostgreSQL database")
	}
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	schema := "destructive_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		pool.Close()
	})
	fx := fxfake.New()
	t.Cleanup(fx.Close)
	stripe := &failingStripe{}
	cfg := openrails.Config{
		Database:          openrails.DatabaseConfig{Schema: schema, RiverSchema: schema},
		TestMode:          openrails.Sandbox,
		ProviderWriteMode: openrails.ProviderWritesFull,
		ReturnOrigins:     []string{"https://e2e.test"},
		Merchants: map[string]openrails.MerchantDeclaration{"failed-provider-edit": {DisplayName: "Failed provider edit",
			PSPs: map[string]openrails.PSPConfig{"stripe": {Rail: "stripe", AccountID: "acct_e2e",
				Secrets: map[string]string{"secret_key": "sk_test_e2e", "webhook_signing_secret": "whsec_e2e"}}}}},
	}
	engine, err := openrails.New(t.Context(), cfg, openrails.Deps{Postgres: pool, FXTransport: fx.Transport(), StripeTransport: stripe})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
	client, err := engine.MerchantBySlug("failed-provider-edit")
	require.NoError(t, err)
	staff := openrails.WithInvoker(t.Context(), openrails.Invoker{Issuer: "https://staff.example", ID: "catalog-editor"})

	product, err := client.CreateProduct(staff, billing.CreateProductParams{Key: "member", DisplayName: "Member"})
	require.NoError(t, err)
	hours := 720
	price, err := client.CreatePrice(staff, billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 9_990_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
	require.NoError(t, err)
	require.Zero(t, stripe.calls.Load(), "a price without a PSP link calls no provider")

	_, err = client.UpdatePrice(staff, price.ID, billing.UpdatePriceParams{PSPs: map[string]catalog.Field[map[string]string]{"stripe": catalog.Value(map[string]string{"lookup_key": "member-monthly"})}})
	require.Error(t, err)
	require.Positive(t, stripe.calls.Load(), "the edit reached its provider")
	var state string
	require.NoError(t, pool.QueryRow(t.Context(), "SELECT state FROM "+pgx.Identifier{schema}.Sanitize()+".destructive_actions WHERE target=$1", "price/"+price.ID.String()).Scan(&state))
	require.Equal(t, "no_effect", state, "a failed edit is released, not left uncertain")
	stored, err := client.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Empty(t, stored.PSPs, "the failed edit linked nothing")

	for i := range 5 {
		_, err := client.UpdateProduct(staff, product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{fmt.Sprintf("member:%d", i)})})
		require.NoError(t, err, "edit %d of the actor's five", i+1)
	}
	_, err = client.UpdateProduct(staff, product.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"member:held"})})
	var refusal *billing.StatusError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, billing.CodeRateLimitExceeded, refusal.Code)
}
