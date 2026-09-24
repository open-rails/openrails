package solana

import (
	"context"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/pkg/merchant"
)

// chainMint serves one SPL mint account layout for every address.
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
	if m.err != nil || m.absent {
		return nil, m.err
	}
	blob := make([]byte, 82)
	blob[44] = m.decimals
	blob[45] = 1
	return blob, nil
}

func railsWithTokens(tokens map[string]config.TokenConfig) railresolve.Source {
	return railresolve.FixedSet{"solana": {
		Rail:   models.RailSolana,
		Solana: &config.SolanaRailConfig{Network: "mainnet", Tokens: tokens},
	}}
}

// #817: the decimals shift is read from the mint on-chain, never assumed 6.
func TestRequireTokenDecimalsIsChainSourced(t *testing.T) {
	ctx := context.Background()
	rails := railsWithTokens(map[string]config.TokenConfig{"USDC": {Name: "USD Coin", Mint: usdcMainnetMint}})

	for _, d := range []uint8{2, 6, 8, 9} {
		got, err := RequireTokenDecimals(ctx, rails, "usdc", NewMintDecimals(chainMint{decimals: d}))
		require.NoError(t, err)
		require.Equal(t, int(d), got)
	}

	var typedNil *MintDecimals
	for name, tc := range map[string]struct {
		rails railresolve.Source
		sym   string
		mints MintDecimalsSource
	}{
		"mint account absent":   {rails, "USDC", NewMintDecimals(chainMint{absent: true})},
		"rpc failure":           {rails, "USDC", NewMintDecimals(chainMint{err: errors.New("rpc down")})},
		"0-decimal mint":        {rails, "USDC", NewMintDecimals(chainMint{decimals: 0})},
		"19-decimal mint":       {rails, "USDC", NewMintDecimals(chainMint{decimals: 19})},
		"no resolver":           {rails, "USDC", nil},
		"typed-nil resolver":    {rails, "USDC", typedNil},
		"token not configured":  {rails, "PYUSD", NewMintDecimals(chainMint{decimals: 6})},
		"token has no mint":     {railsWithTokens(map[string]config.TokenConfig{"USDC": {Name: "USD Coin"}}), "USDC", NewMintDecimals(chainMint{decimals: 6})},
		"solana rail not armed": {railresolve.FixedSet{}, "USDC", NewMintDecimals(chainMint{decimals: 6})},
		"blank symbol":          {rails, " ", NewMintDecimals(chainMint{decimals: 6})},
	} {
		_, err := RequireTokenDecimals(ctx, tc.rails, tc.sym, tc.mints)
		require.Error(t, err, name)
	}
}

// Mint decimals are immutable on-chain, so one read per merchant+mint; the key
// is merchant-scoped because merchants may be armed on different clusters.
func TestMintDecimalsCachePerMerchantAndMint(t *testing.T) {
	calls := 0
	mints := NewMintDecimals(chainMint{decimals: 9, calls: &calls})
	ctxA := merchant.WithID(context.Background(), merchant.ID(uuid.New()))
	ctxB := merchant.WithID(context.Background(), merchant.ID(uuid.New()))

	for i := 0; i < 3; i++ {
		for _, ctx := range []context.Context{ctxA, ctxB} {
			d, err := mints.ForMint(ctx, usdcMainnetMint)
			require.NoError(t, err)
			require.Equal(t, 9, d)
		}
	}
	require.Equal(t, 2, calls)

	_, err := mints.ForMint(ctxA, "not-base58!")
	require.Error(t, err)
	_, err = mints.ForMint(ctxA, " ")
	require.Error(t, err)
}
