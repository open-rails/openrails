package checkout

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A one-time Stripe checkout prices inline from the accepted local terms, in
// whole minor units, under a derived provider idempotency key.
func TestStripeOneTimeCheckoutWire(t *testing.T) {
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}
	svc := &CheckoutService{Config: cfg, Rails: railresolve.FixedSet{"stripe": {Rail: models.RailStripe, AccountID: "acct_test", Stripe: &config.StripeRailConfig{SecretKey: "sk_test_synthetic"}}}}
	product := &models.Product{ID: uuid.New(), DisplayName: "Accepted post"}
	price := &models.Price{ID: uuid.New(), ProductID: product.ID, Amount: 12_340_000, Currency: "USD"}
	calls := 0
	svc.StripeClients = stripeapi.NewFactory(roundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "/v1/checkout/sessions", r.URL.Path)
		require.Equal(t, stripeCheckoutIdempotencyKey("accepted-inline"), r.Header.Get(stripeapi.IdempotencyKeyHeader))
		require.NoError(t, r.ParseForm())
		require.Equal(t, "payment", r.Form.Get("mode"))
		require.Empty(t, r.Form.Get("line_items[0][price]"))
		require.Equal(t, "usd", r.Form.Get("line_items[0][price_data][currency]"))
		require.Equal(t, "1234", r.Form.Get("line_items[0][price_data][unit_amount]"))
		require.Equal(t, "Accepted post", r.Form.Get("line_items[0][price_data][product_data][name]"))
		require.Equal(t, price.ID.String(), r.Form.Get("metadata[internal_price_id]"))
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"url":"https://checkout.stripe.test/accepted"}`))}, nil
	}))
	req := &CheckoutRequest{SuccessURL: "https://app.example/success", CancelURL: "https://app.example/cancel", IdempotencyKey: "accepted-inline"}
	result, err := svc.processStripePayment(context.Background(), req, &UserIdentity{ID: uuid.NewString()}, price, product)
	require.NoError(t, err)
	require.Equal(t, "https://checkout.stripe.test/accepted", result.RedirectURL)

	for name, mutate := range map[string]func(*CheckoutRequest, *models.Price, *models.Product) *models.Product{
		"missing return url": func(r *CheckoutRequest, _ *models.Price, p *models.Product) *models.Product {
			r.CancelURL = " "
			return p
		},
		"sub-cent amount": func(_ *CheckoutRequest, pr *models.Price, p *models.Product) *models.Product {
			pr.Amount = 12_340_001
			return p
		},
		"product is not the price's": func(_ *CheckoutRequest, _ *models.Price, _ *models.Product) *models.Product {
			return &models.Product{ID: uuid.New(), DisplayName: "x"}
		},
	} {
		r, pr := *req, *price
		p := mutate(&r, &pr, product)
		_, err := svc.processStripePayment(context.Background(), &r, &UserIdentity{ID: uuid.NewString()}, &pr, p)
		require.Error(t, err, name)
	}
	require.Equal(t, 1, calls, "refusals never reach Stripe")
}

// ccbillScope is a one-account PSP catalog for credential resolution.
type ccbillScope struct{ scope merchants.PSPScope }

func (c ccbillScope) ActivePSPSecretName(_ context.Context, _ merchant.ID, rail, env, key string) (string, bool, error) {
	name, err := merchants.PSPSecretName(rail, env, c.scope.AccountID, key)
	return name, err == nil, err
}
func (c ccbillScope) ActivePSPScope(_ context.Context, _ merchant.ID, rail, _ string) (merchants.PSPScope, bool, error) {
	return c.scope, rail == c.scope.Rail, nil
}
func (c ccbillScope) PSPScopeByID(_ context.Context, _ merchant.ID, id uuid.UUID) (merchants.PSPScope, bool, error) {
	return c.scope, id == c.scope.ID, nil
}

func ccbillService(t *testing.T, accountID string) *CheckoutService {
	store := merchants.NewMemorySecretStore()
	if name, err := merchants.PSPSecretName("ccbill", "live", accountID, "salt"); err == nil {
		_, err = store.Put(merchantCtx(), testMerchant, name, "merchant-salt")
		require.NoError(t, err)
	}
	svc := &CheckoutService{Config: &config.Config{ProviderWriteMode: config.ProviderWriteModeFull}}
	svc.SetMerchantSecretStore(store)
	svc.SetPSPSecretResolver(ccbillScope{merchants.PSPScope{ID: uuid.New(), Rail: "ccbill", Environment: "live", AccountID: accountID, Key: "ccbill"}})
	return svc
}

// #819/#697: a CCBill upgrade form carries the target price's own currency
// and the dash-split account identity, signed with the merchant's secret.
func TestCCBillUpgradeWire(t *testing.T) {
	email := "alice@example.com"
	user := &UserIdentity{ID: "user-1", Username: "alice", Email: &email}
	sub := &models.Subscription{ID: uuid.New(), Rail: models.RailCCBill, RailSubscriptionID: "ccbill-sub-1"}
	price := func(currency string) *models.Price {
		return &models.Price{ID: uuid.New(), Currency: currency, PSPLinks: map[string]map[string]string{
			"ccbill": {models.RailKeyRail: "ccbill", models.RailKeyCCBillFormName: "premium", models.RailKeyCCBillFlexID: "flex-123"}}}
	}

	resp, err := ccbillService(t, "945280-0000").processCCBillUpgrade(merchantCtx(), user, price("eur"), sub)
	require.NoError(t, err)
	parsed, err := url.Parse(resp.RedirectURL)
	require.NoError(t, err)
	q := parsed.Query()
	require.Equal(t, "978", q.Get("currencyCode"))
	require.Equal(t, "945280", q.Get("clientAccnum"))
	require.Equal(t, "0000", q.Get("clientSubacc"))
	require.Equal(t, "ccbill-sub-1", q.Get("originalSubscriptionId"))
	require.NotEmpty(t, q.Get("signature"))

	for _, currency := range []string{"", "sek"} {
		_, err := ccbillService(t, "945280-0000").processCCBillUpgrade(merchantCtx(), user, price(currency), sub)
		require.Error(t, err, "a currency CCBill cannot bill is never defaulted: %q", currency)
	}
	_, err = ccbillService(t, "945280/0000").processCCBillUpgrade(merchantCtx(), user, price("usd"), sub)
	require.ErrorContains(t, err, "CCBill account_id uses a dash")

	missing := &CheckoutService{}
	missing.SetMerchantSecretStore(merchants.NewMemorySecretStore())
	missing.SetPSPSecretResolver(pspCatalog{})
	_, err = missing.resolveCCBillClient(merchantCtx())
	require.ErrorContains(t, err, "missing scoped merchant CCBill PSP")
	_, err = (&CheckoutService{}).resolveCCBillClient(merchantCtx())
	require.ErrorContains(t, err, "not configured", "no boot-config fallback (#788)")
}

// NMI credentials resolve only through the scoped merchant plane (#788) and
// only for the account the operation captured.
func TestNMIClientResolutionFailsClosed(t *testing.T) {
	_, err := (&CheckoutService{}).resolveNMIClient(merchantCtx(), "nmi")
	require.ErrorContains(t, err, "not configured")

	unarmed := &CheckoutService{}
	unarmed.SetMerchantSecretStore(merchants.NewMemorySecretStore())
	unarmed.SetPSPSecretResolver(pspCatalog{})
	_, err = unarmed.resolveNMIClient(merchantCtx(), "nmi")
	require.ErrorContains(t, err, "has no armed PSP")

	captured := ccbillService(t, "945280-0000")
	pspID := captured.ProviderSecrets.(ccbillScope).scope.ID
	_, err = captured.resolveNMIClient(db.WithPSPID(merchantCtx(), pspID), "nmi")
	require.ErrorContains(t, err, "captured PSP rail mismatch")
}
