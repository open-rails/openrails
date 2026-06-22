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

func TestCancelSubscriptionCCBill(t *testing.T) {
	suite := getSharedTestSuite(t)
	userID := uuid.New().String()
	router := newHostSeamSelfRouter(t, suite, userID, nil)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:         userID,
		PriceID:        priceID,
		Status:         models.StatusActive,
		Processor:      models.ProcessorCCBill,
		ProcessorSubID: "test-ccbill-sub-" + uuid.NewString(),
	})

	body := map[string]string{"feedback": "I want to cancel"}
	jsonBody, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/me/subscriptions/"+sub.ID.String()+"/cancel", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)

	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)

	var response map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &response)
	require.NoError(t, err)
	require.Equal(t, "ccbill_cancel_required", response["code"])
	require.Equal(t, "https://support.ccbill.com", response["support_url"])
}

func TestCancelSubscriptionAlreadyCancelled(t *testing.T) {
	suite := getSharedTestSuite(t)
	userID := uuid.New().String()
	router := newHostSeamSelfRouter(t, suite, userID, nil)

	products := suite.SeedProducts()
	priceID := products[0].Prices[0].ID

	sub := suite.CreateTestSubscriptionWithOptions(SubscriptionOptions{
		UserID:         userID,
		PriceID:        priceID,
		Status:         models.StatusCancelled,
		Processor:      models.ProcessorMobius,
		ProcessorSubID: "test-nmi-cancelled-" + uuid.NewString(),
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
		UserID:         userBID,
		PriceID:        priceID,
		Status:         models.StatusActive,
		Processor:      models.ProcessorMobius,
		ProcessorSubID: "test-mobius-sub-" + uuid.NewString(),
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
