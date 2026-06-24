//go:build integration

package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/models"
)

func TestAdminOffChannelPaymentCreatesPaymentAndEntitlements(t *testing.T) {
	// #528 hard cut: the off-channel write is on the delegated /v1/merchant surface,
	// authorized by merchant:customer-settings:update - no per-user admin model.
	suite := getSharedTestSuite(t)
	admin := newHostSeamAdminRouter(t, suite, "b5555555-5555-4555-8555-555555555555",
		[]string{controlplane.PermMerchantCustomerSettingsUpdate})
	products := suite.SeedProducts()

	// Use the one-time "lifetime" product so the entitlements granted are sensible as a one-off.
	lifetimePriceID := products[2].Prices[0].ID
	userID := uuid.New().String()

	fixedNow := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	suite.SetMockClock(fixedNow)

	body, err := json.Marshal(map[string]any{
		"price_id":          lifetimePriceID.String(),
		"transaction_id":    "cash-rcpt-" + uuid.NewString()[:8],
		"amount":            int64(1500),
		"currency":          "usd",
		"purchased_at":      fixedNow.Format(time.RFC3339),
		"discount_reason":   "manual_discount",
		"discount_code":     "SUPPORT15",
		"discount_metadata": map[string]any{"note": "paid cash at meetup"},
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/merchant/customers/"+userID+"/payments/off-channel", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+merchantDelegatedTestToken)
	req.Header.Set("Content-Type", "application/json")

	admin.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var resp struct {
		PaymentID string `json:"payment_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	paymentID := uuid.MustParse(resp.PaymentID)

	p := suite.GetPaymentByID(req.Context(), paymentID)

	require.Equal(t, userID, p.CustomerID.String())
	require.Equal(t, lifetimePriceID, p.PriceID)
	require.Equal(t, models.RailManual, p.Rail)
	require.Equal(t, int64(1500), p.Amount)
	require.Equal(t, int64(29999), p.ListAmount) // canonical list price from seed data
	require.Equal(t, "usd", p.Currency)
	// Instant equality, not struct equality: the DB round-trip can hand back the
	// same instant with a different internal time.Time representation.
	require.WithinDuration(t, fixedNow, p.PurchasedAt, 0)
	require.NotNil(t, p.DiscountReason)
	require.Equal(t, "manual_discount", *p.DiscountReason)
	require.NotNil(t, p.DiscountCode)
	require.Equal(t, "SUPPORT15", *p.DiscountCode)

	ents := suite.QueryEntitlements(req.Context(),
		"WHERE source_type = $1 AND source_id = $2 AND deleted_at IS NULL",
		string(models.EntitlementSourceOneOff), paymentID)
	require.NotEmpty(t, ents)
}
