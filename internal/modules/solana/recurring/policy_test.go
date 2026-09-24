package recurring

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/billing/declinecode"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// Only plain-SPL stablecoins can back an immutable on-chain plan amount; the
// zero-config accepted token (or#881) must be one, or that merchant can't rebill.
func TestRecurringAllowlist(t *testing.T) {
	for sym, want := range map[string]bool{
		"USDC": true, " usdc ": true, "USD1": true, "DUSD": true,
		"PYUSD": false, "USDG": false, "USDT": false, "SOL": false, "": false,
	} {
		require.Equal(t, want, IsRecurringStablecoinSymbol(sym), sym)
	}
	require.True(t, IsRecurringStablecoinSymbol(solanatokens.DefaultAcceptedSymbol))

	for _, network := range []string{"mainnet", "devnet"} {
		mint, err := ResolveRecurringMint("USDC", network)
		require.NoError(t, err)
		require.Equal(t, solanatokens.ForNetwork(network)["USDC"].Mint, mint)
	}
	_, err := ResolveRecurringMint("USD1", "devnet")
	require.Error(t, err, "USD1 has no devnet mint")

	_, err = ResolveRecurringMint("PYUSD", "mainnet")
	var typed ErrTokenNotRecurringEligible
	require.ErrorAs(t, err, &typed)
	require.Equal(t, "PYUSD", typed.Symbol)

	mint, err := ResolveRecurringMintFromTokens("usdc", map[string]config.TokenConfig{"USDC": {Mint: testDevnetUSDCMint}})
	require.NoError(t, err)
	require.Equal(t, testDevnetUSDCMint, mint, "the configured mint wins over the registry")
	_, err = ResolveRecurringMintFromTokens("USDC", map[string]config.TokenConfig{"USDC": {Mint: " "}})
	require.Error(t, err)
}

// #257/#263: an operational failure (transport, liveness, cranker out of SOL)
// retries and never duns; subscriber faults and unknowns dun (recoverable);
// cap-reached means already paid; revoked delegate, cancelled-at-period-end
// and ghost plans are terminal.
func TestClassifyCrankError(t *testing.T) {
	type want struct {
		code declinecode.Code
		cat  declinecode.Category
		on   int
	}
	operational := want{declinecode.CommunicationError, declinecode.Operational, -1}
	dun := want{declinecode.GenericDecline, declinecode.Recoverable, -1}
	for _, tc := range []struct {
		err  error
		want want
	}{
		{nil, want{"", "", -1}},
		{errors.New(`{"InstructionError":[0,{"Custom":400}]}`), want{declinecode.DuplicateTransaction, declinecode.AlreadyPaid, 400}},
		{errors.New("custom program error: 0x190"), want{declinecode.DuplicateTransaction, declinecode.AlreadyPaid, 400}},
		{errors.New(`{"InstructionError":[0,{"Custom":4}]}`), want{declinecode.DeclinedStopRecurring, declinecode.Terminal, 4}},
		{errors.New(`{"InstructionError":[0,{"Custom":508}]}`), want{declinecode.DeclinedStopRecurring, declinecode.Terminal, 508}},
		{errors.New(`{"InstructionError":[0,{"Custom": 519}]}`), want{declinecode.DeclinedStopRecurring, declinecode.Terminal, 519}},
		{errors.New(`{"InstructionError":[0,{"Custom":1}]}`), want{declinecode.InsufficientFunds, declinecode.Recoverable, 1}},
		{errors.New("custom program error: 0x1"), want{declinecode.InsufficientFunds, declinecode.Recoverable, 1}},
		{errors.New(`{"InstructionError":[0,{"Custom":6001}]}`), want{declinecode.GenericDecline, declinecode.Recoverable, 6001}},
		{errors.New("Transfer: insufficient funds"), dun},
		{errors.New("delegation has been revoked"), dun},
		{errors.New("PlanTermsMismatch"), dun},
		{errors.New("something weird"), dun},
		{context.DeadlineExceeded, operational},
		{fmt.Errorf("submit: %w", context.Canceled), operational},
		{errors.New("rpc error: context deadline exceeded"), operational},
		{errors.New("dial tcp: connection refused"), operational},
		{errors.New("HTTP 429: too many requests"), operational},
		{errors.New("503 service unavailable"), operational},
		{errors.New("transaction was not confirmed in 30s"), operational},
		{errors.New("Blockhash not found"), operational},
		{errors.New("Transaction simulation failed: Insufficient funds for fee"), operational},
		{errors.New("Attempt to debit an account but found no record of a prior credit"), operational},
		// Transport wins over a program code in the same message: a blip is never a decline.
		{errors.New(`timeout after {"Custom":1}`), operational},
	} {
		got := ClassifyCrankError(tc.err)
		require.Equal(t, tc.want, want{got.Code, got.Category, got.OnChainCode}, "%v", tc.err)
		require.Equal(t, tc.want.cat == declinecode.Operational, IsOperationalFailure(tc.err), "%v", tc.err)
	}
}
