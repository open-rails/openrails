//go:build integration

package embed_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This test deliberately removes the database's tenant filter, only in a new
// scratch database. All requests still use a normal runtime login and real
// authorization/handler/service paths. Knowing another merchant's UUID must
// not become authority to read or change that merchant's records.
func TestMerchantScopeWithoutRLS(t *testing.T) {
	for _, owner := range []bool{false, true} {
		name := "normal_login"
		if owner {
			name = "database_owner"
		}
		t.Run(name, func(t *testing.T) { scopeWithoutRLSJourney(t, owner) })
	}
}

func scopeWithoutRLSJourney(t *testing.T, owner bool) {
	ctx := t.Context()
	admin, pool, dsn := scopeWithoutRLSDatabase(t)
	if owner {
		pool = admin
		ownerURL, err := url.Parse(dsn)
		require.NoError(t, err)
		credentials := admin.Config().ConnConfig
		ownerURL.User = url.UserPassword(credentials.User, credentials.Password)
		dsn = ownerURL.String()
	}
	newRuntime := func(slug string) *embed.Runtime {
		var declaration *embed.MerchantDeclaration
		if slug != "" {
			declaration = &embed.MerchantDeclaration{Slug: slug, Config: embed.MerchantConfig{DisplayName: slug}}
		}
		rt, err := embed.New(ctx, embed.Options{
			Merchant: declaration,
			Config: &config.Config{
				Env: "dev", TestMode: config.CredentialPostureSandbox,
				MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB,
				ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: dsn},
			},
			PGXPool: pool, River: embed.RiverManagedByOpenRails(),
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, rt.Close(context.Background())) })
		return rt
	}
	a := scopeWithoutRLSSeed(t, newRuntime("scope-a"), "scope-a")
	b := scopeWithoutRLSSeed(t, newRuntime("scope-b"), "scope-b")
	platform := newRuntime("") // The trusted host may explicitly select a merchant.

	rows, err := admin.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'billing' AND rowsecurity ORDER BY tablename`)
	require.NoError(t, err)
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	for _, table := range tables {
		_, err := admin.Exec(ctx, "ALTER TABLE "+pgx.Identifier{"billing", table}.Sanitize()+" DISABLE ROW LEVEL SECURITY")
		require.NoError(t, err)
	}
	var remaining int
	require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE schemaname = 'billing' AND rowsecurity`).Scan(&remaining))
	require.Zero(t, remaining, "no scratch billing table may provide an RLS safety net")
	var visible int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM billing.products`).Scan(&visible))
	require.Equal(t, 2, visible, "runtime login must really see both merchants without a SQL predicate")

	t.Run("authorized_A_cannot_read_B_known_product_or_price_ID", func(t *testing.T) {
		_, err := a.client.Products.Retrieve(ctx, b.product.ID)
		assert.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = a.client.Prices.Retrieve(ctx, b.price.ID)
		assert.ErrorIs(t, err, openrails.ErrNotFound)
	})
	t.Run("lists_and_counts_include_only_A", func(t *testing.T) {
		products, err := a.client.Products.List(ctx, &openrails.ProductListParams{})
		require.NoError(t, err)
		assert.EqualValues(t, 1, products.Total)
		if assert.Len(t, products.Items, 1) {
			assert.Equal(t, a.product.ID, products.Items[0].ID)
		}
		prices, err := a.client.Prices.List(ctx, &openrails.PriceListParams{})
		require.NoError(t, err)
		assert.EqualValues(t, 1, prices.Total)
		if assert.Len(t, prices.Items, 1) {
			assert.Equal(t, a.price.ID, prices.Items[0].ID)
		}
	})
	t.Run("authorized_A_cannot_update_B_product", func(t *testing.T) {
		title := "cross-merchant overwrite"
		_, err := a.client.Products.Update(ctx, b.product.ID, &openrails.ProductUpdateParams{DisplayName: &title})
		assert.ErrorIs(t, err, openrails.ErrNotFound)
		got, err := b.client.Products.Retrieve(ctx, b.product.ID)
		require.NoError(t, err)
		assert.Equal(t, b.product.DisplayName, got.DisplayName, "B's actual row must remain unchanged")
	})
	t.Run("authorized_A_cannot_write_a_key_for_B_price", func(t *testing.T) {
		_, err := a.client.Prices.SetKey(ctx, b.price.ID, "cross-merchant-price-key")
		assert.ErrorIs(t, err, openrails.ErrNotFound)
		got, err := b.client.Prices.Retrieve(ctx, b.price.ID)
		require.NoError(t, err)
		assert.Equal(t, b.price.Key, got.Key)
	})
	t.Run("A_price_cannot_reference_B_product", func(t *testing.T) {
		_, err := a.client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: b.product.ID, Key: "foreign-product-reference", UnitAmount: 123, Currency: "USD"})
		assert.ErrorIs(t, err, openrails.ErrNotFound, "foreign products must be rejected before a composite FK violation")
		var count int
		require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM billing.prices WHERE merchant_id=$1 AND product_id=$2`, a.mid.UUID(), sdkProductID(t, b.product.ID).UUID()).Scan(&count))
		assert.Zero(t, count, "rejection must leave no cross-merchant reference")
	})
	t.Run("A_cannot_read_or_pay_B_invoice", func(t *testing.T) {
		_, err := a.client.GetMerchantInvoice(ctx, b.invoice)
		assert.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = a.client.RecordInvoicePayment(ctx, b.invoice, openrails.RecordInvoicePaymentRequest{Amount: 1, Reference: "foreign-invoice-reference"})
		assert.ErrorIs(t, err, openrails.ErrNotFound)
		got, err := b.client.GetMerchantInvoice(ctx, b.invoice)
		require.NoError(t, err)
		assert.EqualValues(t, 500, got.AmountDue, "B's receivable cannot be mutated by A")
		invoices, total, err := a.client.ListMerchantInvoices(ctx, openrails.MerchantInvoiceFilter{}, 10, 0)
		require.NoError(t, err)
		assert.EqualValues(t, 1, total)
		if assert.Len(t, invoices, 1) {
			assert.Equal(t, a.invoice, invoices[0].ID)
		}
	})
	t.Run("A_cannot_reference_B_only_customer", func(t *testing.T) {
		// This API explicitly does not create customers. EnsureCustomer is a
		// different contract: the same host subject can exist under both merchants.
		_, err := a.client.SetCustomerBillingPolicy(ctx, (b.customer).String(), nil)
		assert.ErrorIs(t, err, openrails.ErrNotFound)
		var count int
		require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM billing.customers WHERE merchant_id=$1 AND id=$2`, a.mid.UUID(), b.customer.UUID()).Scan(&count))
		assert.Zero(t, count)
	})
	t.Run("A_cannot_delete_B_customer_delegation", func(t *testing.T) {
		err := a.client.DeleteCustomerSpendDelegation(ctx, (b.customer).String(), "invoker", "scope-worker")
		assert.ErrorIs(t, err, openrails.ErrNotFound)
		var count int
		require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM billing.invoker_spend_limits WHERE merchant_id=$1 AND customer_id=$2 AND scope_key='scope-worker'`, b.mid.UUID(), b.customer.UUID()).Scan(&count))
		assert.Equal(t, 1, count)
	})
	t.Run("HTTP_authorization_for_A_does_not_authorize_B_resource", func(t *testing.T) {
		handler, err := httptesthost.Handler(a.runtime, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true}, Gate: scopeWithoutRLSGate{mid: a.mid}})
		require.NoError(t, err)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/merchant/catalog/products/"+b.product.ID, nil))
		assert.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	})
	t.Run("missing_merchant_fails_closed", func(t *testing.T) {
		unselected, err := platform.Client()
		require.NoError(t, err)
		_, err = unselected.Products.Retrieve(ctx, b.product.ID)
		assert.ErrorIs(t, err, openrails.ErrInvalid, "a reusable client never invents an operation target")
		_, err = app.HostGraph(platform).Runtime.ProductService.GetByID(ctx, sdkProductID(t, b.product.ID).UUID())
		assert.Error(t, err, "service access without a merchant must fail before unscoped SQL")
		handler, err := httptesthost.Handler(platform, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true}, Gate: scopeWithoutRLSGate{}})
		require.NoError(t, err)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/merchant/catalog/products/"+b.product.ID, nil))
		assert.GreaterOrEqual(t, response.Code, 400, "a gate result without merchant identity cannot authorize data access")
		assert.NotContains(t, response.Body.String(), b.product.Key)
	})
	t.Run("trusted_platform_selection_can_act_for_B", func(t *testing.T) {
		client, err := platform.Client(openrails.WithMerchantID(b.mid))
		require.NoError(t, err)
		got, err := client.Products.Retrieve(ctx, b.product.ID)
		require.NoError(t, err)
		assert.Equal(t, b.product.ID, got.ID)
		name := "authorized B update"
		updated, err := client.Products.Update(ctx, b.product.ID, &openrails.ProductUpdateParams{DisplayName: &name})
		require.NoError(t, err)
		assert.Equal(t, name, updated.DisplayName)
	})
}

type scopeWithoutRLSMerchant struct {
	runtime  *embed.Runtime
	client   *openrails.Client
	mid      merchant.ID
	product  *openrails.Product
	price    *openrails.Price
	customer openrails.CustomerID
	invoice  uuid.UUID
}

func scopeWithoutRLSSeed(t *testing.T, rt *embed.Runtime, slug string) scopeWithoutRLSMerchant {
	t.Helper()
	ctx := t.Context()
	client, err := rt.Client()
	require.NoError(t, err)
	mid := client.MerchantID()
	product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: slug + "-product", DisplayName: slug})
	require.NoError(t, err)
	price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: slug + "-price", UnitAmount: 1000, Currency: "USD"})
	require.NoError(t, err)
	customer := openrails.CustomerID(uuid.New())
	_, err = client.EnsureCustomer(ctx, (customer).String())
	require.NoError(t, err)
	require.NoError(t, client.SetCustomerSpendDelegations(ctx, (customer).String(), []openrails.SpendDelegationInput{{Scope: "invoker", ScopeKey: "scope-worker", Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 1000000, Currency: "USD"}}}}))
	scoped := merchant.WithID(ctx, mid)
	graph := app.HostGraph(rt).Runtime
	mode := money.BillingModeArrears
	require.NoError(t, graph.DB.RunInMerchantConn(scoped, func(c context.Context) error {
		_, err := graph.MoneyService.UpsertAccountSettings(c, identity.CustomerID(customer), "USD", money.AccountSettingsInput{BillingMode: &mode})
		return err
	}))
	_, err = client.EnsureCustomerInvoiceProfile(ctx, (customer).String(), openrails.InvoiceProfileDTO{NetTermsDays: 7, CollectionMethod: "send_invoice"})
	require.NoError(t, err)
	_, err = graph.MoneyService.AccrueOwed(scoped, identity.CustomerID(customer), "USD", "scope-test", uuid.NewString(), 500)
	require.NoError(t, err)
	invoice, err := graph.MoneyService.FinalizeInvoice(scoped, identity.CustomerID(customer), "USD", time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
	require.NoError(t, err)
	return scopeWithoutRLSMerchant{runtime: rt, client: client, mid: mid, product: product, price: price, customer: customer, invoice: invoice.ID}
}

type scopeWithoutRLSGate struct{ mid merchant.ID }

func (g scopeWithoutRLSGate) Authorize(_ context.Context, _ *http.Request, permission string) (billingauth.Principal, error) {
	return billingauth.Principal{MerchantID: g.mid, Permissions: []string{permission}}, nil
}

func scopeWithoutRLSDatabase(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_TEST_DB_URL")
	if dsn == "" {
		dsn = os.Getenv("OPENRAILS_TEST_DB_DSN")
	}
	if dsn == "" {
		t.Skip("set OPENRAILS_TEST_DB_URL to an administrative PostgreSQL URL; test creates its own database and login")
	}
	ctx := t.Context()
	cluster, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(cluster.Close)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	database, role := "scope_no_rls_"+suffix, "scope_app_"+suffix
	_, err = cluster.Exec(ctx, "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS PASSWORD 'scope_test'")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := cluster.Exec(context.Background(), "DROP ROLE "+pgx.Identifier{role}.Sanitize())
		require.NoError(t, err)
	})
	_, err = cluster.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{database}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := cluster.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{database}.Sanitize()+" WITH (FORCE)")
		require.NoError(t, err)
	})
	adminCfg := cluster.Config()
	adminCfg.ConnConfig.Database = database
	admin, err := pgxpool.NewWithConfig(ctx, adminCfg)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	appCfg := adminCfg.Copy()
	appCfg.ConnConfig.User, appCfg.ConnConfig.Password = role, "scope_test"
	pool, err := pgxpool.NewWithConfig(ctx, appCfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	require.NoError(t, embed.ApplyMigrations(ctx, admin, embed.MigrationOptions{RuntimePool: pool}))
	var privileged bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&privileged))
	require.False(t, privileged)
	runtimeURL, err := url.Parse(dsn)
	require.NoError(t, err)
	require.NotEmpty(t, runtimeURL.Host, "test administrator connection must be a PostgreSQL URL")
	runtimeURL.Path, runtimeURL.User = "/"+database, url.UserPassword(role, "scope_test")
	t.Logf("RLS-off qualification database %s; runtime role %s", database, role)
	return admin, pool, runtimeURL.String()
}
