//go:build devnet

// On-chain validation (real USDC) of the multi-subscription rules:
//   - a user CAN hold multiple subs to the SAME merchant for DIFFERENT plans
//     (products) — both pull independently;
//   - re-subscribing the SAME plan is rejected on-chain (the Subscription PDA
//     already exists), backstopping the app-layer duplicate-billing guard (#269);
//   - it also proves the Custom:519 fix: the 2nd distinct subscribe (plan B), which
//     used to trip PlanTermsMismatch from a stale client created_at, now lands on
//     the FIRST try (subscribeStrict does NOT retry 519).
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> go test -tags devnet -run TestDevnetMultiSubRules -v \
//	  -timeout 900s ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
)

func TestDevnetMultiSubRules(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
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

	funderUSDC, _ := rc.GetTokenBalanceForMint(ctx, funder.PublicKey(), usdc)
	if funderUSDC < 4_000_000 {
		t.Skipf("funder has %d USDC base units (<4) — top up first", funderUSDC)
	}
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)
	signer := solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := NewPlanService(submitter, "devnet")
	prepSvc := NewPrepareSubscribeService(submitter, rc, "devnet")
	crankSvc := NewCrankService(submitter)

	sub, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	t.Logf("user: %s", sub.PublicKey())
	fundSOL(ctx, t, rc, raw, merchant, sub.PublicKey(), 50_000_000)
	ensureATA(ctx, t, rc, raw, merchant, sub.PublicKey(), usdc)
	transferUSDC(ctx, t, rc, raw, funder, sub.PublicKey(), usdc, 3_000_000)

	publish := func() *PlanHandle {
		h, e := planSvc.PublishPlan(ctx, PublishPlanInput{
			TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
			TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720,
		})
		require.NoError(t, e)
		return h
	}

	// prepareSubscribe returns the current Prepare result for (sub, plan).
	prepareSubscribe := func(h *PlanHandle) *PrepareSubscribeResult {
		res, e := prepSvc.Prepare(ctx, PrepareSubscribeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(),
			PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720, PlanCreatedAt: h.CreatedAt,
		})
		require.NoError(t, e)
		return res
	}

	// subscribeStrict subscribes (init if needed) and sends the subscribe with NO
	// 519 retry — so a Custom:519 here FAILS the test, proving the created_at fix.
	subscribeStrict := func(h *PlanHandle, label string) solanago.PublicKey {
		res := prepareSubscribe(h)
		if res.Step == "init" {
			signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions)
			res = prepareSubscribe(h)
		}
		require.Equal(t, "subscribe", res.Step, "%s: expected subscribe step after init", label)
		signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions) // require.Nil on-chain error -> fails on 519
		planPDA := solanago.MustPublicKeyFromBase58(h.PlanPDA)
		subPDA, _, e := subscriptions.DeriveSubscriptionPDA(planPDA, sub.PublicKey())
		require.NoError(t, e)
		t.Logf("✅ subscribed %s (first try, no 519) -> %s", label, subPDA)
		return subPDA
	}

	hA := publish()
	hB := publish()

	// Subscribe A, then B (different plan, same merchant). B is the 2nd distinct
	// subscribe — the one that used to 519. Strict (no retry) => proves the fix.
	subscribeStrict(hA, "A")
	subscribeStrict(hB, "B")

	// DUPLICATE SAME PLAN: re-subscribing plan A must NOT succeed on-chain (the
	// Subscription PDA already exists). Either Prepare refuses or the tx reverts.
	dupRes, dupErr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
		TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(),
		PlanID: hA.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720, PlanCreatedAt: hA.CreatedAt,
	})
	if dupErr != nil {
		t.Logf("✅ duplicate same-plan subscribe refused at prepare: %v", dupErr)
	} else {
		e, ok := trySignSend(ctx, t, raw, rc, sub, dupRes.Transactions)
		require.False(t, ok, "re-subscribing the SAME plan A must be rejected on-chain (Subscription PDA already exists)")
		t.Logf("✅ duplicate same-plan subscribe rejected on-chain: %s", e)
	}

	// MULTI-PRODUCT INDEPENDENCE: both A and B are active under the same merchant;
	// each pulls 1 USDC independently.
	rowA := subscriptionRowFromHandle(hA, sub.PublicKey().String())
	rowB := subscriptionRowFromHandle(hB, sub.PublicKey().String())
	base, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	_, aErr := crankSvc.Crank(ctx, tenant.DefaultID, rowA, amount)
	require.NoError(t, aErr, "plan A pull should succeed")
	awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, base+amount, 90*time.Second, "plan A pull")
	_, bErr := crankSvc.Crank(ctx, tenant.DefaultID, rowB, amount)
	require.NoError(t, bErr, "plan B pull should succeed (independent of A, same merchant, different product)")
	awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, base+2*amount, 90*time.Second, "plan B pull")

	t.Log("✅ MULTI-SUB RULES VALIDATED: different products under one merchant pull independently; duplicate same-plan subscribe rejected; 519 fix confirmed (2nd subscribe landed first try).")
}
