//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/httptesthost"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type catalogCheckoutProvider struct{ calls atomic.Int32 }

func (p *catalogCheckoutProvider) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPost || r.URL.Path != "/v1/checkout/sessions" {
		return nil, fmt.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
	}
	p.calls.Add(1)
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"cs_test_wrapped","url":"https://checkout.stripe.test/wrapped"}`)), Request: r}, nil
}

func TestCatalogCheckoutCurrentOfferAcrossEmbeddedAndRemote(t *testing.T) {
	ctx := t.Context()
	_, pool, dsn := scopeWithoutRLSDatabase(t)
	provider := &catalogCheckoutProvider{}
	runtime, mid, err := newDeclaredMerchant(ctx, embed.Options{Config: &config.Config{
		Env: "development", TestMode: config.CredentialPostureSandbox,
		MerchantConfigSource: config.MerchantConfigSourceManifest, AllowCatalogUpdates: true,
		ProviderWriteMode: config.ProviderWriteModeFull, NewSubscriptionCollectionPolicy: "engine", DB: &config.DBConfig{URL: dsn},
	}, PGXPool: pool, River: embed.RiverManagedByOpenRails(), StripeTransport: provider}, "checkout-"+uuid.NewString(), embed.MerchantConfig{DisplayName: "Checkout catalog", PSPs: map[string]embed.PSPConfig{"stripe": {"stripe": {AccountID: "acct_checkout_1051", Secrets: map[string]string{"secret_key": "sk_test_checkout_1051", "webhook_signing_secret": "whsec_checkout_1051"}}}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
	local, err := runtime.Client()
	require.NoError(t, err)
	handler, err := httptesthost.Handler(runtime, httptesthost.Options{HTTP: embed.HTTPConfig{Catalog: true, MerchantAPI: true}, Gate: creatorAdminTestGate{mid: mid}})
	require.NoError(t, err)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	remote, err := openrails.NewRemote(server.URL, openrails.WithAPIKey("administrator"), openrails.WithMerchantID(mid))
	require.NoError(t, err)
	for name, client := range map[string]*openrails.Client{"embedded": local, "remote": remote} {
		t.Run(name, func(t *testing.T) {
			productKey, offerKey := "post-"+name, "offer-"+name
			apply := func(product openrails.CatalogApplyProduct) {
				revision, err := client.Catalog.Revision(ctx)
				require.NoError(t, err)
				_, err = client.Catalog.Apply(ctx, &openrails.CatalogApplyParams{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: &revision.Revision, Products: []openrails.CatalogApplyProduct{product}})
				require.NoError(t, err)
			}
			apply(openrails.CatalogApplyProduct{Key: productKey, DisplayName: openrails.CatalogValue("Post"), Prices: []openrails.CatalogApplyPrice{{Key: offerKey, UnitAmount: openrails.CatalogValue(int64(1_000_000)), Currency: openrails.CatalogValue("USD")}}})
			customer := uuid.NewString()
			request := openrails.CreateCheckoutSessionRequest{Customer: openrails.CheckoutCustomerIdentity{ID: customer, VerifiedEmail: "reader@example.com"}, PriceKey: offerKey, PaymentOptions: openrails.CheckoutPaymentOptions{Rail: "stripe"}, IdempotencyKey: uuid.NewString(), SuccessURL: "https://blog.example/success", CancelURL: "https://blog.example/cancel"}
			original, err := client.CreateCheckoutSession(ctx, request)
			require.NoError(t, err, "the one SDK operation resolves the opaque current offer")
			require.EqualValues(t, 1_000_000, *original.Amount)
			apply(openrails.CatalogApplyProduct{Key: productKey, Prices: []openrails.CatalogApplyPrice{{Key: offerKey, UnitAmount: openrails.CatalogValue(int64(2_000_000))}}})
			retired, err := client.Prices.Retrieve(ctx, *original.PriceID)
			require.NoError(t, err)
			require.True(t, retired.Archived)
			replay, err := client.CreateCheckoutSession(ctx, request)
			require.NoError(t, err)
			require.Equal(t, original.ID, replay.ID)
			require.Equal(t, original.Amount, replay.Amount)
			stale := request
			stale.Customer.ID = uuid.NewString()
			stale.IdempotencyKey = uuid.NewString()
			stale.PriceID = *original.PriceID
			stale.PriceKey = ""
			_, err = client.CreateCheckoutSession(ctx, stale)
			require.Error(t, err, "retired immutable price cannot start a new purchase")
			current := request
			current.Customer.ID = uuid.NewString()
			current.IdempotencyKey = uuid.NewString()
			next, err := client.CreateCheckoutSession(ctx, current)
			require.NoError(t, err)
			require.EqualValues(t, 2_000_000, *next.Amount)
			require.NotEqual(t, original.PriceID, next.PriceID)
			// A key that looks exactly like another price's UUID still selects
			// the KEY's product/offer. The ID field selects the UUID's own row.
			priceID, err := openrails.ParsePriceID(*next.PriceID)
			require.NoError(t, err)
			uuidKey := priceID.UUID().String()
			apply(openrails.CatalogApplyProduct{Key: "uuid-key-" + name, DisplayName: openrails.CatalogValue("UUID key"), Prices: []openrails.CatalogApplyPrice{{Key: uuidKey, UnitAmount: openrails.CatalogValue(int64(3_000_000)), Currency: openrails.CatalogValue("USD")}}})
			keyRequest := current
			keyRequest.Customer.ID, keyRequest.IdempotencyKey, keyRequest.PriceKey = uuid.NewString(), uuid.NewString(), uuidKey
			byKey, err := client.CreateCheckoutSession(ctx, keyRequest)
			require.NoError(t, err)
			require.EqualValues(t, 3_000_000, *byKey.Amount)
			require.NotEqual(t, next.PriceID, byKey.PriceID)
			idRequest := keyRequest
			idRequest.Customer.ID, idRequest.IdempotencyKey, idRequest.PriceID, idRequest.PriceKey = uuid.NewString(), uuid.NewString(), priceID.String(), ""
			byID, err := client.CreateCheckoutSession(ctx, idRequest)
			require.NoError(t, err)
			require.EqualValues(t, 2_000_000, *byID.Amount)
			require.Equal(t, next.PriceID, byID.PriceID)
			for _, invalid := range []openrails.CreateCheckoutSessionRequest{
				{PriceID: uuidKey, PriceKey: uuidKey}, {}, {PriceID: offerKey},
			} {
				invalid.Customer = request.Customer
				invalid.IdempotencyKey = uuid.NewString()
				_, err := client.CreateCheckoutSession(ctx, invalid)
				require.ErrorIs(t, err, openrails.ErrInvalid)
			}
			apply(openrails.CatalogApplyProduct{Key: productKey, Archived: openrails.CatalogValue(true)})
			unavailable := current
			unavailable.Customer.ID = uuid.NewString()
			unavailable.IdempotencyKey = uuid.NewString()
			_, err = client.CreateCheckoutSession(ctx, unavailable)
			require.Error(t, err, "archived content offer is unavailable through the SDK itself")
			replay, err = client.CreateCheckoutSession(ctx, current)
			require.NoError(t, err)
			require.Equal(t, next.ID, replay.ID)
		})
	}
	// Direct HTTP callers get the same exclusivity check even without the SDK.
	for _, selectors := range []map[string]string{{}, {"price_id": uuid.NewString(), "price_key": "offer"}} {
		body := map[string]any{"customer": map[string]string{"id": uuid.NewString()}}
		for key, value := range selectors {
			body[key] = value
		}
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/merchant/checkout-sessions", strings.NewReader(string(encoded)))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer administrator")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", uuid.NewString())
		req.Header.Set(merchant.BindingHeader, mid.String())
		response, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		response.Body.Close()
		require.Equal(t, http.StatusBadRequest, response.StatusCode)
	}
	require.EqualValues(t, 8, provider.calls.Load())
}
