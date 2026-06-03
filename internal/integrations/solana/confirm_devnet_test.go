//go:build devnet

// Devnet validation of the general transaction-watch helpers (SubmitAndConfirm /
// WatchTransaction) the cranker now relies on to confirm a pull actually landed.
// Uses a plain SOL transfer so it needs no USDC/allowlist/DB.
//
//	Run: SOLANA_DEVNET_PAYER_KEY=<funded> HELIUS_API_KEY=<key> \
//		go test -tags devnet -run SubmitAndConfirm -v -timeout 240s ./internal/integrations/solana/...
package solana

import (
	"context"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/programs/system"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/stretchr/testify/require"
)

func TestDevnetSubmitAndConfirm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	endpoint := devnetEndpoint()
	rc := NewRPCClientWithConfig(RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	dnRaw = rpc.New(endpoint)
	merchant := fundedMerchant(ctx, t, dnRaw)

	dest, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)

	// SUCCESS: a funded transfer, submitted + confirmed via the new helper.
	bh, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	tx, err := solanago.NewTransaction(
		[]solanago.Instruction{system.NewTransferInstruction(2_000_000, merchant.PublicKey(), dest.PublicKey()).Build()},
		bh, solanago.TransactionPayer(merchant.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		if merchant.PublicKey().Equals(pk) {
			return &merchant
		}
		return nil
	})
	require.NoError(t, err)

	outcome, err := rc.SubmitAndConfirm(ctx, tx, 0)
	require.NoError(t, err, "SubmitAndConfirm should not error on a valid tx")
	require.NotNil(t, outcome)
	require.True(t, outcome.Succeeded(), "transfer should succeed on-chain; err=%v", outcome.Err)
	require.Nil(t, outcome.OnChainError())
	t.Logf("SUCCESS path OK: status=%s slot=%d sig=%s", outcome.Status, outcome.Slot, outcome.Signature)

	// WATCH-ONLY: re-watch the same signature by id (the "observe someone else's
	// transaction" path) and confirm it resolves to the same successful outcome.
	watched, err := rc.WatchTransaction(ctx, outcome.Signature, rpc.CommitmentConfirmed, 60*time.Second)
	require.NoError(t, err)
	require.True(t, watched.Succeeded())
	t.Logf("WATCH-by-signature OK: status=%s", watched.Status)

	// FAILURE: a transfer of more lamports than the brand-new dest holds reverts;
	// the helper must report Err (not a Go error) so callers can classify it.
	bh2, err := rc.GetLatestBlockhash(ctx)
	require.NoError(t, err)
	failTx, err := solanago.NewTransaction(
		[]solanago.Instruction{system.NewTransferInstruction(9_000_000_000, dest.PublicKey(), merchant.PublicKey()).Build()},
		bh2, solanago.TransactionPayer(dest.PublicKey()))
	require.NoError(t, err)
	_, err = failTx.Sign(func(pk solanago.PublicKey) *solanago.PrivateKey {
		if dest.PublicKey().Equals(pk) {
			return &dest
		}
		return nil
	})
	require.NoError(t, err)
	failOutcome, err := rc.SubmitAndConfirm(ctx, failTx, 0)
	if err != nil {
		// Default preflight may reject before submit — that's also a surfaced failure.
		t.Logf("FAILURE path: rejected at submit (preflight): %v", err)
	} else {
		require.False(t, failOutcome.Succeeded(), "overspend transfer must not succeed")
		require.Error(t, failOutcome.OnChainError())
		t.Logf("FAILURE path OK: outcome reports on-chain error = %v", failOutcome.OnChainError())
	}
	t.Log("WatchTransaction / SubmitAndConfirm VALIDATED ON DEVNET ✅")
}
