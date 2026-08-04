package solana

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

type fakeTokenPriceProvider map[string]float64

func (f fakeTokenPriceProvider) PriceUSD(_ context.Context, symbol string) (float64, error) {
	price, ok := f[symbol]
	if !ok {
		return 0, fmt.Errorf("price missing for %s", symbol)
	}
	return price, nil
}

const usdcMainnetMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

func TestCalculateTokenQuote_USDPrice(t *testing.T) {
	quote, err := CalculateTokenQuote(context.Background(), "USDC", usdcMainnetMint, 6, 10_000_000, "usd", nil, fakeTokenPriceProvider{"USDC": 1.0})
	require.NoError(t, err)
	require.Equal(t, uint64(10_000_000), quote.Units)
	require.Equal(t, "10.000000", quote.Amount)
	require.Equal(t, 1.0, quote.FXRate)
	require.Equal(t, "USD", quote.FXCurrency)
}

// TestCalculateTokenQuote_MicrosWirePin pins the #671 units contract on the
// quote seam: a $19.99 price (19_990_000 MICROS) must quote 19.99 USDC =
// 19_990_000 base units — never the 10,000x cents-misread (199_900_000_000).
func TestCalculateTokenQuote_MicrosWirePin(t *testing.T) {
	quote, err := CalculateTokenQuote(context.Background(), "USDC", usdcMainnetMint, 6, 19_990_000, "usd", nil, fakeTokenPriceProvider{"USDC": 1.0})
	require.NoError(t, err)
	require.Equal(t, uint64(19_990_000), quote.Units) // 19.99 USDC in base units
	require.Equal(t, "19.990000", quote.Amount)
	require.Equal(t, moneyutil.Micros(19_990_000), quote.AmountUSDMicros)
}

func TestCalculateTokenQuote_NonUSDPrice_RequiresFXProvider(t *testing.T) {
	_, err := CalculateTokenQuote(context.Background(), "USDC", usdcMainnetMint, 6, 10_000_000, "eur", nil, fakeTokenPriceProvider{"USDC": 1.0})
	require.Error(t, err)
}

func TestCalculateTokenQuote_ZeroAmount(t *testing.T) {
	quote, err := CalculateTokenQuote(context.Background(), "USDC", usdcMainnetMint, 6, 0, "usd", nil, nil)
	require.NoError(t, err)
	require.Equal(t, uint64(0), quote.Units)
	require.Equal(t, "0.000000", quote.Amount)
}

// #830: an absent currency is refused, never defaulted to "usd".
func TestCalculateTokenQuote_EmptyCurrencyIsRefused(t *testing.T) {
	for _, currency := range []string{"", "   "} {
		_, err := CalculateTokenQuote(context.Background(), "USDC", usdcMainnetMint, 6, 10_000_000, currency, nil, fakeTokenPriceProvider{"USDC": 1.0})
		require.ErrorContains(t, err, "requires a currency")
	}
}

// An unregistered code must not reach the FX provider or the persisted quote.
func TestCalculateTokenQuote_UnregisteredCurrencyIsRefused(t *testing.T) {
	mockFX := fx.NewMockProvider(map[string]float64{"xyz": 1.0})

	_, err := CalculateTokenQuote(context.Background(), "USDC", usdcMainnetMint, 6, 10_000_000, "xyz", mockFX, fakeTokenPriceProvider{"USDC": 1.0})
	require.ErrorContains(t, err, "unknown currency")
	require.Zero(t, mockFX.CallCount)
}

func TestCalculateTokenQuote_MissingMint(t *testing.T) {
	_, err := CalculateTokenQuote(context.Background(), "TEST", "", 6, 10_000_000, "usd", nil, fakeTokenPriceProvider{"TEST": 1.0})
	require.Error(t, err)
}

func TestCalculateTokenQuote_WithMockFXProvider(t *testing.T) {
	mockFX := fx.NewMockProvider(map[string]float64{
		"eur": 1.08,
		"gbp": 1.27,
	})

	quote, err := CalculateTokenQuote(context.Background(), "USDC", usdcMainnetMint, 6, 10_000_000, "eur", mockFX, fakeTokenPriceProvider{"USDC": 1.0})
	require.NoError(t, err)
	require.Equal(t, moneyutil.Micros(10_800_000), quote.AmountUSDMicros)
	require.Equal(t, 1.08, quote.FXRate)
	require.Equal(t, "EUR", quote.FXCurrency)
}

func TestSolanaPaymentMatchesPendingRequiresSameReferenceAndSession(t *testing.T) {
	priceID := uuid.New()
	pending := &PendingSolanaPayment{
		UserID:    "user_123",
		PriceID:   priceID.String(),
		SessionID: "session_123",
		Amount:    1000,
		Currency:  "USD",
	}
	payment := &models.Payment{
		CustomerID: identity.CustomerIDFromString("user_123").UUID(),
		PriceID:    priceID,
		Amount:     1000,
		Currency:   "USD",
		Metadata: map[string]any{
			"solana_reference":    "reference_123",
			"checkout_session_id": "session_123",
		},
	}

	require.True(t, solanaPaymentMatchesPending(payment, "reference_123", pending))
	require.False(t, solanaPaymentMatchesPending(payment, "other_reference", pending))

	payment.Metadata["checkout_session_id"] = "other_session"
	require.False(t, solanaPaymentMatchesPending(payment, "reference_123", pending))

	payment.Metadata["checkout_session_id"] = "session_123"
	pending.SessionID = ""
	require.False(t, solanaPaymentMatchesPending(payment, "reference_123", pending))
}

func TestGeneratePaymentRequiresCheckoutSession(t *testing.T) {
	svc := &SolanaPayService{}
	_, err := svc.GeneratePayment(context.Background(), "user_123", uuid.New(), "USDC", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checkout session id is required")

	nilID := uuid.Nil
	_, err = svc.GeneratePayment(context.Background(), "user_123", uuid.New(), "USDC", &nilID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "checkout session id is required")
}

func TestStablecoinPegAndDepegFailsafe(t *testing.T) {
	ctx := context.Background()

	// USDC within 1% of peg -> use clean $1.00 (ignore sub-penny noise).
	q, err := CalculateTokenQuote(ctx, "USDC", "mint", 6, 10_000_000, "usd", nil, fakeTokenPriceProvider{"USDC": 0.999})
	require.NoError(t, err)
	require.Equal(t, 1.0, q.TokenPriceUSD)

	// USDC depegged >1% -> failsafe uses the live price (charge compensates).
	q, err = CalculateTokenQuote(ctx, "USDC", "mint", 6, 10_000_000, "usd", nil, fakeTokenPriceProvider{"USDC": 0.95})
	require.NoError(t, err)
	require.Equal(t, 0.95, q.TokenPriceUSD)
	require.Greater(t, q.Units, uint64(10_000_000)) // $10 / $0.95 > 10 tokens

	// Feedless stablecoin (USD1) -> always $1.00, no provider consulted.
	q, err = CalculateTokenQuote(ctx, "USD1", "mint", 6, 10_000_000, "usd", nil, nil)
	require.NoError(t, err)
	require.Equal(t, 1.0, q.TokenPriceUSD)
}
