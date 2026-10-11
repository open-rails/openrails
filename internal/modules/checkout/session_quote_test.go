package checkout

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

// A session's quoted membership renews, as an order's does, and its terms
// survive the session's round trip.
func TestSessionMembershipQuoteRenews(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	mid, customer, psp := uuid.New(), uuid.New(), uuid.New()
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	product := models.Product{ID: uuid.New(), DisplayName: "Term", Entitlements: []string{"premium"}}
	price := models.Price{ID: uuid.New(), ProductID: product.ID, Amount: 10_000_000, Currency: "USD", BillingIntervalHours: new(720), AccessDurationHours: new(24)}
	method := gen.BillingPaymentMethod{ID: uuid.New(), MerchantID: mid, CustomerID: customer, Rail: "nmi", Status: "active", Custodian: models.CustodianPSP, PspID: &psp, RailCustomerRef: new("vault"), RailMethodRef: new("method")}
	session := models.CheckoutAttempt{ID: uuid.New(), CustomerID: customer, PspID: psp, PriceID: &price.ID, Mode: models.CheckoutAttemptModeSubscription, Rail: models.RailNMI, Amount: &price.Amount, Currency: &price.Currency, ExpiresAt: new(now.Add(time.Hour)), RailState: map[string]any{}}
	require.NoError(t, quoteInitialMembership(ctx, &session, &price, &product, method, now))
	serialized, err := json.Marshal(session)
	require.NoError(t, err)
	var replay models.CheckoutAttempt
	require.NoError(t, json.Unmarshal(serialized, &replay))
	terms, err := readInitialMembershipQuote(&replay)
	require.NoError(t, err)
	require.False(t, terms.CancelAfterInitial)
	require.Equal(t, 720*time.Hour, terms.PeriodEnd.Sub(terms.PeriodStart))
	require.Equal(t, 24, *terms.AccessDurationHours)
	require.Equal(t, "Term", (&CheckoutAttemptService{}).sessionToResponse(&replay).MembershipQuote.ProductName)
}
