package subscriptions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testStripeConfig() *config.Config {
	return &config.Config{}
}

func testStripeProcessors() config.ProcessorSet {
	return config.ProcessorSet{
		"stripe": {Type: config.ProcessorTypeStripe, SecretKey: "sk_test_123"},
	}
}

// newTestServer spins up an httptest server, skipping the test if the sandbox
// disallows binding local ports (matches the pattern in stripe_refunds_test.go).
func newTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Skipf("cannot bind local listener in this environment: %v", r)
		}
	}()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateCustomer_SendsMetadataAndIdempotency(t *testing.T) {
	var gotBody, gotIdem string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/customers", r.URL.Path)
		gotIdem = r.Header.Get("Idempotency-Key")
		_ = r.ParseForm()
		gotBody = r.Form.Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cus_created"}`))
	})

	svc := &StripeService{Config: testStripeConfig(), Processors: testStripeProcessors()}
	svc.SetBaseURLForTest(srv.URL)

	id, err := svc.CreateCustomer(context.Background(), "a@b.com", "user-123")
	require.NoError(t, err)
	assert.Equal(t, "cus_created", id)
	assert.Contains(t, gotBody, "email=a%40b.com")
	assert.Contains(t, gotBody, "metadata%5Bapp_user_id%5D=user-123")
	assert.Equal(t, "customer_create_user-123", gotIdem)
}

func TestCreateCustomer_RequiresAppUserID(t *testing.T) {
	svc := &StripeService{Config: testStripeConfig(), Processors: testStripeProcessors()}
	_, err := svc.CreateCustomer(context.Background(), "a@b.com", "  ")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "app_user_id is required")
}

func TestFindCustomerIDByAppUserID_Found(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/customers/search", r.URL.Path)
		q := r.URL.Query().Get("query")
		assert.Contains(t, q, "metadata['app_user_id']:'user-9'")
		_, _ = w.Write([]byte(`{"data":[{"id":"cus_found"}]}`))
	})
	svc := &StripeService{Config: testStripeConfig(), Processors: testStripeProcessors()}
	svc.SetBaseURLForTest(srv.URL)

	id, err := svc.FindCustomerIDByAppUserID(context.Background(), "user-9")
	require.NoError(t, err)
	assert.Equal(t, "cus_found", id)
}

func TestFindCustomerIDByAppUserID_NoneReturnsEmpty(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	svc := &StripeService{Config: testStripeConfig(), Processors: testStripeProcessors()}
	svc.SetBaseURLForTest(srv.URL)

	id, err := svc.FindCustomerIDByAppUserID(context.Background(), "user-9")
	require.NoError(t, err)
	assert.Equal(t, "", id)
}

func TestListActiveSubscriptionsForCustomer_QueriesActiveAndTrialing(t *testing.T) {
	var statuses []string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/subscriptions", r.URL.Path)
		assert.Equal(t, "cus_1", r.URL.Query().Get("customer"))
		status := r.URL.Query().Get("status")
		statuses = append(statuses, status)
		if status == "active" {
			_, _ = w.Write([]byte(`{"data":[{"id":"sub_a","status":"active","items":{"data":[{"price":{"id":"price_1","lookup_key":"lk_1"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"sub_t","status":"trialing","items":{"data":[{"price":{"id":"price_2"}}]}}]}`))
	})
	svc := &StripeService{Config: testStripeConfig(), Processors: testStripeProcessors()}
	svc.SetBaseURLForTest(srv.URL)

	subs, err := svc.ListActiveSubscriptionsForCustomer(context.Background(), "cus_1")
	require.NoError(t, err)
	require.Len(t, subs, 2)
	assert.ElementsMatch(t, []string{"active", "trialing"}, statuses)

	byID := map[string]StripeSubscriptionSummary{}
	for _, s := range subs {
		byID[s.ID] = s
	}
	assert.Equal(t, "price_1", byID["sub_a"].PriceID)
	assert.Equal(t, "lk_1", byID["sub_a"].LookupKey)
	assert.Equal(t, "price_2", byID["sub_t"].PriceID)
}

func TestListActiveSubscriptionsForCustomer_PropagatesAPIError(t *testing.T) {
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad customer"}}`))
	})
	svc := &StripeService{Config: testStripeConfig(), Processors: testStripeProcessors()}
	svc.SetBaseURLForTest(srv.URL)

	_, err := svc.ListActiveSubscriptionsForCustomer(context.Background(), "cus_1")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "bad customer"))
}
