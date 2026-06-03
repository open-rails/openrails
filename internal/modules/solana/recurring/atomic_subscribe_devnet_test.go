//go:build devnet

// Devnet validation (real USDC) of the ATOMIC subscribe (#286): the subscribe step
// now returns ONE co-signed [subscribe + transfer(first period)] tx (cranker
// pre-signs the transfer slot; the wallet completes the fee-payer slot), so a new
// subscription is created AND the first period is pulled in a single transaction.
// First-timer flow: [init] (user-only) then the atomic [subscribe+transfer].
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> go test -tags devnet -run TestDevnetAtomicSubscribe -v \
//	  -timeout 600s ./internal/modules/solana/recurring/...
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

func TestDevnetAtomicSubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	endpoint := "https://devnet.helius-rpc.com/?api-key=" + devnetEnv(t, "HELIUS_API_KEY")
	rc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)
	merchant, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_PAYER_KEY"))
	require.NoError(t, err)
	funder, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_SUBSCRIBER_KEY"))
	require.NoError(t, err)
	usdc := solanago.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	const amount = uint64(1_000_000) // 1 USDC first period

	funderUSDC, _ := rc.GetTokenBalanceForMint(ctx, funder.PublicKey(), usdc)
	if funderUSDC < 2_000_000 {
		t.Skipf("funder has %d USDC base units (<2) — top up first", funderUSDC)
	}
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)

	signer := solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := NewPlanService(submitter, "devnet")
	prepSvc := NewPrepareSubscribeService(submitter, signer, rc, "devnet")

	sub, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	t.Logf("fresh subscriber: %s", sub.PublicKey())
	fundSOL(ctx, t, rc, raw, merchant, sub.PublicKey(), 40_000_000)
	ensureATA(ctx, t, rc, raw, merchant, sub.PublicKey(), usdc)
	transferUSDC(ctx, t, rc, raw, funder, sub.PublicKey(), usdc, 2_000_000) // 2 USDC

	h, err := planSvc.PublishPlan(ctx, PublishPlanInput{
		TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
		TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720,
	})
	require.NoError(t, err)
	t.Logf("published 1-USDC plan %s", h.PlanPDA)

	prepare := func() *PrepareSubscribeResult {
		res, perr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: sub.PublicKey().String(),
			PlanID: h.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: 720, PlanCreatedAt: h.CreatedAt,
		})
		require.NoError(t, perr)
		return res
	}

	// Step 1: init (first-timer authority). User-only signed.
	res := prepare()
	require.Equal(t, "init", res.Step, "fresh wallet should need init first")
	signAndSendBase64(ctx, t, rc, raw, sub, res.Transactions)
	t.Log("init_subscription_authority landed")

	// Step 2: the ATOMIC co-signed [subscribe + transfer]. The cranker pre-signed the
	// transfer slot; the wallet completes the fee-payer slot WITHOUT dropping that
	// signature. This single tx must create the subscription AND pull the first period.
	res = prepare()
	require.Equal(t, "subscribe", res.Step, "authority now exists -> subscribe step")
	require.Len(t, res.Transactions, 1, "atomic subscribe is a single tx")

	before, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	completePartialAndSend(ctx, t, rc, raw, sub, res.Transactions[0])
	awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, before+amount, 90*time.Second,
		"atomic subscribe must pull the first period (1 USDC) in the SAME tx")

	// The subscription PDA exists on-chain (subscribe ran in the same tx).
	planPDA := solanago.MustPublicKeyFromBase58(h.PlanPDA)
	subPDA, _, err := subscriptions.DeriveSubscriptionPDA(planPDA, sub.PublicKey())
	require.NoError(t, err)
	data, err := rc.GetAccountData(ctx, subPDA)
	require.NoError(t, err)
	require.NotEmpty(t, data, "subscription PDA must exist after the atomic subscribe")

	t.Logf("✅ ATOMIC SUBSCRIBE VALIDATED ON DEVNET: one co-signed tx created subscription %s AND pulled 1 USDC (merchant %d->%d)",
		subPDA, before, before+amount)
}
