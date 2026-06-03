//go:build devnet

// DECISIVE devnet probe (real USDC): can a user cancel ONE subscription on-chain
// WITHOUT affecting their OTHER subscriptions on the same token? The SPL token
// delegate is shared per (user, mint) across ALL merchants, so token.Revoke is
// all-or-nothing. The subscriptions program's own revoke_delegation (3) /
// cancel_subscription (12) target a SINGLE subscription account — this probe finds
// out which (if any) actually STOPS that one subscription's pulls on-chain while
// leaving a second subscription pulling normally.
//
// Setup: one fresh user subscribes to TWO plans (A and B) under the same
// SubscriptionAuthority. We "cancel" A on-chain, then prove: a crank on A FAILS
// and a crank on B SUCCEEDS. That is the definition of independent per-sub cancel.
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> go test -tags devnet -run TestDevnetPerSubIndependentCancel -v \
//	  -timeout 900s ./internal/modules/solana/recurring/...
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

func TestDevnetPerSubIndependentCancel(t *testing.T) {
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
	if funderUSDC < 5_000_000 {
		t.Skipf("funder has %d USDC base units (<5) — top up first", funderUSDC)
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
	fundSOL(ctx, t, rc, raw, merchant, sub.PublicKey(), 60_000_000)
	ensureATA(ctx, t, rc, raw, merchant, sub.PublicKey(), usdc)
	transferUSDC(ctx, t, rc, raw, funder, sub.PublicKey(), usdc, 5_000_000)

	publish := func() *PlanHandle {
		h, e := planSvc.PublishPlan(ctx, PublishPlanInput{
			TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
			TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720,
		})
		require.NoError(t, e)
		return h
	}

	// subscribeWithRetry runs init (once) then the subscribe step, retrying the
	// subscribe on the intermittent Custom:519 read-lag (re-prepares so it re-reads
	// the authority initId from a hopefully-caught-up node).
	subscribeWithRetry := func(t *testing.T, h *PlanHandle, label string) solanago.PublicKey {
		t.Helper()
		// init step (must land)
		res, e := prepSvc.Prepare(ctx, PrepareSubscribeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(),
			PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720, PlanCreatedAt: h.CreatedAt,
		})
		require.NoError(t, e)
		if res.Step == "init" {
			signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions)
		}
		// subscribe step (retry on 519)
		for attempt := 0; attempt < 10; attempt++ {
			res, e = prepSvc.Prepare(ctx, PrepareSubscribeInput{
				TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(),
				PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720, PlanCreatedAt: h.CreatedAt,
			})
			require.NoError(t, e)
			if res.Step != "subscribe" {
				signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions)
				continue
			}
			errStr, ok := trySignSend(ctx, t, raw, rc, sub, res.Transactions)
			if ok {
				break
			}
			if strings.Contains(errStr, "519") {
				t.Logf("subscribe %s: Custom:519 (read-lag), retry %d", label, attempt+1)
				time.Sleep(2 * time.Second)
				continue
			}
			t.Fatalf("subscribe %s failed on-chain: %s", label, errStr)
		}
		planPDA := solanago.MustPublicKeyFromBase58(h.PlanPDA)
		subPDA, _, e := subscriptions.DeriveSubscriptionPDA(planPDA, sub.PublicKey())
		require.NoError(t, e)
		t.Logf("subscribed %s -> %s", label, subPDA)
		return subPDA
	}

	sendIx := func(t *testing.T, ixs ...solanago.Instruction) (string, bool) {
		t.Helper()
		b64, e := buildTierChangeUnsignedTxBase64(ctx, rc, sub.PublicKey(), ixs)
		require.NoError(t, e)
		return trySignSend(ctx, t, raw, rc, sub, []string{b64})
	}

	hA := publish()
	hB := publish()
	subA := subscribeWithRetry(t, hA, "A")
	_ = subscribeWithRetry(t, hB, "B") // B's PDA derived via its row below
	rowA := subscriptionRowFromHandle(hA, sub.PublicKey().String())
	rowB := subscriptionRowFromHandle(hB, sub.PublicKey().String())
	planA := solanago.MustPublicKeyFromBase58(hA.PlanPDA)

	// Cancel A on-chain. Try the program's targeted instructions, strongest first:
	// (1) cancel_subscription(A) then revoke_delegation(A) (close A's account), so A
	// can no longer be pulled. We do NOT touch the shared SPL delegate (that would
	// kill B too). Report exactly what lands.
	cancelA := subscriptions.BuildCancelSubscription(subscriptions.CancelOrResumeParams{
		Subscriber: sub.PublicKey(), PlanPDA: planA, SubscriptionPDA: subA, EventAuthority: eventAuth,
	})
	if e, ok := sendIx(t, cancelA); !ok {
		t.Logf("cancel_subscription(A) on-chain err: %s", e)
	} else {
		t.Log("cancel_subscription(A) landed")
	}
	revokeA := subscriptions.BuildRevokeDelegation(sub.PublicKey(), subA, planA)
	revokeErr, revokeOK := sendIx(t, revokeA)
	if revokeOK {
		t.Log("revoke_delegation(A) landed — A's subscription/delegation account closed")
	} else {
		t.Logf("revoke_delegation(A) on-chain err: %s (may require the subscription to be expired/cancelled first)", revokeErr)
	}

	// Now the decisive checks. Crank A: must FAIL (A is cancelled/closed on-chain).
	// Crank B: must SUCCEED (independent), proving per-sub cancel does NOT affect siblings.
	beforeB, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	_, errA := crankSvc.Crank(ctx, tenant.DefaultID, rowA, amount)
	sigB, errB := crankSvc.Crank(ctx, tenant.DefaultID, rowB, amount)

	t.Logf("RESULT crank A (cancelled): err=%v", errA)
	t.Logf("RESULT crank B (active):    err=%v sig=%s", errB, sigB)

	require.Error(t, errA, "crank on the CANCELLED subscription A must fail on-chain")
	cfA := ClassifyCrankError(errA)
	t.Logf("   A classified: Custom:%d -> %s", cfA.OnChainCode, cfA.Category)

	require.NoError(t, errB, "crank on the still-active subscription B must SUCCEED (independent of A's cancel)")
	awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, beforeB+amount, 90*time.Second,
		"B should still pull 1 USDC after A was cancelled")

	t.Log("✅ INDEPENDENT PER-SUB CANCEL VALIDATED: cancelling subscription A on-chain stopped A's pulls while subscription B kept pulling — independent per-sub cancellation works.")
}
