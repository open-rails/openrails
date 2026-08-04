package solana

import (
	"context"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

const mainnetUSDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// chainMint is a fake SPL mint account served by a fake chain reader.
type chainMint struct {
	decimals uint8
	absent   bool
	err      error
	calls    *int
}

func (m chainMint) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	if m.calls != nil {
		*m.calls++
	}
	if m.err != nil {
		return nil, m.err
	}
	if m.absent {
		return nil, nil
	}
	blob := make([]byte, 82)
	blob[44] = m.decimals
	blob[45] = 1 // is_initialized
	return blob, nil
}

func usdcRails() railresolve.Source {
	return railresolve.FixedSet{
		"solana": {
			Rail: models.RailSolana,
			Solana: &config.SolanaRailConfig{
				Network: "mainnet",
				Tokens:  map[string]config.TokenConfig{"USDC": {Name: "USD Coin", Mint: mainnetUSDCMint}},
			},
		},
	}
}

// TestMintDecimalsIsChainSourcedWirePin is the #817 wire pin on the conversion
// boundary: known micros in, exact base-unit integer out, with the shift taken
// from the MINT ON-CHAIN. The same $10.00 price is 10_000_000 base units on a
// 6-decimal mint and 10_000_000_000 on a 9-decimal one — a 1000x difference
// that the old hardcoded 6 got wrong in whichever direction it ran.
func TestMintDecimalsIsChainSourcedWirePin(t *testing.T) {
	ctx := context.Background()
	rails := usdcRails()

	cases := []struct {
		onchain uint8
		micros  moneyutil.Micros
		want    uint64
	}{
		{onchain: 6, micros: 10_000_000, want: 10_000_000},
		{onchain: 9, micros: 10_000_000, want: 10_000_000_000},
		{onchain: 8, micros: 10_000_000, want: 1_000_000_000},
		{onchain: 2, micros: 10_000_000, want: 1_000},
		{onchain: 6, micros: 19_990_000, want: 19_990_000},
		{onchain: 9, micros: 19_990_000, want: 19_990_000_000},
		{onchain: 9, micros: 1, want: 1_000},
	}

	for _, tc := range cases {
		mints := NewMintDecimals(chainMint{decimals: tc.onchain})

		decimals, err := RequireTokenDecimals(ctx, rails, "USDC", mints)
		if err != nil {
			t.Fatalf("RequireTokenDecimals(on-chain %d): %v", tc.onchain, err)
		}
		if decimals != int(tc.onchain) {
			t.Fatalf("decimals = %d, want the mint's %d", decimals, tc.onchain)
		}

		got, err := FiatMicrosToBaseUnitsAtPeg(tc.micros, "USDC", decimals)
		if err != nil {
			t.Fatalf("FiatMicrosToBaseUnitsAtPeg: %v", err)
		}
		if got != tc.want {
			t.Fatalf("%d micros at %d on-chain decimals = %d base units, want %d",
				tc.micros, tc.onchain, got, tc.want)
		}
	}
}

// Decimals are read once per mint and memoized: the value is immutable on-chain
// (written by InitializeMint, never changed), so re-reading it is pure waste.
func TestMintDecimalsCachesImmutableValue(t *testing.T) {
	calls := 0
	mints := NewMintDecimals(chainMint{decimals: 9, calls: &calls})
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		d, err := mints.ForMint(ctx, mainnetUSDCMint)
		if err != nil || d != 9 {
			t.Fatalf("ForMint = (%d, %v), want (9, nil)", d, err)
		}
	}
	if calls != 1 {
		t.Fatalf("chain reads = %d, want 1 (mint decimals are immutable)", calls)
	}
}

// Every unreadable path is an error. Nothing here may fabricate a 6.
func TestMintDecimalsFailsClosed(t *testing.T) {
	ctx := context.Background()
	rails := usdcRails()

	t.Run("mint account absent", func(t *testing.T) {
		if _, err := NewMintDecimals(chainMint{absent: true}).ForMint(ctx, mainnetUSDCMint); err == nil {
			t.Fatal("absent mint must error, not default to 6")
		}
	})

	t.Run("rpc failure", func(t *testing.T) {
		_, err := NewMintDecimals(chainMint{err: errors.New("rpc down")}).ForMint(ctx, mainnetUSDCMint)
		if err == nil {
			t.Fatal("rpc failure must error, not default to 6")
		}
	})

	t.Run("unpayable precision", func(t *testing.T) {
		// A 0-decimal mint cannot express a sub-unit charge; refuse it rather
		// than round every price up to a whole token.
		if _, err := NewMintDecimals(chainMint{decimals: 0}).ForMint(ctx, mainnetUSDCMint); err == nil {
			t.Fatal("0-decimal mint must be refused")
		}
		if _, err := NewMintDecimals(chainMint{decimals: 19}).ForMint(ctx, mainnetUSDCMint); err == nil {
			t.Fatal("19-decimal mint must be refused (outside the payable range)")
		}
	})

	t.Run("no resolver armed", func(t *testing.T) {
		if _, err := RequireTokenDecimals(ctx, rails, "USDC", nil); err == nil {
			t.Fatal("an unarmed resolver must error, not default to 6")
		}
		var typedNil *MintDecimals
		if _, err := RequireTokenDecimals(ctx, rails, "USDC", typedNil); err == nil {
			t.Fatal("a typed-nil resolver must error, not panic or default")
		}
	})

	t.Run("token not configured", func(t *testing.T) {
		mints := NewMintDecimals(chainMint{decimals: 6})
		if _, err := RequireTokenDecimals(ctx, rails, "PYUSD", mints); err == nil {
			t.Fatal("an unconfigured token must error")
		}
	})

	t.Run("token has no mint", func(t *testing.T) {
		rails := railresolve.FixedSet{"solana": {
			Rail:   models.RailSolana,
			Solana: &config.SolanaRailConfig{Tokens: map[string]config.TokenConfig{"USDC": {Name: "USD Coin"}}},
		}}
		mints := NewMintDecimals(chainMint{decimals: 6})
		if _, err := RequireTokenDecimals(ctx, rails, "USDC", mints); err == nil {
			t.Fatal("a mintless token must error")
		}
	})
}
