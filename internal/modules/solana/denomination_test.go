package solana

import (
	"context"
	"testing"
	"time"

	"github.com/open-rails/openrails/internal/integrations/fx"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/require"
)

type forbiddenTokenFeed struct{ calls int }

func (f *forbiddenTokenFeed) PriceUSD(context.Context, string) (float64, error) {
	f.calls++
	return 150, nil
}

func TestNativeTokenDenominationsKeepTheirExactAmount(t *testing.T) {
	fxFeed := fx.NewMockProvider(map[string]float64{"SOL": 150, "USDC": 1})
	prices := &forbiddenTokenFeed{}
	for _, tc := range []struct {
		currency, mint string
		decimals       int
		amount         int64
		display        string
	}{
		{"SOL", WrappedSOLMint, 9, 1_000_000_000, "1.000000000"},
		{"USDC", usdcMainnetMint, 6, 10_000_000, "10.000000"},
		{"SOL", WrappedSOLMint, 9, 1, "0.000000001"},
		{"USDC", usdcMainnetMint, 6, 1, "0.000001"},
	} {
		q, err := CalculateTokenQuote(t.Context(), tc.currency, tc.mint, tc.decimals, moneyutil.Micros(tc.amount), tc.currency, fxFeed, prices, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
		require.NoError(t, err)
		require.Equal(t, uint64(tc.amount), q.Units)
		require.Equal(t, tc.display, q.Amount)
		require.Zero(t, q.TokenPriceUSD, "a direct token amount does not invent a USD price")
	}
	require.Zero(t, fxFeed.CallCount)
	require.Zero(t, prices.calls)
	_, err := CalculateTokenQuote(t.Context(), "USDC", usdcMainnetMint, 6, 1_000_000_000, "SOL", fxFeed, prices, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	require.ErrorContains(t, err, "cross-token conversion")
	_, err = CalculateTokenQuote(t.Context(), "USDC", WrappedSOLMint, 6, 10_000_000, "USDC", fxFeed, prices, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	require.ErrorContains(t, err, "registered token mint")
	_, err = CalculateTokenQuote(t.Context(), "USDC", usdcMainnetMint, 9, 10_000_000, "USDC", fxFeed, prices, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	require.ErrorContains(t, err, "precision")
	require.Zero(t, fxFeed.CallCount)
	require.Zero(t, prices.calls)
}

func TestFiatTokenQuoteUsesRegisteredNativePrecision(t *testing.T) {
	feed := fx.NewMockProvider(map[string]float64{"JPY": 0.01})
	q, err := CalculateTokenQuote(t.Context(), "USDC", usdcMainnetMint, 6, 10_000, "JPY", feed, nil, time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.Equal(t, uint64(10_000), q.Units, "one JPY (10,000 native units) at 0.01 USD is 0.01 USDC")
}

func TestTokenQuoteUsesItsOwningCreationTime(t *testing.T) {
	created := time.Date(2001, 2, 3, 4, 5, 6, 0, time.FixedZone("checkout", 3600))
	for _, tc := range []struct {
		currency string
		amount   moneyutil.Micros
	}{{"USD", 1_000_000}, {"USDC", 1_000_000}, {"USD", 0}} {
		quote, err := CalculateTokenQuote(t.Context(), "USDC", usdcMainnetMint, 6, tc.amount, tc.currency, nil, nil, created)
		require.NoError(t, err)
		require.Equal(t, created.UTC(), quote.QuotedAt, "fiat, token and zero-amount quotes use the same checkout clock")
	}
	_, err := CalculateTokenQuote(t.Context(), "USDC", usdcMainnetMint, 6, 1_000_000, "USDC", nil, nil, time.Time{})
	require.ErrorContains(t, err, "creation time")
}
