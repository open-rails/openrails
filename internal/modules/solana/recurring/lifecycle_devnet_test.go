//go:build devnet

// FULL recurring lifecycle on devnet with REAL USDC, end to end through the
// PRODUCTION services (#263/#286): PlanService.PublishPlan -> PrepareSubscribeService
// (the ATOMIC co-signed [subscribe+transfer]) -> CrankService.Crank (period 2
// rebill after the on-chain period rolls over) -> cancel that STOPS the merchant
// from pulling any further period. This exercises the real allowlist resolution,
// the verified instruction builders, and submit+confirm — not raw builders. No
// DB: the membership/ledger side (EnrollService / ConfirmCancelService) is
// unit-tested + covered in the docker stack; this proves the on-chain service
// layer with real money.
//
// Atomic-subscribe reality (#286): the subscribe step returns ONE co-signed
// [subscribe + transfer(first period)] tx — devnetSubscribe completes the wallet
// slot WITHOUT dropping the cranker's pre-signature, so the subscription is
// created AND the first period is pulled in a single transaction. The first
// SEPARATE crank a test issues is therefore PERIOD 2, which needs the ~1h
// rollover. The shape of that atomic bundle (2 instructions, 2 signers, exactly
// one pre-signed) is asserted network-free in atomic_subscribe_test.go.
//
// Two tests:
//
//   - LifecycleFastPlan : the fast end-to-end (720h plan) — publish, atomic
//     subscribe (period 1 pulled), cancel. Proves the happy path + the co-signed
//     subscribe land on real chain in ~minutes. Run this in the normal devnet pass.
//
//   - CancelStopsAtPeriodEnd : the SLOW (~1h) proof that cancel genuinely stops
//     future pulls — subscribe (period 1), cancel, then prove a post-rollover pull
//     is rejected with Custom:508 (SubscriptionCancelled), never a period-2 charge.
//
//     SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//     HELIUS_API_KEY=<key> go test -tags devnet -run TestDevnetLifecycle -v \
//     -timeout 2h ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/stretchr/testify/require"
)

func TestDevnetLifecycle(t *testing.T) {
	usdc := solanago.MustPublicKeyFromBase58(devnetUSDCMintStr)
	const amount = uint64(1_000_000) // 1 USDC / period

	// LifecycleFastPlan — the fast happy path on a 720h plan: publish -> atomic
	// subscribe (pulls period 1 in the same tx) -> cancel. Proves the co-signed
	// subscribe round-trips across the wallet boundary on real chain in minutes.
	t.Run("FastPlan", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
		defer cancel()
		rc, raw := devnetClients(t)
		merchant, funder := devnetKeys(t)
		requireFunderUSDC(ctx, t, rc, funder, usdc, 2_000_000)
		ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc) // merchant receiving ATA

		signer := newMerchantSigner(merchant)
		submitter := NewSignerSubmitter(signer, rc)
		planSvc := newDevnetPlanService(submitter)
		prepSvc := newDevnetPrepareSubscribeService(submitter, signer, rc)

		sub := freshFundedSubscriber(ctx, t, rc, raw, merchant, funder, usdc, 30_000_000, 2_000_000)

		const periodHours = uint64(720)
		h := publishUSDCPlan(ctx, t, planSvc, amount, periodHours)
		t.Logf("1) PublishPlan OK: plan_pda=%s", h.PlanPDA)

		// 2) ATOMIC subscribe — creates the sub AND pulls period 1 in one co-signed tx.
		before, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
		row := devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, h, amount, periodHours)
		awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, before+amount, 90*time.Second,
			"atomic subscribe must pull the first period (1 USDC) in the SAME tx")
		subPDA := solanago.MustPublicKeyFromBase58(row.SubscriptionPDA)
		data, err := rc.GetAccountData(ctx, subPDA)
		require.NoError(t, err)
		require.NotEmpty(t, data, "subscription PDA must exist after the atomic subscribe")
		t.Logf("2) Atomic subscribe OK: sub %s created + period 1 pulled (merchant %d->%d)", subPDA, before, before+amount)

		// 3) Cancel on-chain (PrepareCancelService path), signed by the wallet.
		cancelSvc := NewPrepareCancelService(fixedSubReader{row: row}, rc)
		cres, xerr := cancelSvc.Prepare(ctx, row.SubscriptionID)
		require.NoError(t, xerr)
		signAndSendBase64(ctx, t, rc, raw, sub, []string{cres.Transaction})
		t.Logf("3) Cancel OK: cancel_subscription submitted for %s", cres.SubscriptionPDA)

		t.Log("✅ LIFECYCLE (fast) VALIDATED ON DEVNET: publish -> atomic subscribe (period 1) -> cancel")
	})

	// CancelStopsAtPeriodEnd — the SLOW proof (~1h) that cancel actually stops
	// future pulls. Subscribe to a 1h plan (period 1 pulled atomically), cancel,
	// then assert: (1) an in-period re-pull is cap-rejected (Custom:400) and
	// (2) AFTER the period rolls over the pull is rejected with Custom:508
	// (SubscriptionCancelled) — NOT a successful period-2 charge. Final invariant:
	// the subscriber paid for exactly ONE period.
	t.Run("CancelStopsAtPeriodEnd", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		rc, raw := devnetClients(t)
		merchant, funder := devnetKeys(t)
		requireFunderUSDC(ctx, t, rc, funder, usdc, 2_000_000)
		ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)

		signer := newMerchantSigner(merchant)
		submitter := NewSignerSubmitter(signer, rc)
		planSvc := newDevnetPlanService(submitter)
		prepSvc := newDevnetPrepareSubscribeService(submitter, signer, rc)
		crankSvc := NewCrankService(submitter)
		eventAuth, _, err := subscriptions.DeriveEventAuthority()
		require.NoError(t, err)

		// 2 USDC: the atomic subscribe pulls period 1; we need headroom only so the
		// would-be period-2 pull is rejected by cancel/cap, not by an empty wallet.
		sub := freshFundedSubscriber(ctx, t, rc, raw, merchant, funder, usdc, 40_000_000, 2_000_000)

		const periodHours = uint64(1)
		h := publishUSDCPlan(ctx, t, planSvc, amount, periodHours)
		t.Logf("published 1-USDC/1h plan %s", h.PlanPDA)

		// Subscribe — atomic [subscribe+transfer] pulls PERIOD 1 here. Track the
		// SUBSCRIBER's exclusive balance for the final invariant (the merchant ATA is
		// shared across concurrent devnet tests, so its balance can't be asserted).
		subBefore, _ := rc.GetTokenBalanceForMint(ctx, sub.PublicKey(), usdc)
		row := devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, h, amount, periodHours)
		planPDA := solanago.MustPublicKeyFromBase58(h.PlanPDA)
		subPDA := solanago.MustPublicKeyFromBase58(row.SubscriptionPDA)
		t.Log("subscribed; period 1 pulled atomically inside the subscribe tx")

		// Cancel on-chain (expires_at_ts = end of the current period).
		cancelIx := subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
			Subscriber: sub.PublicKey(), PlanPDA: planPDA, SubscriptionPDA: subPDA, EventAuthority: eventAuth,
		})
		b64, e := buildTierChangeUnsignedTxBase64(ctx, rc, sub.PublicKey(), []solanago.Instruction{cancelIx})
		require.NoError(t, e)
		signAndSendBase64(ctx, t, rc, raw, sub, []string{b64})
		t.Log("cancel_subscription landed")

		// In-period: a second pull is rejected by the cap (period 1 already paid).
		_, immErr := crankSvc.Crank(ctx, dbtest.TestTenantID, row, amount)
		require.Error(t, immErr, "an in-period re-pull must be rejected")
		require.Equalf(t, 400, parseOnChainCode(immErr.Error()), "in-period re-pull should be cap-reached (400): %v", immErr)
		t.Log("in-period re-pull correctly rejected (Custom:400, cap)")

		// Wait for the period to roll over; a post-rollover pull on a CANCELLED sub
		// must yield Custom:508 (SubscriptionCancelled), NEVER a successful period-2.
		const pollEvery = 90 * time.Second
		deadline := time.Now().Add(80 * time.Minute)
		t.Logf("waiting for the period (1h) to roll over; the post-rollover pull must be REJECTED with 508...")
		var got508 bool
		for !got508 {
			_, perr := crankSvc.Crank(ctx, dbtest.TestTenantID, row, amount)
			if perr == nil {
				t.Fatalf("❌ CANCEL DID NOT WORK: a pull SUCCEEDED after cancel — the merchant charged a new period")
			}
			switch code := parseOnChainCode(perr.Error()); code {
			case 508:
				got508 = true
				t.Logf("✅ post-rollover pull rejected with Custom:508 (SubscriptionCancelled) — cancel is enforced on-chain")
			case 400:
				t.Logf("   still within the paid period (Custom:400); waiting %s", pollEvery)
			default:
				t.Logf("   pull rejected with Custom:%d (will keep polling): %v", code, perr)
			}
			if got508 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("period did not roll over to a 508 within the poll window")
			}
			select {
			case <-ctx.Done():
				t.Fatalf("context expired waiting for rollover: %v", ctx.Err())
			case <-time.After(pollEvery):
			}
		}

		// Final invariant: the subscriber paid for exactly ONE period (the atomic
		// subscribe), and nothing more — the cancelled period-2 pull was rejected.
		subFinal, _ := rc.GetTokenBalanceForMint(ctx, sub.PublicKey(), usdc)
		require.Equal(t, subBefore-amount, subFinal, "subscriber must have paid exactly ONE period (no period-2 charge after cancel)")

		// Surface the classifier's handling of 508 for follow-up (it may not map it yet).
		_, post := crankSvc.Crank(ctx, dbtest.TestTenantID, row, amount)
		if post != nil && strings.TrimSpace(post.Error()) != "" {
			t.Logf("NOTE classifier on the cancelled (508) pull: %+v", ClassifyCrankError(post))
		}
		t.Log("✅ CANCEL-AT-PERIOD-END VALIDATED ON DEVNET: one period charged (atomic subscribe), then cancel blocks all further pulls (508).")
	})
}
