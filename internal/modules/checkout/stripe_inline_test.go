package checkout

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/stretchr/testify/require"
)

func TestEngineOneTimeCheckoutUsesLocalInlineTerms(t *testing.T) {
	cfg := &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull, NewSubscriptionCollectionPolicy: "engine"}
	service := &CheckoutService{Config: cfg, Rails: railresolve.FixedSet{"stripe": {Rail: models.RailStripe, AccountID: "acct_test", Stripe: &config.StripeRailConfig{SecretKey: "sk_test_synthetic"}}}}
	product := &models.Product{ID: uuid.New(), DisplayName: "Accepted post"}
	price := &models.Price{ID: uuid.New(), ProductID: product.ID, Amount: 12340000, Currency: "USD"}
	calls := 0
	release := stripeapi.InstallBaseTransport(initialStripeWireUnit(func(r *http.Request) (*http.Response, error) {
		calls++
		require.Equal(t, "/v1/checkout/sessions", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		require.Empty(t, r.Form.Get("line_items[0][price]"))
		require.Equal(t, "usd", r.Form.Get("line_items[0][price_data][currency]"))
		require.Equal(t, "1234", r.Form.Get("line_items[0][price_data][unit_amount]"))
		require.Equal(t, "Accepted post", r.Form.Get("line_items[0][price_data][product_data][name]"))
		require.Equal(t, price.ID.String(), r.Form.Get("metadata[internal_price_id]"))
		require.Equal(t, "payment", r.Form.Get("mode"))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"url":"https://checkout.stripe.test/accepted"}`))}, nil
	}))
	defer release()
	result, err := service.processStripePayment(context.Background(), &CheckoutRequest{SuccessURL: "https://app.example/success", CancelURL: "https://app.example/cancel", IdempotencyKey: "accepted-inline"}, &UserIdentity{ID: uuid.NewString()}, price, product)
	require.NoError(t, err)
	require.Equal(t, "redirect_required", result.Status)
	require.Equal(t, 1, calls)
}

type initialStripeWireUnit func(*http.Request) (*http.Response, error)

func (f initialStripeWireUnit) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
