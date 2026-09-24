package solana

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const usdcMainnetMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

type priceFeed map[string]float64

func (f priceFeed) PriceUSD(_ context.Context, symbol string) (float64, error) {
	p, ok := f[symbol]
	if !ok {
		return 0, fmt.Errorf("price missing for %s", symbol)
	}
	return p, nil
}

// #817: base units scale with the mint's decimals; below micro precision the
// divide rounds UP; a >1% depeg scales the charge by the live price.
func TestFiatMicrosToStablecoinBaseUnits(t *testing.T) {
	for _, tc := range []struct {
		micros   moneyutil.Micros
		decimals int
		feed     TokenPriceProvider
		want     uint64
	}{
		{1_000_000, 6, nil, 1_000_000},
		{1, 6, nil, 1},
		{19_990_000, 6, nil, 19_990_000},
		{0, 6, nil, 0},
		{-500, 6, nil, 0},
		{10_000_000, 9, nil, 10_000_000_000},
		{1, 9, nil, 1_000},
		{19_990_000, 8, nil, 1_999_000_000},
		{10_000_000, 2, nil, 1_000},
		{10_000_001, 2, nil, 1_001},
		{1_000_000, 6, priceFeed{"USDC": 0.995}, 1_000_000},
		{1_000_000, 6, priceFeed{"USDC": 0.95}, 1_052_632},
		{1_000_000, 9, priceFeed{"USDC": 0.95}, 1_052_631_579},
		{1_000_000, 6, priceFeed{}, 1_000_000}, // feed down holds the peg
	} {
		got, err := FiatMicrosToStablecoinBaseUnits(context.Background(), tc.micros, "USDC", tc.decimals, tc.feed)
		require.NoError(t, err)
		require.Equal(t, tc.want, got, "%d micros @%d feed=%v", tc.micros, tc.decimals, tc.feed)
		if tc.feed == nil {
			peg, err := FiatMicrosToBaseUnitsAtPeg(tc.micros, "USDC", tc.decimals)
			require.NoError(t, err)
			require.Equal(t, tc.want, peg)
		}
	}
	// An omitted decimals decodes as 0 and must fail loudly, not misprice 10^6.
	for _, d := range []int{0, -1, config.MaxTokenDecimals + 1} {
		_, err := FiatMicrosToStablecoinBaseUnits(context.Background(), 1_000_000, "USDC", d, nil)
		require.Error(t, err, "decimals=%d", d)
		_, err = FiatMicrosToBaseUnitsAtPeg(1_000_000, "USDC", d)
		require.Error(t, err, "decimals=%d at peg", d)
	}
}

// #818: the old float chain overcharged 1.19% of whole-cent amounts by one base
// unit. The peg path must be an exact integer rescale for every whole cent.
func TestPegConversionIsExactForEveryWholeCent(t *testing.T) {
	for cents := int64(1); cents <= 200_000; cents++ {
		micros := moneyutil.Micros(cents * 10_000)
		for _, d := range []int{6, 8, 9} {
			got, err := FiatMicrosToBaseUnitsAtPeg(micros, "USDC", d)
			require.NoError(t, err)
			if want := uint64(micros) * pow10(d-6).Uint64(); got != want {
				t.Fatalf("%d micros @%d: got %d, want %d", micros, d, got, want)
			}
		}
	}
}

// The one Solana amount formatter (#863): fixed precision, left-zero-padded
// fraction, no trimming or rounding.
func TestFormatBaseUnits(t *testing.T) {
	for _, tc := range []struct {
		units    uint64
		decimals int
		want     string
	}{
		{1_000_000, 6, "1.000000"},
		{19_990_000, 6, "19.990000"},
		{1, 6, "0.000001"},
		{0, 6, "0.000000"},
		{999_999, 6, "0.999999"},
		{1_000_005, 6, "1.000005"},
		{123_456_789, 8, "1.23456789"},
		{1, 9, "0.000000001"},
		{19_990_000_000, 9, "19.990000000"},
		{15, config.MinTokenDecimals, "1.5"},
		{1, config.MaxTokenDecimals, "0.000000000000000001"},
		{18_446_744_073_709_551_615, 9, "18446744073.709551615"},
		{1_000_000, 0, "1000000"}, // never a bare "."
	} {
		got := FormatBaseUnits(tc.units, tc.decimals)
		require.Equal(t, tc.want, got)
		if tc.decimals > 0 {
			_, frac, _ := strings.Cut(got, ".")
			require.Len(t, frac, tc.decimals, got)
		}
	}
}

// Wire pin for the Solana Pay transfer-request URL: fiat micros in, the exact
// `amount=` the wallet sees out. A 10^n slip anywhere shows as a wrong dollar
// value here.
func TestSolanaPayTransferURLWirePin(t *testing.T) {
	const (
		recipient = "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh"
		mint      = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
		reference = "11111111111111111111111111111112"
	)
	sessionID := uuid.MustParse("0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42")
	s := &SolanaPayService{}

	for _, tc := range []struct {
		micros   moneyutil.Micros
		decimals int
		symbol   string
		memo     string
		want     string
	}{
		{1_000_000, 6, "USDC", "", "?amount=1.000000&spl-token=" + mint + "&reference=" + reference + "&label=Purchase"},
		{10_000, 6, "USDC", "", "?amount=0.010000&spl-token=" + mint + "&reference=" + reference + "&label=Purchase"},
		{19_990_000, 9, "USDC", "", "?amount=19.990000000&spl-token=" + mint + "&reference=" + reference + "&label=Purchase"},
		{19_990_000, 8, "USDC", "", "?amount=19.99000000&spl-token=" + mint + "&reference=" + reference + "&label=Purchase"},
		// #713: the memo precedes the label, URL-escaped.
		{5_000_000, 6, "USDC", solanarpc.PurchaseMemo(sessionID),
			"?amount=5.000000&spl-token=" + mint + "&reference=" + reference +
				"&memo=openrails%3A1%3A0dae1b8f-4c6e-4f6a-9b2d-7e5c3a1f8d42&label=Purchase"},
		// Native SOL carries no spl-token param.
		{1_000_000, 9, "SOL", "", "?amount=1.000000000&reference=" + reference + "&label=Purchase"},
	} {
		units, err := FiatMicrosToBaseUnitsAtPeg(tc.micros, tc.symbol, tc.decimals)
		require.NoError(t, err)
		got := s.buildTransferRequestURL(context.Background(), recipient, units, tc.decimals, mint, tc.symbol, reference, tc.memo)
		require.Equal(t, "solana:"+recipient+tc.want, got)
	}
}

func TestCalculateTokenQuote(t *testing.T) {
	ctx := context.Background()
	usdc := priceFeed{"USDC": 1.0}

	t.Run("usd at peg is an exact rescale (#671 micros, not cents)", func(t *testing.T) {
		q, err := CalculateTokenQuote(ctx, "usdc", usdcMainnetMint, 6, 19_990_000, "usd", nil, usdc)
		require.NoError(t, err)
		require.Equal(t, uint64(19_990_000), q.Units)
		require.Equal(t, "19.990000", q.Amount)
		require.Equal(t, moneyutil.Micros(19_990_000), q.AmountUSDMicros)
		require.Equal(t, "USD", q.FXCurrency)
		require.Equal(t, 1.0, q.FXRate)
	})

	t.Run("fx rate is applied as its exact decimal", func(t *testing.T) {
		mockFX := fx.NewMockProvider(map[string]float64{"eur": 1.08})
		q, err := CalculateTokenQuote(ctx, "USDC", usdcMainnetMint, 6, 10_000_000, "eur", mockFX, usdc)
		require.NoError(t, err)
		require.Equal(t, uint64(10_800_000), q.Units, "1.08 must not add a phantom base unit")
		require.Equal(t, moneyutil.Micros(10_800_000), q.AmountUSDMicros)
		require.Equal(t, "EUR", q.FXCurrency)
	})

	t.Run("depeg failsafe charges more tokens", func(t *testing.T) {
		q, err := CalculateTokenQuote(ctx, "USDC", usdcMainnetMint, 6, 10_000_000, "usd", nil, priceFeed{"USDC": 0.95})
		require.NoError(t, err)
		require.Equal(t, 0.95, q.TokenPriceUSD)
		require.Equal(t, uint64(10_526_316), q.Units)
	})

	t.Run("sub-tolerance noise and missing feed hold the peg", func(t *testing.T) {
		for _, feed := range []TokenPriceProvider{priceFeed{"USDC": 0.999}, nil} {
			q, err := CalculateTokenQuote(ctx, "USDC", usdcMainnetMint, 6, 10_000_000, "usd", nil, feed)
			require.NoError(t, err)
			require.Equal(t, 1.0, q.TokenPriceUSD)
			require.Equal(t, uint64(10_000_000), q.Units)
		}
	})

	t.Run("custom symbol on a known USD mint gets parity (#360)", func(t *testing.T) {
		q, err := CalculateTokenQuote(ctx, "MYUSD", usdcMainnetMint, 6, 10_000_000, "usd", nil, nil)
		require.NoError(t, err)
		require.Equal(t, uint64(10_000_000), q.Units)
	})

	t.Run("volatile token uses the live price", func(t *testing.T) {
		q, err := CalculateTokenQuote(ctx, "SOL", WrappedSOLMint, 9, 15_000_000, "usd", nil, priceFeed{"SOL": 150})
		require.NoError(t, err)
		require.Equal(t, uint64(100_000_000), q.Units)
		require.Equal(t, "0.100000000", q.Amount)
		_, err = CalculateTokenQuote(ctx, "SOL", WrappedSOLMint, 9, 15_000_000, "usd", nil, nil)
		require.Error(t, err)
	})

	t.Run("zero amount", func(t *testing.T) {
		q, err := CalculateTokenQuote(ctx, "USDC", usdcMainnetMint, 6, 0, "usd", nil, nil)
		require.NoError(t, err)
		require.Equal(t, uint64(0), q.Units)
		require.Equal(t, "0.000000", q.Amount)
	})

	t.Run("refusals", func(t *testing.T) {
		mockFX := fx.NewMockProvider(map[string]float64{"xyz": 1.0})
		for name, call := range map[string]func() error{
			"empty currency (#830)": func() error {
				_, err := CalculateTokenQuote(ctx, "USDC", usdcMainnetMint, 6, 1, "  ", nil, usdc)
				return err
			},
			"unregistered currency": func() error {
				_, err := CalculateTokenQuote(ctx, "USDC", usdcMainnetMint, 6, 1, "xyz", mockFX, usdc)
				return err
			},
			"non-usd without fx provider": func() error {
				_, err := CalculateTokenQuote(ctx, "USDC", usdcMainnetMint, 6, 1, "eur", nil, usdc)
				return err
			},
			"missing mint": func() error {
				_, err := CalculateTokenQuote(ctx, "TEST", "", 6, 1, "usd", nil, priceFeed{"TEST": 1})
				return err
			},
			"missing decimals": func() error {
				_, err := CalculateTokenQuote(ctx, "USDC", usdcMainnetMint, 0, 1, "usd", nil, usdc)
				return err
			},
		} {
			require.Error(t, call(), name)
		}
		require.Zero(t, mockFX.CallCount, "an unregistered code must not reach the FX provider")
	})
}
