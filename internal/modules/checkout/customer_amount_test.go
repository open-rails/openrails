package checkout

import (
	"encoding/json"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/stretchr/testify/require"
)

func TestCustomerAmountSelection(t *testing.T) {
	price := &models.Price{Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}}
	for _, amount := range []int64{1_000_000, 100_000_000, 500_000_000} {
		selected, err := CheckoutPriceForAmount(price, &amount)
		require.NoError(t, err)
		require.Equal(t, amount, selected.Amount)
		require.Zero(t, price.Amount, "a deposit must not rewrite the shared catalog price")
	}
	for name, amount := range map[string]*int64{"missing": nil, "zero": new(int64(0)), "negative": new(int64(-1)), "too small": new(int64(990_000)), "too large": new(int64(500_010_000)), "fractional cent": new(int64(1_000_001))} {
		_, err := CheckoutPriceForAmount(price, amount)
		require.ErrorIs(t, err, ErrCheckoutAttemptValidation, name)
	}
	for _, currency := range []string{"JPY", "NO_SUCH_CURRENCY"} {
		changed := *price
		changed.Currency = currency
		_, err := CheckoutPriceForAmount(&changed, new(int64(1_000_001)))
		require.ErrorIs(t, err, ErrCheckoutAttemptValidation, currency)
	}
	fixed := &models.Price{Currency: "USD", Amount: 9_990_000}
	selected, err := CheckoutPriceForAmount(fixed, nil)
	require.NoError(t, err)
	require.Same(t, fixed, selected)
	_, err = CheckoutPriceForAmount(fixed, new(fixed.Amount))
	require.ErrorIs(t, err, ErrCheckoutAttemptValidation, "even a matching override is invalid on a fixed pack")
	recurring := *price
	recurring.AutoRenew = true
	_, err = CheckoutPriceForAmount(&recurring, new(int64(1_000_000)))
	require.ErrorIs(t, err, ErrCheckoutAttemptValidation)
}

func TestCustomerAmountBoundToAttemptReplay(t *testing.T) {
	request := &CheckoutAttemptCreateRequest{PriceID: testPriceID, Amount: new(int64(100_000_000)), Payment: CheckoutAttemptPaymentRequest{Rail: "stripe"}}
	response := &CheckoutAttemptResponse{Amount: new(int64(100_000_000)), Payment: CheckoutAttemptPaymentResponse{Rail: "stripe"}}
	payload, err := json.Marshal(checkoutAttemptIdempotencyResult{RequestFingerprint: checkoutAttemptRequestFingerprintForRail(request, nil, "stripe"), Response: response})
	require.NoError(t, err)
	replay, err := decodeCheckoutAttemptIdempotencyResult(payload, request, nil)
	require.NoError(t, err)
	require.Equal(t, response, replay)
	request.Amount = new(int64(200_000_000))
	_, err = decodeCheckoutAttemptIdempotencyResult(payload, request, nil)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
	request.Amount = nil
	_, err = decodeCheckoutAttemptIdempotencyResult(payload, request, nil)
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
}

func TestCustomerAmountOnlyAdvertisesSupportedRails(t *testing.T) {
	price := &models.Price{Currency: "USD", CustomerAmount: &catalog.CustomerAmount{MinAmount: 1_000_000, MaxAmount: 500_000_000}}
	service := &CheckoutAttemptService{}
	for _, rail := range []string{"nmi", "stripe"} {
		cfg := &config.ResolvedPSP{NMI: &config.NMIRailConfig{SecurityKey: "test"}, Stripe: &config.StripeRailConfig{SecretKey: "test"}}
		require.Empty(t, service.checkoutRailSkipReason(price, railTarget{PSP: rail, Rail: rail}, cfg, models.CheckoutAttemptModeOneOff))
	}
	for _, rail := range []string{"solana", "ccbill"} {
		require.Equal(t, models.CheckoutRoutingSkipModeUnsupported, service.checkoutRailSkipReason(price, railTarget{PSP: rail, Rail: rail}, &config.ResolvedPSP{}, models.CheckoutAttemptModeOneOff))
	}
}

func TestCreditOnlyDepositIsNotPermanentOwnership(t *testing.T) {
	price := &models.Price{Currency: "USD"}
	product := &models.Product{CreditGrant: &catalog.CreditGrantSpec{Currency: "USD", FromPayment: true}}
	require.NoError(t, validateOfferAssertion(price, product, "", ""))
	require.ErrorIs(t, validateOfferAssertion(price, product, "", billing.OfferPermanent), ErrCheckoutAttemptValidation)
}
