//go:build integration

package tests

// Stripe checkout e2e (#694 GAP 1). The session→redirect front door of the
// hosted Stripe rail runs through the REAL stack — HTTP in, real Postgres,
// real checkout services — with the ONLY fake at the stripeapi choke-point
// transport: an httptest server answering real Stripe wire shapes (customer
// search/create, checkout session create). Asserts the returned redirect URL,
// the persisted checkout_sessions row, and the exact form Stripe would have
// received (wire pinning).
//
// The activation leg (checkout.session.completed webhook → active local
// subscription) is deliberately NOT covered here yet: the Stripe webhook apply
// path (internal/modules/webhooks/stripe.go) is being rewritten to
// fetch-and-converge under #684. Add that leg once #684 lands.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/pkg/api"
)

// fakeStripeAPI serves the minimal set of real Stripe wire shapes the hosted
// checkout path speaks, recording every request for assertions.
type fakeStripeAPI struct {
	server *httptest.Server

	mu               sync.Mutex
	checkoutForms    []url.Values
	checkoutVersions []string
	checkoutKeys     []string
	customerCreates  int

	webhookEndpointLists   int
	webhookEndpointCreates []url.Values
}

func newFakeStripeAPI(t *testing.T) *fakeStripeAPI {
	t.Helper()
	f := &fakeStripeAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/customers/search", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "search_result", "data": []any{}, "has_more": false})
	})
	mux.HandleFunc("POST /v1/customers", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.customerCreates++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"id": "cus_e2e_test", "object": "customer"})
	})
	mux.HandleFunc("GET /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{}, "has_more": false})
	})
	mux.HandleFunc("POST /v1/checkout/sessions", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.mu.Lock()
		f.checkoutForms = append(f.checkoutForms, r.PostForm)
		f.checkoutVersions = append(f.checkoutVersions, r.Header.Get(stripeapi.VersionHeader))
		f.checkoutKeys = append(f.checkoutKeys, r.Header.Get(stripeapi.IdempotencyKeyHeader))
		f.mu.Unlock()
		writeJSON(w, map[string]any{
			"id":     "cs_test_e2e_1",
			"object": "checkout.session",
			"status": "open",
			"url":    "https://checkout.stripe.com/c/pay/cs_test_e2e_1",
		})
	})
	mux.HandleFunc("GET /v1/webhook_endpoints", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.webhookEndpointLists++
		f.mu.Unlock()
		writeJSON(w, map[string]any{"object": "list", "data": []any{}, "has_more": false})
	})
	mux.HandleFunc("POST /v1/webhook_endpoints", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		f.mu.Lock()
		f.webhookEndpointCreates = append(f.webhookEndpointCreates, r.PostForm)
		f.mu.Unlock()
		writeJSON(w, map[string]any{
			"id":             "we_e2e_1",
			"object":         "webhook_endpoint",
			"status":         "enabled",
			"url":            r.PostForm.Get("url"),
			"api_version":    r.PostForm.Get("api_version"),
			"enabled_events": r.PostForm["enabled_events[]"],
			"metadata":       map[string]string{"openrails_managed": "true"},
			"secret":         "whsec_river_e2e",
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fake stripe: unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotImplemented)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)

	// Install the choke-point fake: every stripeapi client call is host-rewritten
	// onto the fake server; guard semantics (version pinning) still apply.
	stripeapi.SetBaseTransport(hostRewriteTransport{target: f.server.URL})
	t.Cleanup(func() { stripeapi.SetBaseTransport(nil) })
	return f
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func clearCheckoutIdempotency(t *testing.T, suite *TestContainerSuite, clientKey string) {
	t.Helper()
	sum := sha256.Sum256([]byte(clientKey))
	keys, err := suite.RedisClient.Keys(
		context.Background(),
		"idemp:*:"+hex.EncodeToString(sum[:]),
	).Result()
	require.NoError(t, err)
	if len(keys) > 0 {
		require.NoError(t, suite.RedisClient.Del(context.Background(), keys...).Err())
	}
}

// hostRewriteTransport sends every request to target regardless of the
// original host (api.stripe.com), preserving method/path/query/body/headers.
type hostRewriteTransport struct{ target string }

func (h hostRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := url.Parse(h.target)
	if err != nil {
		return nil, err
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = u.Scheme
	clone.URL.Host = u.Host
	clone.Host = u.Host
	return http.DefaultTransport.RoundTrip(clone)
}

// seedStripeSubscriptionPrice inserts a product + auto-renewing price mapped to
// a Stripe price id, mirroring how cozy-art catalogs its Stripe plans.
func seedStripeSubscriptionPrice(t *testing.T, suite *TestContainerSuite, stripePriceID string) uuid.UUID {
	t.Helper()
	ctx := suite.MerchantCtx()
	product := &models.Product{
		ID:          uuid.New(),
		Key:         "stripe-e2e-" + uuid.New().String()[:8],
		DisplayName: "Stripe E2E Premium",
		Description: "Stripe hosted-checkout e2e product",
		EntitlementsSpec: map[string]*int{
			"stripe-e2e-premium": nil,
		},
	}
	suite.InsertProduct(ctx, product)
	trialAmount := int64(0)
	trialHours := 24
	price := &models.Price{
		ID:                  uuid.New(),
		ProductID:           product.ID,
		Amount:              9_990_000, // micros
		Currency:            "usd",
		AutoRenew:           true,
		AccessDurationHours: intPtr(720),
		TrialUnitAmount:     &trialAmount,
		TrialDurationHours:  &trialHours,
		PSPLinks: map[string]map[string]string{
			string(models.RailStripe): {
				models.RailKeyRail:          string(models.RailStripe),
				models.RailKeyStripePriceID: stripePriceID,
			},
		},
	}
	suite.InsertPrice(ctx, price)
	return price.ID
}

// TestCheckoutSessionStripeRedirect drives POST /v1/checkout for the Stripe
// rail end-to-end: authenticated HTTP in ⇒ requires_action + hosted-checkout
// redirect URL out, checkout_sessions row persisted with the redirect state,
// and the exact Stripe checkout-session create form pinned on the wire.
func TestCheckoutSessionStripeRedirect(t *testing.T) {
	fake := newFakeStripeAPI(t)
	suite := setupTestSuite(t, WithSuiteStripeRail("sk_test_e2e"))
	priceID := seedStripeSubscriptionPrice(t, suite, "price_e2e_stripe")

	userID := uuid.New().String()
	email := "checkout-session-stripe-" + t.Name() + "@test.example.com"
	token := suite.MintUserToken(userID, email)

	body := map[string]any{
		"price_id":    priceID.String(),
		"success_url": "https://app.test.example.com/billing/success",
		"cancel_url":  "https://app.test.example.com/billing/cancel",
		"payment": map[string]any{
			"rail": "stripe",
		},
	}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", "stripe-checkout-e2e")

	suite.Server.Handler().ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code, "Should return 200 OK, got body: %s", w.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "requires_action", resp["status"], "Status should be requires_action")

	payment, ok := resp["payment"].(map[string]any)
	require.True(t, ok, "payment should be an object")
	assert.Equal(t, "stripe", payment["rail"], "Rail should be stripe")
	assert.Equal(t, "https://checkout.stripe.com/c/pay/cs_test_e2e_1", payment["redirect_url"],
		"redirect_url should be the hosted-checkout URL Stripe returned")

	// The session row is the durable artifact the webhook leg later resolves.
	sessionID, err := api.ParseCheckoutSessionID(resp["id"].(string))
	require.NoError(t, err)
	var status, rail, redirectURL string
	require.NoError(t, suite.Pool.QueryRow(context.Background(), `
		SELECT status, rail, COALESCE(rail_state->>'redirect_url', '')
		  FROM openrails.checkout_sessions
		 WHERE id = $1
	`, sessionID).Scan(&status, &rail, &redirectURL))
	assert.Equal(t, "requires_action", status)
	assert.Equal(t, "stripe", rail)
	assert.Equal(t, "https://checkout.stripe.com/c/pay/cs_test_e2e_1", redirectURL)

	// Wire pinning: exactly one checkout-session create, with the fields the
	// activation leg depends on (metadata linkage back to OUR session/user).
	fake.mu.Lock()
	checkoutForms := append([]url.Values(nil), fake.checkoutForms...)
	checkoutVersions := append([]string(nil), fake.checkoutVersions...)
	checkoutKeys := append([]string(nil), fake.checkoutKeys...)
	customerCreates := fake.customerCreates
	fake.mu.Unlock()
	require.Len(t, checkoutForms, 1, "exactly one Stripe checkout session create")
	form := checkoutForms[0]
	assert.Equal(t, "subscription", form.Get("mode"))
	assert.Equal(t, "price_e2e_stripe", form.Get("line_items[0][price]"))
	assert.Equal(t, "1", form.Get("line_items[0][quantity]"))
	assert.Equal(t, "https://app.test.example.com/billing/success", form.Get("success_url"))
	assert.Equal(t, "https://app.test.example.com/billing/cancel", form.Get("cancel_url"))
	assert.Equal(t, userID, form.Get("client_reference_id"))
	assert.Equal(t, userID, form.Get("metadata[user_id]"))
	assert.Equal(t, priceID.String(), form.Get("metadata[internal_price_id]"))
	assert.Equal(t, resp["id"].(string), form.Get("metadata[checkout_session_id]"))
	assert.Equal(t, userID, form.Get("subscription_data[metadata][user_id]"))
	// One durable Stripe customer per user (#212): resolved BEFORE the session,
	// and the session is created against it (not customer_email).
	assert.Equal(t, 1, customerCreates, "customer created once via search-miss → create")
	assert.Equal(t, "cus_e2e_test", form.Get("customer"))
	assert.Empty(t, form.Get("customer_email"), "customer and customer_email are mutually exclusive")
	// Choke-point proof: the API version pin rode through the fake transport.
	assert.Equal(t, stripeapi.APIVersion, checkoutVersions[0])
	assert.NotEmpty(t, checkoutKeys[0], "checkout create must carry a Stripe idempotency key")

	// Drop both Redis replay layers to simulate retry after their cache windows.
	// The durable session id/fingerprint must recover the original local row and
	// redirect without issuing a second provider request.
	clearCheckoutIdempotency(t, suite, "stripe-checkout-e2e")
	second := httptest.NewRecorder()
	secondReq, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	secondReq.Header.Set("Content-Type", "application/json")
	secondReq.Header.Set("Authorization", "Bearer "+token)
	secondReq.Header.Set("Idempotency-Key", "stripe-checkout-e2e")
	suite.Server.Handler().ServeHTTP(second, secondReq)
	require.Equal(t, http.StatusOK, second.Code, "durable replay body: %s", second.Body.String())
	var secondResp map[string]any
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &secondResp))
	assert.Equal(t, resp["id"], secondResp["id"], "durable replay must reuse the checkout session")
	fake.mu.Lock()
	checkoutCreateCount := len(fake.checkoutForms)
	fake.mu.Unlock()
	require.Equal(t, 1, checkoutCreateCount, "durable replay must not create another Stripe checkout session")

	// Simulate a process dying after Stripe accepted the request but before the
	// local row persisted the redirect. The next cache-miss retry must reuse both
	// the local session id and Stripe key, so every provider parameter is stable.
	_, err = suite.Pool.Exec(context.Background(), `
		UPDATE openrails.checkout_sessions
		   SET status = 'created', rail_state = rail_state - 'redirect_url'
		 WHERE id = $1
	`, sessionID)
	require.NoError(t, err)
	clearCheckoutIdempotency(t, suite, "stripe-checkout-e2e")
	third := httptest.NewRecorder()
	thirdReq, _ := http.NewRequest("POST", "/v1/checkout", bytes.NewReader(jsonBody))
	thirdReq.Header.Set("Content-Type", "application/json")
	thirdReq.Header.Set("Authorization", "Bearer "+token)
	thirdReq.Header.Set("Idempotency-Key", "stripe-checkout-e2e")
	suite.Server.Handler().ServeHTTP(third, thirdReq)
	require.Equal(t, http.StatusOK, third.Code, "crash replay body: %s", third.Body.String())
	var thirdResp map[string]any
	require.NoError(t, json.Unmarshal(third.Body.Bytes(), &thirdResp))
	assert.Equal(t, resp["id"], thirdResp["id"], "crash replay must reuse the checkout session")
	fake.mu.Lock()
	checkoutForms = append([]url.Values(nil), fake.checkoutForms...)
	checkoutKeys = append([]string(nil), fake.checkoutKeys...)
	fake.mu.Unlock()
	require.Len(t, checkoutForms, 2, "crash replay must retry the same Stripe operation once")
	assert.Equal(t, checkoutKeys[0], checkoutKeys[1], "crash replay must reuse the Stripe idempotency key")
	assert.Equal(t, checkoutForms[0].Get("metadata[checkout_session_id]"), checkoutForms[1].Get("metadata[checkout_session_id]"))
	assert.NotEmpty(t, checkoutForms[0].Get("subscription_data[trial_end]"))
	assert.Equal(t, checkoutForms[0].Get("subscription_data[trial_end]"), checkoutForms[1].Get("subscription_data[trial_end]"))

	// The user's rail-customer mapping was recorded at checkout time (#212).
	var mapped string
	require.NoError(t, suite.Pool.QueryRow(context.Background(), `
		SELECT account_id FROM openrails.rail_customer_accounts
		 WHERE customer_id = $1::uuid AND rail = 'stripe'
	`, userID).Scan(&mapped))
	assert.Equal(t, "cus_e2e_test", mapped)
}
