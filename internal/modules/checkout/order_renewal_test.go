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

func TestOrderRenewalPreferenceIsAcceptedAndReplayed(t *testing.T) {
	now := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	mid, customer, psp := uuid.New(), uuid.New(), uuid.New()
	ctx := merchant.WithID(t.Context(), billing.MerchantID(mid))
	product := models.Product{ID: uuid.New(), DisplayName: "Term", EntitlementsSpec: map[string]*int{"premium": nil}}
	price := models.Price{ID: uuid.New(), ProductID: product.ID, Amount: 10_000_000, Currency: "USD", BillingIntervalHours: new(720), AccessDurationHours: new(24)}
	method := gen.BillingPaymentMethod{ID: uuid.New(), MerchantID: mid, CustomerID: customer, Rail: "nmi", Custodian: models.CustodianPSP, PspID: &psp, RailCustomerRef: new("vault"), RailMethodRef: new("method")}
	for _, preference := range []*bool{nil, new(true), new(false)} {
		state := map[string]any{}
		if preference != nil {
			state["auto_renew"] = *preference
		}
		session := models.CheckoutAttempt{ID: uuid.New(), CustomerID: customer, PspID: psp, PriceID: &price.ID, Mode: models.CheckoutAttemptModeSubscription, Rail: models.RailNMI, Amount: &price.Amount, Currency: &price.Currency, ExpiresAt: new(now.Add(time.Hour)), RailState: state}
		require.NoError(t, quoteInitialMembership(ctx, &session, &price, &product, method, now))
		serialized, err := json.Marshal(session)
		require.NoError(t, err)
		var replay models.CheckoutAttempt
		require.NoError(t, json.Unmarshal(serialized, &replay))
		terms, err := readInitialMembershipQuote(&replay)
		require.NoError(t, err)
		cancel := preference != nil && !*preference
		require.Equal(t, cancel, terms.CancelAfterInitial)
		require.Equal(t, !cancel, (&CheckoutAttemptService{}).sessionToResponse(&replay).MembershipQuote.AutoRenew)
		require.Equal(t, 720*time.Hour, terms.PeriodEnd.Sub(terms.PeriodStart))
		require.Equal(t, 24, *terms.AccessDurationHours)
	}
	request := CheckoutAttemptCreateRequest{PriceID: price.ID.String()}
	original := checkoutAttemptRequestFingerprint(&request)
	request.AutoRenew = new(true)
	require.Equal(t, original, checkoutAttemptRequestFingerprint(&request), "explicit true and default accept the same renewal terms")
	request.AutoRenew = new(false)
	require.NotEqual(t, original, checkoutAttemptRequestFingerprint(&request), "an idempotency key cannot change the accepted cancellation decision")
}

func TestOrderRenewalRefusesUnsupportedProviderBeforePayment(t *testing.T) {
	price := &models.Price{BillingIntervalHours: new(720)}
	for _, rail := range []models.Rail{models.RailNMI, models.RailStripe} {
		require.NoError(t, validateOrderRenewal(price, new(false), rail))
	}
	for _, rail := range []models.Rail{models.RailCCBill, models.RailSolana} {
		require.NoError(t, validateOrderRenewal(price, nil, rail))
		require.ErrorContains(t, validateOrderRenewal(price, new(false), rail), "cannot guarantee a single term before payment")
	}
	require.Error(t, validateOrderRenewal(&models.Price{}, new(true), models.RailNMI))
	require.NoError(t, validateOrderRenewal(&models.Price{}, new(false), models.RailNMI))
}
