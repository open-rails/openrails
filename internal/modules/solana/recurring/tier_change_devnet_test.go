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
// round-trips across the wallet boundary and the program accepts it on-chain. The
// shape of the partial-sign (cranker pre-signed, wallet completes fee-payer) is
// asserted network-free in completePartialAndSend + prepare_tier_change_test.go.
//
// The initial subscribe to the OLD plan uses the shared devnetSubscribe (the
// atomic [subscribe+transfer], which pulls the old plan's period 1). Each subtest
// funds a FRESH subscriber so authority state never accumulates.
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> go test -tags devnet -run TestDevnetTierChange -v \
//	  -timeout 600s ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestDevnetTierChange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	rc, raw := devnetClients(t)
	merchant, funder := devnetKeys(t)
	usdc := solanago.MustPublicKeyFromBase58(devnetUSDCMintStr)
	requireFunderUSDC(ctx, t, rc, funder, usdc, 4_000_000)
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc) // merchant receiving ATA

	signer := newMerchantSigner(merchant)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := newDevnetPlanService(submitter)
	prepSvc := newDevnetPrepareSubscribeService(submitter, signer, rc)
	tierSvc := newDevnetPrepareTierChangeService(signer, rc)

	const lowTier = uint64(1_000_000)  // 1 USDC plan A
	const highTier = uint64(2_000_000) // 2 USDC plan B

	t.Run("Upgrade", func(t *testing.T) {
		// 3 USDC: low-tier subscribe pulls 1 (period 1 of A), the prorated upgrade
		// pulls 1 more. Fund with headroom.
		sub := freshFundedSubscriber(ctx, t, rc, raw, merchant, funder, usdc, 35_000_000, 3_000_000)
		planA := publishUSDCPlan(ctx, t, planSvc, lowTier, 720)
		planB := publishUSDCPlan(ctx, t, planSvc, highTier, 720)
		devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, planA, lowTier, 720)
		t.Logf("subscribed to plan A (low tier) %s — period 1 pulled atomically", planA.PlanPDA)

		oldPlanPDA := solanago.MustPublicKeyFromBase58(planA.PlanPDA)
		oldSubPDA, _, err := subscriptions.DeriveSubscriptionPDA(oldPlanPDA, sub.PublicKey())
		require.NoError(t, err)

		const prorated = uint64(1_000_000) // new_full(2) - old_unused(1) = 1 USDC, Model-B
		res, err := tierSvc.Prepare(ctx, PrepareTierChangeInput{
			MerchantID: dbtest.TestTenantID, SubscriberWallet: sub.PublicKey().String(), MintSymbol: "USDC",
			OldPlanPDA: planA.PlanPDA, OldSubscriptionPDA: oldSubPDA.String(),
			NewPlanID: planB.PlanID, NewAmountBaseUnits: highTier, NewPeriodHours: 720, NewPlanCreatedAt: planB.CreatedAt,
			IsUpgrade: true, FirstChargeBaseUnits: prorated,
		})
		require.NoError(t, err)
		require.Equal(t, "upgrade", res.Kind)

		beforeMerchant, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
		completePartialAndSend(ctx, t, rc, raw, sub, res.Transaction)
		awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, beforeMerchant+prorated, 90*time.Second,
			"atomic upgrade should pull exactly the prorated first charge into the merchant ATA")
		newSubData, err := rc.GetAccountData(ctx, solanago.MustPublicKeyFromBase58(res.NewSubscriptionPDA))
		require.NoError(t, err)
		require.NotEmpty(t, newSubData, "new subscription PDA should exist on-chain after the upgrade")
		t.Logf("✅ UPGRADE VALIDATED ON DEVNET: co-signed [cancel+subscribe+transfer] landed; merchant %d->%d (+%d prorated); new sub %s",
			beforeMerchant, beforeMerchant+prorated, prorated, res.NewSubscriptionPDA)
	})

	t.Run("Downgrade", func(t *testing.T) {
		// High-tier subscribe pulls 2 USDC (period 1 of B); the downgrade itself pulls
		// nothing. 3 USDC covers it with headroom.
		sub := freshFundedSubscriber(ctx, t, rc, raw, merchant, funder, usdc, 35_000_000, 3_000_000)
		planHigh := publishUSDCPlan(ctx, t, planSvc, highTier, 720)
		planLow := publishUSDCPlan(ctx, t, planSvc, lowTier, 720)
		devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, planHigh, highTier, 720)
		t.Logf("subscribed to plan B (high tier) %s — period 1 pulled atomically", planHigh.PlanPDA)

		oldPlanPDA := solanago.MustPublicKeyFromBase58(planHigh.PlanPDA)
		oldSubPDA, _, err := subscriptions.DeriveSubscriptionPDA(oldPlanPDA, sub.PublicKey())
		require.NoError(t, err)

		res, err := tierSvc.Prepare(ctx, PrepareTierChangeInput{
			MerchantID: dbtest.TestTenantID, SubscriberWallet: sub.PublicKey().String(), MintSymbol: "USDC",
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
