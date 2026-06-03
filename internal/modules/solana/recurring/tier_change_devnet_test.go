//go:build devnet

// End-to-end validation of the REAL tier-change service (#272) against devnet
// with REAL USDC. This proves the atomic on-chain bundles the production
// PrepareTierChangeService builds actually execute:
//
//	UPGRADE   = co-signed [cancel(old) + subscribe(new) + transfer_subscription(new, prorated)]
//	            the cranker pre-signs its slot, the wallet completes the fee-payer
//	            slot, and the prorated first pull lands atomically with the switch.
//	DOWNGRADE = unsigned [cancel(old) + subscribe(new)] — wallet signs alone, NO
//	            charge (the first pull is deferred by the cranker).
//
// The DB mirror (ConfirmTierChangeService) is pure logic + unit-tested; this test
// validates the one thing mocks can't — that the partially-signed upgrade bundle
// round-trips across the wallet boundary and the program accepts it on-chain.
//
// Prereqs: the funder wallet (SOLANA_DEVNET_SUBSCRIBER_KEY) holds faucet USDC
// (https://faucet.circle.com, Solana devnet) and the payer (SOLANA_DEVNET_PAYER_KEY)
// holds SOL. Each subtest funds a FRESH subscriber so authority state never
// accumulates.
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> go test -tags devnet -run TierChange -v \
//	  -timeout 600s ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
)

func TestDevnetTierChangeUSDC(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	endpoint := "https://devnet.helius-rpc.com/?api-key=" + devnetEnv(t, "HELIUS_API_KEY")
	rc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)

	merchant, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_PAYER_KEY"))
	require.NoError(t, err)
	funder, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_SUBSCRIBER_KEY"))
	require.NoError(t, err)

	usdc := solanago.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	funderUSDC, err := rc.GetTokenBalanceForMint(ctx, funder.PublicKey(), usdc)
	if err != nil || funderUSDC < 3_000_000 {
		t.Skipf("funder %s has %d USDC base units (<3 USDC) — fund via https://faucet.circle.com (Solana devnet, USDC) then re-run",
			funder.PublicKey(), funderUSDC)
	}
	t.Logf("funder USDC balance: %d base units", funderUSDC)

	// The cranker pulls USDC INTO the merchant's own USDC ATA — ensure it exists.
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)

	signer := solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := NewPlanService(submitter, "devnet")
	prepSvc := NewPrepareSubscribeService(submitter, signer, rc, "devnet")
	tierSvc := NewPrepareTierChangeService(signer, rc, "devnet")

	const lowTier = uint64(1_000_000)  // 1 USDC plan A
	const highTier = uint64(2_000_000) // 2 USDC plan B

	// publishPlan creates a USDC/720h plan on-chain and returns its handle.
	publishPlan := func(amount uint64) *PlanHandle {
		h, perr := planSvc.PublishPlan(ctx, PublishPlanInput{
			TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
			TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720,
		})
		require.NoError(t, perr, "PublishPlan(%d) should create the plan on-chain", amount)
		return h
	}

	// subscribe runs the prepare->sign loop (init then subscribe) for `sub` to `h`.
	subscribe := func(sub solanago.PrivateKey, h *PlanHandle, amount uint64) {
		for step := 0; step < 2; step++ {
			res, perr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
				TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(),
				PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720, PlanCreatedAt: h.CreatedAt,
			})
			require.NoError(t, perr)
			signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions)
			if res.Step == "subscribe" {
				break
			}
		}
	}

	t.Run("Upgrade", func(t *testing.T) {
		// Fresh subscriber funded with 2 USDC (1 will be pulled by the prorated upgrade).
		sub, err := solanago.NewRandomPrivateKey()
		require.NoError(t, err)
		t.Logf("upgrade subscriber: %s", sub.PublicKey())
		fundSOL(ctx, t, rc, raw, merchant, sub.PublicKey(), 35_000_000)
		ensureATA(ctx, t, rc, raw, merchant, sub.PublicKey(), usdc)
		transferUSDC(ctx, t, rc, raw, funder, sub.PublicKey(), usdc, 2_000_000)

		planA := publishPlan(lowTier)
		planB := publishPlan(highTier)
		subscribe(sub, planA, lowTier)
		t.Logf("subscribed to plan A (low tier) %s", planA.PlanPDA)

		oldPlanPDA := solanago.MustPublicKeyFromBase58(planA.PlanPDA)
		oldSubPDA, _, err := subscriptions.DeriveSubscriptionPDA(oldPlanPDA, sub.PublicKey())
		require.NoError(t, err)

		const prorated = uint64(1_000_000) // new_full(2) - old_unused(1) = 1 USDC, Model-B
		res, err := tierSvc.Prepare(ctx, PrepareTierChangeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(), MintSymbol: "USDC",
			OldPlanPDA: planA.PlanPDA, OldSubscriptionPDA: oldSubPDA.String(),
			NewPlanID: planB.PlanID, NewAmountBaseUnits: highTier, NewPeriodHours: 720, NewPlanCreatedAt: planB.CreatedAt,
			IsUpgrade: true, FirstChargeBaseUnits: prorated,
		})
		require.NoError(t, err)
		require.Equal(t, "upgrade", res.Kind)

		beforeMerchant, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
		completePartialAndSend(ctx, t, rc, raw, sub, res.Transaction)
		// Poll past devnet RPC read-after-confirm lag: the co-signed transfer already
		// confirmed on-chain; wait for a node to reflect the prorated credit.
		awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, beforeMerchant+prorated, 90*time.Second,
			"atomic upgrade should pull exactly the prorated first charge into the merchant ATA")
		newSubData, err := rc.GetAccountData(ctx, solanago.MustPublicKeyFromBase58(res.NewSubscriptionPDA))
		require.NoError(t, err)
		require.NotEmpty(t, newSubData, "new subscription PDA should exist on-chain after the upgrade")
		t.Logf("✅ UPGRADE VALIDATED ON DEVNET: co-signed [cancel+subscribe+transfer] landed; merchant %d->%d (+%d prorated); new sub %s",
			beforeMerchant, beforeMerchant+prorated, prorated, res.NewSubscriptionPDA)
	})

	t.Run("Downgrade", func(t *testing.T) {
		// Downgrade subscribes (no pull) -> needs only SOL + an (empty) USDC ATA.
		sub, err := solanago.NewRandomPrivateKey()
		require.NoError(t, err)
		t.Logf("downgrade subscriber: %s", sub.PublicKey())
		fundSOL(ctx, t, rc, raw, merchant, sub.PublicKey(), 35_000_000)
		ensureATA(ctx, t, rc, raw, merchant, sub.PublicKey(), usdc)

		planHigh := publishPlan(highTier)
		planLow := publishPlan(lowTier)
		subscribe(sub, planHigh, highTier)
		t.Logf("subscribed to plan B (high tier) %s", planHigh.PlanPDA)

		oldPlanPDA := solanago.MustPublicKeyFromBase58(planHigh.PlanPDA)
		oldSubPDA, _, err := subscriptions.DeriveSubscriptionPDA(oldPlanPDA, sub.PublicKey())
		require.NoError(t, err)

		res, err := tierSvc.Prepare(ctx, PrepareTierChangeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(), MintSymbol: "USDC",
			OldPlanPDA: planHigh.PlanPDA, OldSubscriptionPDA: oldSubPDA.String(),
			NewPlanID: planLow.PlanID, NewAmountBaseUnits: lowTier, NewPeriodHours: 720, NewPlanCreatedAt: planLow.CreatedAt,
			IsUpgrade: false,
		})
		require.NoError(t, err)
		require.Equal(t, "downgrade", res.Kind)
		// Downgrade is fully unsigned — the wallet signs alone.
		downgradeTx := decodeTxBase64(t, res.Transaction)
		require.Equal(t, 1, int(downgradeTx.Message.Header.NumRequiredSignatures), "downgrade is single-signer")

		beforeMerchant, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
		signAndSendBase64(ctx, t, rc, raw, sub, []string{res.Transaction})
		afterMerchant, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)

		require.Equal(t, beforeMerchant, afterMerchant, "downgrade must NOT pull any USDC (first charge is deferred)")
		newSubData, err := rc.GetAccountData(ctx, solanago.MustPublicKeyFromBase58(res.NewSubscriptionPDA))
		require.NoError(t, err)
		require.NotEmpty(t, newSubData, "new (lower-tier) subscription PDA should exist after the downgrade")
		t.Logf("✅ DOWNGRADE VALIDATED ON DEVNET: unsigned [cancel+subscribe] landed; no USDC pulled; new sub %s", res.NewSubscriptionPDA)
	})
}

// completePartialAndSend takes the partially-signed upgrade bundle (cranker slot
// already signed against its blockhash), has the wallet sign ONLY the fee-payer
// slot (index 0) WITHOUT touching the message — preserving the cranker's
// signature — then submits (skip-preflight) and confirms. This mirrors what the
// real browser wallet does: signAllTransactions over the bytes OpenRails returned.
func completePartialAndSend(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, raw *rpc.Client, wallet solanago.PrivateKey, b64 string) {
	t.Helper()
	tx := decodeTxBase64(t, b64)
	require.Equal(t, 2, int(tx.Message.Header.NumRequiredSignatures), "upgrade bundle has two signers")
	require.True(t, wallet.PublicKey().Equals(tx.Message.AccountKeys[0]), "wallet must be the fee payer (signer 0)")
	require.True(t, tx.Signatures[0].IsZero(), "fee-payer slot must be empty for the wallet")
	require.False(t, tx.Signatures[1].IsZero(), "cranker slot must already be pre-signed")

	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	sig, err := wallet.Sign(msg)
	require.NoError(t, err)
	tx.Signatures[0] = sig
	require.NoError(t, tx.VerifySignatures(), "fully + validly signed after the wallet completes it")

	txid, err := raw.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentConfirmed})
	require.NoError(t, err)
	out, werr := rc.WatchTransaction(ctx, txid, rpc.CommitmentConfirmed, 90*time.Second)
	require.NoError(t, werr)
	require.Nil(t, out.OnChainError(), "upgrade tx %s failed on-chain", txid)
}

func decodeTxBase64(t *testing.T, b64 string) *solanago.Transaction {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(b64)
	require.NoError(t, err)
	tx, err := solanago.TransactionFromBytes(data)
	require.NoError(t, err)
	return tx
}
