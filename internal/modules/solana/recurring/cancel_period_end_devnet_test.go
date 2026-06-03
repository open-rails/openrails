//go:build devnet

// PROOF that cancel_subscription actually stops pulls ON-CHAIN at period end with
// REAL USDC. Subscribe to a 1-USDC/1-HOUR plan, pull period 1, then cancel. We
// assert two things the program guarantees:
//  1. WITHIN the current period after cancel, a second pull is rejected by the cap
//     (Custom:400) — the merchant cannot pull more than the one period.
//  2. AFTER the period rolls over (~1h), the pull is rejected with
//     SubscriptionCancelled (Custom:508) — NOT a successful period-2 charge. This
//     is the on-chain proof that cancel stops future pulls and we genuinely
//     cannot pull again.
//
// Final invariant: the merchant only ever received ONE period's USDC.
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> go test -tags devnet -run TestDevnetCancelStopsAtPeriodEnd -v \
//	  -timeout 2h ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"strings"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
)

func TestDevnetCancelStopsAtPeriodEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()

	endpoint := "https://devnet.helius-rpc.com/?api-key=" + devnetEnv(t, "HELIUS_API_KEY")
	rc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)
	merchant, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_PAYER_KEY"))
	require.NoError(t, err)
	funder, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_SUBSCRIBER_KEY"))
	require.NoError(t, err)
	usdc := solanago.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	const amount = uint64(1_000_000)
	const periodHours = uint64(1)

	funderUSDC, _ := rc.GetTokenBalanceForMint(ctx, funder.PublicKey(), usdc)
	if funderUSDC < 3_000_000 {
		t.Skipf("funder has %d USDC base units (<3) — top up first", funderUSDC)
	}
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)
	signer := solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := NewPlanService(submitter, "devnet")
	prepSvc := NewPrepareSubscribeService(submitter, signer, rc, "devnet")
	crankSvc := NewCrankService(submitter)
	eventAuth, _, err := subscriptions.DeriveEventAuthority()
	require.NoError(t, err)

	sub, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	t.Logf("user: %s", sub.PublicKey())
	fundSOL(ctx, t, rc, raw, merchant, sub.PublicKey(), 40_000_000)
	ensureATA(ctx, t, rc, raw, merchant, sub.PublicKey(), usdc)
	transferUSDC(ctx, t, rc, raw, funder, sub.PublicKey(), usdc, 2_500_000)

	h, err := planSvc.PublishPlan(ctx, PublishPlanInput{
		TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
		TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours,
	})
	require.NoError(t, err)
	t.Logf("published 1-USDC/1h plan %s", h.PlanPDA)

	// Subscribe (init + subscribe), retrying the subscribe step on a transient 519.
	res, perr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
		TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(),
		PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours, PlanCreatedAt: h.CreatedAt,
	})
	require.NoError(t, perr)
	if res.Step == "init" {
		signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions)
	}
	for attempt := 0; attempt < 10; attempt++ {
		res, perr = prepSvc.Prepare(ctx, PrepareSubscribeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(),
			PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours, PlanCreatedAt: h.CreatedAt,
		})
		require.NoError(t, perr)
		if res.Step != "subscribe" {
			signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions)
			continue
		}
		e, ok := trySignSend(ctx, t, raw, rc, sub, res.Transactions)
		if ok {
			break
		}
		require.Truef(t, strings.Contains(e, "519"), "subscribe failed (non-519): %s", e)
		time.Sleep(2 * time.Second)
	}
	t.Log("subscribed")

	row := subscriptionRowFromHandle(h, sub.PublicKey().String())
	planPDA := solanago.MustPublicKeyFromBase58(h.PlanPDA)
	subPDA := solanago.MustPublicKeyFromBase58(row.SubscriptionPDA)

	// 1) Pull period 1. Track the SUBSCRIBER's balance (fresh, EXCLUSIVE wallet) for
	// the final invariant — the merchant ATA is shared across concurrent devnet
	// tests, so unrelated deposits would pollute a merchant-balance assertion.
	subBefore, _ := rc.GetTokenBalanceForMint(ctx, sub.PublicKey(), usdc)
	base, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	_, cerr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
	require.NoError(t, cerr, "period-1 pull should succeed")
	awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, base+amount, 90*time.Second, "period-1 pull")
	t.Logf("period 1 pulled: merchant %d -> %d", base, base+amount)

	// 2) Cancel on-chain.
	cancelIx := subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
		Subscriber: sub.PublicKey(), PlanPDA: planPDA, SubscriptionPDA: subPDA, EventAuthority: eventAuth,
	})
	b64, e := buildTierChangeUnsignedTxBase64(ctx, rc, sub.PublicKey(), []solanago.Instruction{cancelIx})
	require.NoError(t, e)
	signAndSendBase64(ctx, t, rc, raw, sub, []string{b64})
	t.Log("cancel_subscription landed (expires_at_ts = end of current period)")

	// 3) Within the period, a second pull is rejected by the cap (cannot pull more).
	_, immErr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
	require.Error(t, immErr, "a second pull in the same period must be rejected")
	require.Equalf(t, 400, parseOnChainCode(immErr.Error()), "in-period re-pull should be cap-reached (400): %v", immErr)
	t.Log("in-period re-pull correctly rejected (Custom:400, cap) — cannot pull more this period")

	// 4) Wait for the period to roll over, polling. Before the boundary -> 400. After
	// it, a cancelled sub must yield 508 (SubscriptionCancelled), NOT a successful
	// period-2 pull. A success here would mean cancel did NOT work on-chain.
	const pollEvery = 90 * time.Second
	deadline := time.Now().Add(80 * time.Minute)
	t.Logf("waiting for the period (1h) to roll over; a post-rollover pull must be REJECTED with 508 (proving cancel stops it)...")
	var got508 bool
	for {
		_, perr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
		if perr == nil {
			t.Fatalf("❌ CANCEL DID NOT WORK: a pull SUCCEEDED after cancel — the merchant charged a new period")
		}
		code := parseOnChainCode(perr.Error())
		switch code {
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
			t.Fatalf("period did not roll over to a 508 within the poll window; last code=%d", code)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context expired waiting for rollover: %v", ctx.Err())
		case <-time.After(pollEvery):
		}
	}

	// 5) Final invariant: the SUBSCRIBER paid for exactly ONE period and nothing
	// more — the cancelled period-2 pull was rejected (508 above). Asserted on the
	// subscriber's exclusive wallet so concurrent tests touching the shared merchant
	// ATA cannot cause a false failure.
	subFinal, _ := rc.GetTokenBalanceForMint(ctx, sub.PublicKey(), usdc)
	require.Equal(t, subBefore-amount, subFinal, "subscriber must have paid exactly ONE period after cancel (no period-2 charge)")

	// Surface the classifier's handling of 508 for follow-up (it may not map it yet).
	_, post := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
	if post != nil {
		t.Logf("NOTE classifier on the cancelled (508) pull: %+v", ClassifyCrankError(post))
	}
	t.Log("✅ CANCEL-AT-PERIOD-END VALIDATED ON DEVNET: one period charged, then cancel blocks all further pulls (508).")
}
