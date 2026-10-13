package recurring

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	solanatokens "github.com/open-rails/openrails/internal/modules/solana/tokens"
)

// A recurring plan bills in USDC alone: its amount is immutable, so only a
// stablecoin keeps its value. Testnet has no USDC, so no plan.
func TestRecurringAllowlist(t *testing.T) {
	for sym, want := range map[string]bool{
		"USDC": true, " usdc ": true,
		"USD1": false, "DUSD": false, "PYUSD": false, "USDT": false, "SOL": false, "": false,
	} {
		require.Equal(t, want, IsRecurringStablecoinSymbol(sym), sym)
	}
	require.True(t, IsRecurringStablecoinSymbol(solanatokens.PreferredStablecoin))

	for _, network := range []string{"mainnet", "devnet"} {
		mint, err := ResolveRecurringMint("USDC", network)
		require.NoError(t, err)
		require.Equal(t, solanatokens.Accepted(network)["USDC"].Mint, mint)
	}
	_, err := ResolveRecurringMint("USDC", "testnet")
	require.ErrorContains(t, err, "has no configured mint", "testnet has no USDC")

	_, err = ResolveRecurringMint("SOL", "mainnet")
	var typed ErrTokenNotRecurringEligible
	require.ErrorAs(t, err, &typed)
	require.Equal(t, "SOL", typed.Symbol)

	mint, err := ResolveRecurringMintFromTokens("usdc", map[string]config.TokenConfig{"USDC": {Mint: testDevnetUSDCMint}})
	require.NoError(t, err)
	require.Equal(t, testDevnetUSDCMint, mint)
	_, err = ResolveRecurringMintFromTokens("USDC", map[string]config.TokenConfig{"USDC": {Mint: " "}})
	require.Error(t, err)
}

// An operational failure (transport, liveness, cranker out of SOL) retries and
// never duns; subscriber faults and unknowns dun (recoverable); cap-reached
// means already paid; revoked delegate, canceled-at-period-end and ghost plans
// are terminal.
func TestClassifyCrankError(t *testing.T) {
	type want struct {
		code CrankCode
		cat  CrankCategory
		on   int
	}
	operational := want{CommunicationError, Operational, -1}
	dun := want{GenericDecline, Recoverable, -1}
	for _, tc := range []struct {
		err  error
		want want
	}{
		{nil, want{"", "", -1}},
		{errors.New(`{"InstructionError":[0,{"Custom":400}]}`), want{DuplicateTransaction, AlreadyPaid, 400}},
		{errors.New("custom program error: 0x190"), want{DuplicateTransaction, AlreadyPaid, 400}},
		{errors.New(`{"InstructionError":[0,{"Custom":4}]}`), want{DeclinedStopRecurring, Terminal, 4}},
		{errors.New(`{"InstructionError":[0,{"Custom":508}]}`), want{DeclinedStopRecurring, Terminal, 508}},
		{errors.New(`{"InstructionError":[0,{"Custom": 519}]}`), want{DeclinedStopRecurring, Terminal, 519}},
		{errors.New(`{"InstructionError":[0,{"Custom":1}]}`), want{InsufficientFunds, Recoverable, 1}},
		{errors.New("custom program error: 0x1"), want{InsufficientFunds, Recoverable, 1}},
		{errors.New(`{"InstructionError":[0,{"Custom":6001}]}`), want{GenericDecline, Recoverable, 6001}},
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
		require.Equal(t, tc.want.cat == Operational, IsOperationalFailure(tc.err), "%v", tc.err)
	}
}
