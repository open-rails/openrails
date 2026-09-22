//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Each deployment owns a merchant. A fixture's catalog, policies or PSPs must
// never alter another workflow's provider selection or admission decisions.
type clientWorkflowDeployment struct {
	name      string
	client    *openrails.Client
	peer      *openrails.Client
	stranger  *openrails.Client
	authority *integrationharness.Surface
	slug      string
	mid       merchant.ID
	runtime   *app.Runtime
	url       string
	token     string
}

func clientWorkflowDeployments(t *testing.T, h *integrationharness.Harness) []clientWorkflowDeployment {
	t.Helper()
	remote := h.StartStandalone("USD")
	peerRuntime, err := embed.New(context.Background(), embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantConfigSource: config.MerchantConfigSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeFull, DB: &config.DBConfig{URL: h.DSN}},
		Redis:  h.Redis, River: embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, peerRuntime.Close(context.Background())) })
	var out []clientWorkflowDeployment
	for _, mode := range []string{"embedded", "hosted_http", "standalone"} {
		owned := remote.ProvisionOwnedMerchant("client-" + uuid.NewString()[:8])
		d := clientWorkflowDeployment{name: mode, mid: owned.MerchantID, runtime: remote.App().Runtime, url: remote.BaseURL, token: owned.APIKey, slug: owned.MerchantSlug, authority: remote, stranger: remote.Client()}
		if mode == "standalone" {
			d.client = remote.Client(openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(owned.MerchantID))
		} else {
			host := h.StartEmbeddedMerchant("USD", owned.MerchantID, owned.MerchantSlug, func(cfg *config.Config) {
				cfg.MerchantConfigSource = config.MerchantConfigSourceAPI
				cfg.SecretBackend = config.SecretBackendDB
				cfg.ProviderWriteMode = config.ProviderWriteModeFull
			})
			d.runtime, d.url, d.token = app.HostGraph(host.Runtime()).Runtime, host.BaseURL, host.Token
			if mode == "embedded" {
				var err error
				d.client, err = host.Runtime().Client(openrails.WithCurrency("USD"))
				require.NoError(t, err)
			} else {
				d.client = host.Client()
			}
		}
		out = append(out, d)
	}
	hosted := h.StartHosted("USD")
	tenant := hosted.ProvisionMerchant(hosted.RegisterUser("client-owner"), "client-"+uuid.NewString()[:8])
	out = append(out, clientWorkflowDeployment{name: "saas", mid: tenant.ID, client: tenant.Client(), runtime: hosted.AppRuntime(), url: hosted.BaseURL, token: tenant.APIKey, stranger: remote.Client()})
	for i := range out {
		out[i].peer, err = peerRuntime.Client(openrails.WithMerchantID(out[i].mid), openrails.WithCurrency("USD"))
		require.NoError(t, err)
	}
	return out
}

func TestClientCatalogAndAccessWorkflow(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	for _, d := range clientWorkflowDeployments(t, h) {
		t.Run(d.name, func(t *testing.T) {
			client := d.client
			merchantUUID := d.mid.UUID()
			integrationharness.SeedPSPs(ctx, t, d.runtime, d.mid, config.PSPSet{
				"ccbill": {AccountID: "999981-" + new(big.Int).SetBytes(merchantUUID[:]).String(), CCBill: &config.CCBillRailConfig{Salt: "client-workflow-local-signing"}},
			})
			group := "client-catalog-" + uuid.NewString()
			product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: group, DisplayName: "Initial", TierGroup: &group, EntitlementsSpec: map[string]*int{"access": nil}})
			require.NoError(t, err)
			id, err := client.Products.Ensure(ctx, &openrails.ProductCreateParams{Key: group, DisplayName: "Must not overwrite existing"})
			require.NoError(t, err)
			require.Equal(t, product.ID, id.ID)
			read, err := client.Products.RetrieveByKey(ctx, group)
			require.NoError(t, err)
			require.Equal(t, "Initial", read.DisplayName)
			title := "Renamed"
			updated, err := client.Products.Update(ctx, product.ID, &openrails.ProductUpdateParams{DisplayName: &title})
			require.NoError(t, err)
			require.Equal(t, title, updated.DisplayName)
			require.Contains(t, updated.EntitlementsSpec, "access")
			_, err = client.Products.Retrieve(ctx, (openrails.ProductID(uuid.New())).String())
			require.ErrorIs(t, err, openrails.ErrNotFound)

			live, archived := false, true
			retired, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: group + "-retired", DisplayName: "Retired", TierGroup: &group})
			require.NoError(t, err)
			_, err = client.Products.Update(ctx, retired.ID, &openrails.ProductUpdateParams{Archived: &archived})
			require.NoError(t, err)
			duration := 720
			price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: group + "-monthly", UnitAmount: 1234567, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			require.EqualValues(t, 1234567, price.UnitAmount)
			byKey, err := client.Prices.RetrieveByKey(ctx, price.Key)
			require.NoError(t, err)
			require.Equal(t, price.ID, byKey.ID)
			byID, err := client.Prices.Retrieve(ctx, price.ID)
			require.NoError(t, err)
			require.Equal(t, product.ID, byID.ProductID)
			oldPrice, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: group + "-old", UnitAmount: 1000000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
			require.NoError(t, err)
			_, err = client.Prices.Update(ctx, oldPrice.ID, &openrails.PriceUpdateParams{Archived: &archived})
			require.NoError(t, err)
			for _, tc := range []struct {
				archived *bool
				products []string
				prices   []string
			}{
				{nil, []string{product.ID, retired.ID}, []string{price.ID, oldPrice.ID}},
				{&live, []string{product.ID}, []string{price.ID}},
				{&archived, []string{retired.ID}, []string{oldPrice.ID}},
			} {
				products, err := client.Products.List(ctx, &openrails.ProductListParams{TierGroup: group, Archived: tc.archived})
				require.NoError(t, err)
				require.EqualValues(t, len(tc.products), products.Total)
				var ids []string
				for _, p := range products.Items {
					ids = append(ids, p.ID)
				}
				require.ElementsMatch(t, tc.products, ids)
				prices, err := client.Prices.List(ctx, &openrails.PriceListParams{ProductID: product.ID, Archived: tc.archived})
				require.NoError(t, err)
				require.EqualValues(t, len(tc.prices), prices.Total)
				var priceIDs []string
				for _, p := range prices.Items {
					priceIDs = append(priceIDs, p.ID)
				}
				require.ElementsMatch(t, tc.prices, priceIDs)
			}
			_, err = client.Prices.Update(ctx, price.ID, &openrails.PriceUpdateParams{Archived: &archived})
			require.NoError(t, err)
			prices, err := client.Prices.List(ctx, &openrails.PriceListParams{ProductID: product.ID, Archived: &live})
			require.NoError(t, err)
			require.Empty(t, prices.Items)
			byID, err = client.Prices.Retrieve(ctx, price.ID)
			require.NoError(t, err)
			require.True(t, byID.Archived, "archived obligations keep an addressable price identity")
			checkClientDeclaredAccess(t, ctx, h, d)
			checkClientPlanMigration(t, ctx, h, d)
			t.Run("checkout", func(t *testing.T) { checkClientCheckout(t, ctx, h, d) })
		})
	}
}

func checkClientDeclaredAccess(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client := d.client
	customer := openrails.CustomerID(uuid.New())
	first, err := client.EnsureCustomer(ctx, (customer).String())
	require.NoError(t, err)
	require.Equal(t, customer.String(), first.ID)
	require.False(t, first.CreatedAt.IsZero())
	again, err := client.EnsureCustomer(ctx, (customer).String())
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	require.True(t, again.CreatedAt.Equal(first.CreatedAt))
	require.False(t, again.LastSeenAt.Before(first.LastSeenAt))
	_, err = client.EnsureCustomer(ctx, (openrails.CustomerID{}).String())
	require.ErrorIs(t, err, openrails.ErrInvalid)
	product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "access-" + uuid.NewString(), DisplayName: "Membership", EntitlementsSpec: map[string]*int{"premium": nil}})
	require.NoError(t, err)
	bare, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "bare-" + uuid.NewString(), DisplayName: "No spec"})
	require.NoError(t, err)
	duration := 720
	price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: "access-" + uuid.NewString(), UnitAmount: 0, Currency: "USD", AccessDurationHours: &duration})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Microsecond)
	end := now.Add(48 * time.Hour)
	subscriber, comped := openrails.CustomerID(uuid.New()), openrails.CustomerID(uuid.New())
	source, trial := "grant-"+uuid.NewString(), "trial-"+uuid.NewString()
	book := openrails.DeclaredBilling{
		AsOf: now, DefaultPSP: openrails.PSPRef{Key: "ccbill"},
		Customers:     []openrails.DeclaredCustomer{{Customer: subscriber}},
		Subscriptions: []openrails.DeclaredSubscription{{SourceID: trial, Customer: subscriber, Price: sdkPriceID(t, price.ID), Rail: "ccbill", RailSubscriptionID: trial, StartedAt: now.Add(-time.Hour), PaidThrough: &end}},
		AdminGrants: []openrails.DeclaredAdminGrant{
			{Customer: comped, Product: sdkProductID(t, product.ID), SourceID: source, StartsAt: now.Add(-time.Hour), EndsAt: &end},
			{Customer: comped, Product: sdkProductID(t, bare.ID), SourceID: source + "-nospec", StartsAt: now},
		},
	}
	result, err := client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{source, trial}, result.Imported)
	require.Equal(t, []string{source + "-nospec"}, result.Blocked)
	require.Equal(t, "product has no entitlements_spec", result.Reasons[source+"-nospec"])
	hasPremium, err := client.HasEntitlement(ctx, (comped).String(), "premium", time.Time{})
	require.NoError(t, err)
	require.True(t, hasPremium)
	subscriptions, err := client.ListSubscriptions(ctx, openrails.SubscriptionFilter{CustomerID: (subscriber).String()})
	require.NoError(t, err)
	require.Len(t, subscriptions.Data, 1)
	require.Equal(t, price.ID, subscriptions.Data[0].Price.ID)
	replay, err := client.ImportBilling(ctx, book)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{source, trial}, replay.Skipped)
	require.Empty(t, replay.Imported)
	var windows int
	require.NoError(t, h.MerchantPool(d.mid.UUID()).QueryRow(ctx, `SELECT count(*) FROM billing.entitlements WHERE merchant_id=$1 AND customer_id=$2 AND source_type='admin'`, d.mid.UUID(), comped.UUID()).Scan(&windows))
	require.Equal(t, 1, windows)
	_, err = client.ImportBilling(ctx, openrails.DeclaredBilling{AdminGrants: book.AdminGrants})
	require.ErrorIs(t, err, openrails.ErrInvalid)

	// A permanent grant exercises null end times independently of the imported
	// finite window. Batch responses retain an explicit empty unknown customer.
	permanent, err := client.GrantEntitlement(ctx, (customer).String(), openrails.GrantEntitlementRequest{Entitlement: "staff"})
	require.NoError(t, err)
	require.Equal(t, "admin", permanent.SourceType)
	require.Nil(t, permanent.EndAt)
	ghost := openrails.CustomerID(uuid.New())
	batch, err := client.ListActiveEntitlements(ctx, []string{(customer).String(), (customer).String(), (comped).String(), (ghost).String()}, time.Time{})
	require.NoError(t, err)
	require.Len(t, batch, 3)
	require.Contains(t, batch, ghost)
	require.Empty(t, batch[(ghost).String()])
	require.Len(t, batch[(customer).String()], 1)
	require.Equal(t, customer, batch[(customer).String()][0].CustomerID)
	require.Equal(t, "staff", batch[(customer).String()][0].Entitlement)
	require.Equal(t, "admin", batch[(customer).String()][0].SourceType)
	require.Nil(t, batch[(customer).String()][0].EndAt)
	records, err := client.ListEntitlements(ctx, (customer).String(), time.Time{})
	require.NoError(t, err)
	require.Equal(t, batch[(customer).String()], records)
	for _, entitlement := range []string{"staff", "missing"} {
		has, err := client.HasEntitlement(ctx, (customer).String(), entitlement, time.Time{})
		require.NoError(t, err)
		require.Equal(t, entitlement == "staff", has)
	}
	require.ErrorIs(t, client.RevokeEntitlement(ctx, (ghost).String(), permanent.ID), openrails.ErrNotFound)
	require.NoError(t, client.RevokeEntitlement(ctx, (customer).String(), permanent.ID))
	has, err := client.HasEntitlement(ctx, (customer).String(), "staff", time.Time{})
	require.NoError(t, err)
	require.False(t, has)
	fixed, err := client.GrantEntitlement(ctx, (customer).String(), openrails.GrantEntitlementRequest{Entitlement: "beta", EndAt: &end})
	require.NoError(t, err)
	require.NotNil(t, fixed.EndAt)
	require.True(t, fixed.EndAt.Equal(end))
	hours := 1
	_, err = client.GrantEntitlement(ctx, (customer).String(), openrails.GrantEntitlementRequest{Entitlement: "beta", EndAt: &end, Hours: &hours})
	require.ErrorIs(t, err, openrails.ErrInvalid)
	// Product ownership and named entitlements are separate facts. Seed the
	// former through its real service, as the original conformance proof did.
	_, _, err = productaccess.NewService(h.MerchantDB(d.mid.UUID())).GrantProductAccess(merchant.WithID(ctx, d.mid), productaccess.GrantParams{
		UserID: comped.String(), ProductID: sdkProductID(t, product.ID).UUID(), SourceType: models.ProductAccessSourceAdmin, SourceID: source,
	})
	require.NoError(t, err)
	page, err := client.ProductAccess.List(ctx, &openrails.ProductAccessListParams{CustomerID: comped.String()})
	require.NoError(t, err)
	grants := page.Data
	require.Len(t, grants, 1)
	require.Equal(t, product.ID, grants[0].ProductID)
	require.Equal(t, comped.String(), grants[0].CustomerID)
	require.Equal(t, product.Key, grants[0].ProductKey)
	require.Equal(t, "admin", grants[0].SourceType)
	require.Equal(t, "active", grants[0].Status)
	require.Nil(t, grants[0].PaymentID)
	decisions, err := client.ProductAccess.CheckMany(ctx, &openrails.ProductAccessCheckManyParams{CustomerID: comped.String(), ProductIDs: []string{product.ID, product.ID, openrails.ProductID(uuid.New()).String()}})
	require.NoError(t, err)
	require.Len(t, decisions, 2)
	require.True(t, decisions[product.ID])
	require.False(t, page.HasMore)
	require.Empty(t, page.NextCursor)

	access, err := client.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: comped.String(), ProductID: product.ID})
	require.NoError(t, err)
	require.True(t, access.HasAccess)
	access, err = client.ProductAccess.Check(ctx, &openrails.ProductAccessCheckParams{CustomerID: comped.String(), ProductID: openrails.ProductID(uuid.New()).String()})
	require.NoError(t, err)
	require.False(t, access.HasAccess)
}

func checkClientPlanMigration(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client, mid := d.client, d.mid
	key := "operations-" + uuid.NewString()
	source, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: key + "-a", DisplayName: "Plan A"})
	require.NoError(t, err)
	target, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: key + "-b", DisplayName: "Plan B"})
	require.NoError(t, err)
	duration := 720
	sourcePrice, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: source.ID, Key: key + "-a-monthly", UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
	require.NoError(t, err)
	targetPrice, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: target.ID, Key: key + "-b-monthly", UnitAmount: 1_000_000, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
	require.NoError(t, err)
	renamed, err := client.Prices.SetKey(ctx, sourcePrice.ID, key+"-legacy")
	require.NoError(t, err)
	require.Equal(t, sourcePrice.ID, renamed.ID)
	byKey, err := client.Prices.RetrieveByKey(ctx, key+"-legacy")
	require.NoError(t, err)
	require.Equal(t, sourcePrice.ID, byKey.ID)

	subscriber, subscription := uuid.New(), uuid.New()
	psp := dbtest.EnsureTestPSP(ctx, t, h.Pool(), mid.UUID(), "ccbill")
	exec := func(sql string, args ...any) { _, err := h.Pool().Exec(ctx, sql, args...); require.NoError(t, err) }
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), subscriber)
	now := time.Now().UTC()
	exec(`INSERT INTO billing.subscriptions(id,merchant_id,customer_id,product_id,price_id,psp_id,rail,status,rail_subscription_id,current_period_starts_at,current_period_ends_at) VALUES($1,$2,$3,$4,$5,$6,'ccbill','active',$7,$8,$9)`,
		subscription, mid.UUID(), subscriber, sdkProductID(t, source.ID).UUID(), sdkPriceID(t, sourcePrice.ID).UUID(), psp, subscription.String(), now, now.Add(720*time.Hour))
	migration := openrails.PlanMigrationRequest{SourcePrice: key + "-legacy", TargetPrice: targetPrice.ID, FallbackPolicy: "keep_grandfathered"}
	preview, err := client.PreviewPlanMigration(ctx, migration)
	require.NoError(t, err)
	require.Nil(t, preview.BatchID)
	require.Equal(t, 1, preview.Matched)
	require.Equal(t, []openrails.PlanMigrationOutcome{{SubscriptionID: openrails.SubscriptionID(subscription), Rail: "ccbill", Disposition: preview.Outcomes[0].Disposition, Reason: preview.Outcomes[0].Reason}}, preview.Outcomes)
	require.NotEmpty(t, preview.Outcomes[0].Reason)
	created, err := client.CreatePlanMigration(ctx, migration)
	require.NoError(t, err)
	require.NotNil(t, created.BatchID)
	require.Equal(t, preview.Outcomes[0].Disposition, created.Outcomes[0].Disposition)
	canceled, err := client.CancelPlanMigration(ctx, *created.BatchID)
	require.NoError(t, err)
	require.Empty(t, canceled.RailReleaseRequired)
	_, err = client.PreviewPlanMigration(ctx, openrails.PlanMigrationRequest{SourcePrice: sourcePrice.ID, TargetPrice: uuid.NewString()})
	require.ErrorIs(t, err, openrails.ErrNotFound)
}

func checkClientCheckout(t *testing.T, ctx context.Context, h *integrationharness.Harness, d clientWorkflowDeployment) {
	t.Helper()
	client := d.client
	checkout, err := client.GetCheckoutConfig(ctx)
	require.NoError(t, err)
	require.Equal(t, "checkout_config", checkout.Object)
	require.Len(t, checkout.PSPs, 1)
	require.Equal(t, "ccbill", checkout.PSPs[0].Key)
	require.Equal(t, "redirect", checkout.PSPs[0].Flow)
	require.Empty(t, checkout.PSPs[0].Config)
	product, err := client.Products.Create(ctx, &openrails.ProductCreateParams{Key: "checkout-" + uuid.NewString(), DisplayName: "Hosted fixture"})
	require.NoError(t, err)
	duration := 720
	prices := make([]*openrails.Price, 0, 2)
	for _, amount := range []int64{10_000_000, 9_007_199_254_740_993} {
		price, err := client.Prices.Create(ctx, &openrails.PriceCreateParams{ProductID: product.ID, Key: "checkout-" + uuid.NewString(), UnitAmount: amount, Currency: "USD", AccessDurationHours: &duration, AutoRenew: true})
		require.NoError(t, err)
		psp := dbtest.EnsureTestPSP(ctx, t, h.MerchantPool(d.mid.UUID()), d.mid.UUID(), "ccbill")
		_, err = h.MerchantPool(d.mid.UUID()).Exec(ctx, `INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,flex_id,configuration) VALUES($1,$2,$3,$4,'{"form_name":"test-form"}')`, d.mid.UUID(), sdkPriceID(t, price.ID).UUID(), psp, uuid.NewString())
		require.NoError(t, err)
		prices = append(prices, price)
	}
	user := openrails.CustomerID(uuid.New())
	request := openrails.CreateCheckoutSessionRequest{Customer: openrails.CheckoutCustomerIdentity{ID: user.String(), VerifiedEmail: "checkout@example.test", Username: "checkout-" + uuid.NewString()[:8]}, PriceID: prices[0].ID, IdempotencyKey: uuid.NewString(), PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "ccbill", NameOnCard: "Test Buyer", Zip: "90210", Country: "US"}}
	options, err := client.ListCheckoutRailOptions(ctx, prices[0].ID)
	require.NoError(t, err)
	require.NotEmpty(t, options)
	first, err := client.CreateCheckoutSession(ctx, request)
	require.NoError(t, err)
	require.NotEmpty(t, first.ID)
	require.NotNil(t, first.URL)
	require.True(t, strings.HasPrefix(*first.URL, "https://"))
	require.False(t, first.CreatedAt.IsZero())
	require.NotNil(t, first.ExpiresAt)
	require.True(t, first.ExpiresAt.After(first.CreatedAt))
	again, err := client.CreateCheckoutSession(ctx, request)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	read, err := client.GetCheckoutSession(ctx, user.String(), first.ID)
	require.NoError(t, err)
	require.Equal(t, first.ID, read.ID)
	require.Equal(t, first.Amount, read.Amount)
	_, err = client.GetCheckoutSession(ctx, uuid.NewString(), first.ID)
	require.ErrorIs(t, err, openrails.ErrDenied)
	_, err = client.ConfirmCheckoutSession(ctx, first.ID, openrails.ConfirmCheckoutSessionRequest{CustomerID: uuid.NewString(), Payment: openrails.ConfirmPayment{Rail: "solana"}})
	require.ErrorIs(t, err, openrails.ErrDenied)
	require.NoError(t, client.Verify(ctx))
	tier, err := client.ResolveEffectiveTier(ctx, (user).String(), "membership")
	require.NoError(t, err)
	require.Nil(t, tier, "a redirect has not bought access")
	var documentReference []byte
	for _, reader := range []*openrails.Client{client, d.peer} {
		price, err := reader.Prices.RetrieveByKey(ctx, prices[1].Key)
		require.NoError(t, err)
		require.EqualValues(t, 9_007_199_254_740_993, price.UnitAmount)
		product, err := reader.Products.Retrieve(ctx, price.ProductID)
		require.NoError(t, err)
		plan, err := openrails.NewHostedCheckoutPlan(product, price)
		require.NoError(t, err)
		rails, err := reader.ListCheckoutRailOptions(ctx, (sdkPriceID(t, price.ID)).String())
		require.NoError(t, err)
		require.NotEmpty(t, rails)
		document := openrails.HostedCheckoutSession{ID: "ocs_parity", Status: "created", Merchant: openrails.HostedCheckoutMerchant{DisplayName: "Parity"}, Plan: plan, ExpiresAt: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)}
		for _, rail := range rails {
			driver, ok := openrails.HostedCheckoutDriver(rail.Rail)
			require.True(t, ok)
			document.Rails = append(document.Rails, openrails.HostedCheckoutRail{ID: "option_" + rail.PSPID, Rail: rail.Rail, Mode: rail.Mode, Driver: driver})
		}
		raw, err := json.Marshal(document)
		require.NoError(t, err)
		if documentReference == nil {
			documentReference = raw
		} else {
			require.JSONEq(t, string(documentReference), string(raw))
		}
		var wire map[string]any
		require.NoError(t, json.Unmarshal(raw, &wire))
		shape := wire["plan"].(map[string]any)
		require.Equal(t, "9007199254740993", shape["unit_amount"])
		require.Equal(t, "USD", shape["currency"])
		require.Equal(t, float64(6), shape["unit_decimals"])
		require.Equal(t, float64(720), shape["period_hours"])
		require.Equal(t, true, shape["automatically_renews"])
		require.Equal(t, "Hosted fixture", shape["display_name"])
	}
	wallet := solanago.NewWallet().PublicKey().String()
	d.runtime.SolanaMintDecimals = solanamodule.NewMintDecimals(fakeMintReader{decimals: 6})
	merchantUUID := d.mid.UUID()
	integrationharness.SeedPSPs(ctx, t, d.runtime, d.mid, config.PSPSet{
		"ccbill": {AccountID: "999981-" + new(big.Int).SetBytes(merchantUUID[:]).String(), CCBill: &config.CCBillRailConfig{Salt: "client-workflow-local-signing"}},
		"solana": {AccountID: wallet, Solana: &config.SolanaRailConfig{Tokens: map[string]config.TokenConfig{"USDC": {Name: "USD Coin"}}}},
	})
	discovery, err := client.GetCheckoutConfig(ctx)
	require.NoError(t, err)
	require.NotNil(t, discovery.Solana)
	require.Equal(t, "devnet", discovery.Solana.Network)
	require.Equal(t, "solana:devnet", discovery.Solana.Chain)
	require.Equal(t, "USDC", discovery.Solana.PreferredToken)
	require.Len(t, discovery.Solana.Tokens, 1)
	require.Equal(t, openrails.SolanaCheckoutToken{Symbol: "USDC", Name: "USD Coin", Mint: discovery.Solana.Tokens[0].Mint, Decimals: 6, Preferred: true, RecurringEligible: true}, discovery.Solana.Tokens[0])
	require.NotEmpty(t, discovery.Solana.Tokens[0].Mint)
}
