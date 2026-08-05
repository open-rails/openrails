//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

func TestCancelSubscriptionRequiresAuth(t *testing.T) {
	suite := getSharedTestSuite(t)

	body := map[string]string{"feedback": "test feedback"}
	jsonBody, _ := json.Marshal(body)

	cases := []struct {
		name string
		auth string
		code int
	}{
		{name: "returns 401 without auth token", auth: "", code: http.StatusUnauthorized},
		{name: "returns 401 with invalid token", auth: "Bearer invalid-token", code: http.StatusUnauthorized},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subID := uuid.New().String()
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+subID+"/cancel", bytes.NewReader(jsonBody))
			req.Header.Set("Content-Type", "application/json")
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			suite.Server.Handler().ServeHTTP(w, req)
			assert.Equal(t, tc.code, w.Code)
		})
	}
}

func TestCancelSubscriptionNotFound(t *testing.T) {
	suite := getSharedTestSuite(t)
	userID := uuid.New().String()
	router := newHostSeamSelfRouter(t, suite, userID, nil)

	body := map[string]string{"feedback": "test feedback"}
	jsonBody, _ := json.Marshal(body)

	subID := uuid.New().String()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+subID+"/cancel", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)

	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// #696: CCBill cancels queue like every other rail (no more portal redirect);
// the worker's user cancel path then records the local runway cancel + the
// durable ccbill_cancel_subscription intent.
func TestCancelSubscriptionCCBill(t *testing.T) {
	suite := getSharedTestSuite(t)
	userID := uuid.New().String()
	router := newHostSeamSelfRouter(t, suite, userID, nil)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userID,
		PriceID:   priceID,
		Status:    models.StatusActive,
		Rail:      models.RailCCBill,
		RailSubID: "test-ccbill-sub-" + uuid.NewString(),
	})

	body := map[string]string{"feedback": "I want to cancel"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)

	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)

	var response map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, "queued", response["status"])
}

func TestCancelSubscriptionAlreadyCancelled(t *testing.T) {
	suite := getSharedTestSuite(t)
	userID := uuid.New().String()
	router := newHostSeamSelfRouter(t, suite, userID, nil)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userID,
		PriceID:   priceID,
		Status:    models.StatusCancelled,
		Rail:      models.RailNMI,
		RailSubID: "test-nmi-cancelled-" + uuid.NewString(),
	})

	body := map[string]string{"feedback": "test"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)

	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

func TestCancelSubscriptionAuthBoundaries(t *testing.T) {
	suite := getSharedTestSuite(t)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	userAID := uuid.New().String()
	userBID := uuid.New().String()
	routerA := newHostSeamSelfRouter(t, suite, userAID, nil)

	subB := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userBID,
		PriceID:   priceID,
		Status:    models.StatusActive,
		Rail:      models.RailNMI,
		RailSubID: "test-mobius-sub-" + uuid.NewString(),
	})

	body := map[string]string{"feedback": "not yours"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+subB.ID.String()+"/cancel", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)

	routerA.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// or#896: the rail-agnostic cancel used to accept a Solana subscription, queue
// a job, fall through the worker's default branch into a facade with no Solana
// case, and fail permanently. A Solana cancel is an on-chain transaction the
// SUBSCRIBER'S wallet must sign, so the request is refused synchronously with
// the dedicated endpoints named — and nothing local changes.
func TestCancelSubscriptionSolanaNamesDedicatedEndpoints(t *testing.T) {
	suite := getSharedTestSuite(t)
	userID := uuid.New().String()
	router := newHostSeamSelfRouter(t, suite, userID, nil)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:    userID,
		PriceID:   priceID,
		Status:    models.StatusActive,
		Rail:      models.RailSolana,
		RailSubID: "test-solana-sub-" + uuid.NewString(),
	})

	jsonBody, _ := json.Marshal(map[string]string{"feedback": "I want to cancel"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	body := w.Body.String()
	assert.Contains(t, body, "solana-cancel-tx", "the refusal must name the prepare endpoint")
	assert.Contains(t, body, "solana-cancel", "the refusal must name the confirm endpoint")
	assert.Contains(t, body, "wallet")

	assert.Equal(t, models.StatusActive, suite.GetSubscription(sub.ID).Status,
		"a refused cancel leaves the subscription untouched")

	// The service refuses it too, so no other producer can drive a DB-only
	// "soft cancel" of a chain-truth subscription.
	err := suite.App.Runtime.UserSubscriptionService.CancelUserSubscription(suite.MerchantCtx(), userID, "direct")
	require.ErrorIs(t, err, subscriptions.ErrSolanaCancelNeedsWalletSignature)
}
