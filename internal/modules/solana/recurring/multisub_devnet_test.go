//go:build devnet

// On-chain validation (real USDC) of the MULTI-SUBSCRIPTION rules: a single user
// holding several subs under the same merchant for DIFFERENT plans, and the
// independence guarantees between them. Each subscribe uses the ATOMIC subscribe
// (#286), so each plan's PERIOD 1 is pulled in its own subscribe tx.
//
// Two tests (each fresh-funds its own wallet; cancelling A is destructive, so they
// can't share one):
//
//   - MultiProductRules : a user CAN hold subs to two DIFFERENT plans under one
//     merchant (each atomic subscribe pulls its own 1 USDC independently); and
//     re-subscribing the SAME plan is rejected on-chain (the Subscription PDA
//     already exists), backstopping the app-layer duplicate-billing guard (#269).
//     This also exercises the Custom:519 fix: the 2nd distinct subscribe lands
//     correctly through the shared atomic-subscribe path (no client created_at drift).
//
//   - IndependentPerSubCancel : cancelling ONE subscription (A) on-chain stops A's
//     pulls while subscription B keeps pulling — the SPL delegate is shared per
//     (user,mint), so the program's own cancel_subscription/revoke_delegation must
//     target a SINGLE sub. Proven by: crank A fails, crank B succeeds.
//
//     SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//     HELIUS_API_KEY=<key> go test -tags devnet -run TestDevnetMultiSub -v \
//     -timeout 900s ./internal/modules/solana/recurring/...
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

func TestDevnetMultiSub(t *testing.T) {
	usdc := solanago.MustPublicKeyFromBase58(devnetUSDCMintStr)
	const amount = uint64(1_000_000)

	// MultiProductRules — different products under one merchant subscribe (and pull
	// period 1) independently; a duplicate same-plan subscribe is rejected on-chain.
	t.Run("MultiProductRules", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()
		rc, raw := devnetClients(t)
		merchant, funder := devnetKeys(t)
		requireFunderUSDC(ctx, t, rc, funder, usdc, 3_000_000)
		ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)

		signer := newMerchantSigner(merchant)
		submitter := NewSignerSubmitter(signer, rc)
		planSvc := newDevnetPlanService(submitter)
		prepSvc := newDevnetPrepareSubscribeService(submitter, signer, rc)

		// 3 USDC: two atomic subscribes pull 1 each (period 1 of A + of B) + headroom.
		sub := freshFundedSubscriber(ctx, t, rc, raw, merchant, funder, usdc, 50_000_000, 3_000_000)
		hA := publishUSDCPlan(ctx, t, planSvc, amount, 720)
		hB := publishUSDCPlan(ctx, t, planSvc, amount, 720)

		// Subscribe A then B (different plans, same merchant). Each atomic subscribe
		// pulls its own period 1; the merchant receives 1 USDC per distinct product.
		base, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
		devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, hA, amount, 720)
		awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, base+amount, 90*time.Second, "plan A period-1 pull")
		devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, hB, amount, 720)
		awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, base+2*amount, 90*time.Second, "plan B period-1 pull")
		t.Log("✅ two distinct products under one merchant each subscribed + pulled 1 USDC independently (2nd subscribe landed — 519 fix confirmed)")

		// DUPLICATE SAME PLAN: re-subscribing plan A must NOT succeed (the Subscription
		// PDA already exists). Either Prepare refuses, or the tx reverts on-chain.
		dupRes, dupErr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
			MerchantID: dbtest.TestTenantID, SubscriberWallet: sub.PublicKey().String(),
			PlanID: hA.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720, PlanCreatedAt: hA.CreatedAt,
		})
		if dupErr != nil {
			t.Logf("✅ duplicate same-plan subscribe refused at prepare: %v", dupErr)
		} else {
			require.Equal(t, "subscribe", dupRes.Step, "authority exists -> the duplicate goes straight to the subscribe step")
			e, ok := trySignSend(ctx, t, raw, rc, sub, dupRes.Transactions)
			require.False(t, ok, "re-subscribing the SAME plan A must be rejected on-chain (Subscription PDA already exists)")
			t.Logf("✅ duplicate same-plan subscribe rejected on-chain: %s", e)
		}
	})

	// IndependentPerSubCancel — cancelling subscription A on-chain stops A's pulls
	// while subscription B keeps pulling.
	t.Run("IndependentPerSubCancel", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
		defer cancel()
		rc, raw := devnetClients(t)
		merchant, funder := devnetKeys(t)
		requireFunderUSDC(ctx, t, rc, funder, usdc, 4_000_000)
		ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)

		signer := newMerchantSigner(merchant)
		submitter := NewSignerSubmitter(signer, rc)
		planSvc := newDevnetPlanService(submitter)
		prepSvc := newDevnetPrepareSubscribeService(submitter, signer, rc)
		crankSvc := NewCrankService(submitter)
		eventAuth, _, err := subscriptions.DeriveEventAuthority()
		require.NoError(t, err)

		// 4 USDC: A + B each pull period 1 via subscribe; B is cranked again after
		// A's cancel (within B's period -> already-paid, so the proof is err==nil vs
		// err on the cancelled A; we assert A FAILS and B does NOT fail terminally).
		sub := freshFundedSubscriber(ctx, t, rc, raw, merchant, funder, usdc, 60_000_000, 4_000_000)
		hA := publishUSDCPlan(ctx, t, planSvc, amount, 720)
		hB := publishUSDCPlan(ctx, t, planSvc, amount, 720)
		rowA := devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, hA, amount, 720)
		rowB := devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, hB, amount, 720)
		subA := solanago.MustPublicKeyFromBase58(rowA.SubscriptionPDA)
		planA := solanago.MustPublicKeyFromBase58(hA.PlanPDA)

		// Cancel A on-chain WITHOUT touching the shared SPL delegate (that would kill B
		// too). cancel_subscription(A) then revoke_delegation(A) closes A's account.
		sendIx := func(ixs ...solanago.Instruction) (string, bool) {
			b64, e := buildTierChangeUnsignedTxBase64(ctx, rc, sub.PublicKey(), ixs)
			require.NoError(t, e)
			return trySignSend(ctx, t, raw, rc, sub, []string{b64})
		}
		cancelA := subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
			Subscriber: sub.PublicKey(), PlanPDA: planA, SubscriptionPDA: subA, EventAuthority: eventAuth,
		})
		if e, ok := sendIx(cancelA); !ok {
			t.Logf("cancel_subscription(A) on-chain err: %s", e)
		} else {
			t.Log("cancel_subscription(A) landed")
		}
		revokeA := subscriptions.BuildRevokeDelegation(sub.PublicKey(), subA, planA)
		if e, ok := sendIx(revokeA); ok {
			t.Log("revoke_delegation(A) landed — A's subscription/delegation account closed")
		} else {
			t.Logf("revoke_delegation(A) on-chain err: %s (may require the subscription to be expired/cancelled first)", e)
		}

		// Decisive: crank A must FAIL (cancelled/closed); crank B must NOT be a
		// terminal/closed failure. B's period 1 was just paid by its subscribe, so an
		// immediate crank is AlreadyPaid (Custom:400) — that is success for B: it is
		// still a LIVE, billable subscription, unaffected by A's cancel.
		_, errA := crankSvc.Crank(ctx, dbtest.TestTenantID, rowA, amount)
		require.Error(t, errA, "crank on the CANCELLED subscription A must fail on-chain")
		cfA := ClassifyCrankError(errA)
		require.NotEqual(t, onchainCapReached, cfA.OnChainCode,
			"A's failure must be from the cancel/close (508/revoked/closed), NOT an in-period cap — got %+v", cfA)
		t.Logf("crank A (cancelled): Custom:%d -> %s", cfA.OnChainCode, cfA.Category)

		_, errB := crankSvc.Crank(ctx, dbtest.TestTenantID, rowB, amount)
		require.Error(t, errB, "B's period 1 was paid by subscribe, so an immediate crank is AlreadyPaid")
		cfB := ClassifyCrankError(errB)
		require.Equalf(t, onchainCapReached, cfB.OnChainCode,
			"B must still be LIVE (AlreadyPaid/Custom:400), NOT cancelled — A's cancel must not affect B; got %+v", cfB)
		t.Logf("crank B (active): Custom:%d -> %s (still live + billable, independent of A's cancel)", cfB.OnChainCode, cfB.Category)

		t.Log("✅ INDEPENDENT PER-SUB CANCEL VALIDATED: cancelling A stopped A's pulls (508/revoked) while B stayed live (AlreadyPaid) — per-sub cancellation is independent.")
	})
}
