//go:build devnet

// REAL on-chain MULTI-REBILL validation against devnet with REAL USDC (#275-B).
// This is the long-running sibling of the fast, network-free state-machine test
// in internal/river/jobs_solana_crank_statemachine_test.go. Where that test
// asserts the dunning/rebill STATE TRANSITIONS over a fake clock, THIS test
// proves a genuine recurring rebill on actual chain: it publishes a plan with
// period_hours=1, subscribes, pulls once, WAITS for the on-chain period to roll
// over (~1 hour — the program's amount_pulled_in_period cap resets only when the
// period boundary passes), then pulls a SECOND time. A second successful pull
// can ONLY happen if the period genuinely rolled over and the cap reset — that
// is the proof of a real recurring rebill (not a replay of the first pull).
//
// WHY IT IS SLOW / WHY IT IS NOT RUN IN CI: the minimum on-chain period is one
// hour, so the second pull cannot succeed until ~1h after the first. Before the
// boundary, transfer_subscription returns Custom:400 (cap reached / already
// paid); we poll past that until it succeeds, capped well past one period. Budget
// a multi-hour timeout. It also spends scarce devnet USDC. Run it manually or on
// a schedule — NOT in the normal test run.
//
// Prereqs: same as service_devnet_test.go. Fund the funder wallet's USDC ATA via
// https://faucet.circle.com (Solana devnet, USDC); the test funds a fresh
// subscriber's SOL gas + USDC from it each run.
//
// Run (note the multi-HOUR timeout — this WILL sit idle ~1h between pulls):
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> go test -tags devnet -run MultiRebill -v \
//	  -timeout 3h ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"strings"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/open-rails/openrails/internal/billing/declinecode"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
)

func TestDevnetMultiRebillHourly(t *testing.T) {
	// One real period is 1 hour; the second pull cannot land until that boundary
	// passes, and we poll past it. Budget 3 hours so a slow boundary + RPC retries
	// never time the test out mid-rebill.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Hour)
	defer cancel()

	endpoint := "https://devnet.helius-rpc.com/?api-key=" + devnetEnv(t, "HELIUS_API_KEY")
	rc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)

	merchant, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_PAYER_KEY"))
	require.NoError(t, err)
	// Funder holds the faucet USDC and funds a FRESH subscriber each run so the
	// SubscriptionAuthority state never accumulates (avoids the repeat-subscribe
	// Custom:519), mirroring service_devnet_test.go.
	funder, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_SUBSCRIBER_KEY"))
	require.NoError(t, err)

	// USDC devnet mint (must match config DefaultDevnetTokens).
	usdc := solanago.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	// Two real pulls of 1 USDC each + headroom; require >3 USDC in the funder.
	funderUSDC, err := rc.GetTokenBalanceForMint(ctx, funder.PublicKey(), usdc)
	if err != nil || funderUSDC < 3_000_000 {
		t.Skipf("funder %s has %d USDC base units (<3 USDC) — fund via https://faucet.circle.com (Solana devnet, USDC) then re-run",
			funder.PublicKey(), funderUSDC)
	}
	t.Logf("funder USDC balance: %d base units", funderUSDC)

	// Fresh subscriber each run, funded with enough USDC for TWO pulls.
	subscriber, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	t.Logf("fresh subscriber: %s", subscriber.PublicKey())
	fundSOL(ctx, t, rc, raw, merchant, subscriber.PublicKey(), 30_000_000)
	ensureATA(ctx, t, rc, raw, merchant, subscriber.PublicKey(), usdc)
	transferUSDC(ctx, t, rc, raw, funder, subscriber.PublicKey(), usdc, 2_500_000) // 2.5 USDC: covers 2x 1-USDC pulls
	t.Log("funded fresh subscriber with 2.5 USDC + SOL gas (enough for two rebills)")

	// Merchant receiving ATA must exist before any pull (tenant-setup step).
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc)

	// Production Submitter + services backed by the merchant (cranker) key.
	signer := solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := NewPlanService(submitter, "devnet")
	prepSvc := NewPrepareSubscribeService(submitter, rc, "devnet")
	crankSvc := NewCrankService(submitter)

	// 1) Publish a 1-USDC / 1-HOUR plan on-chain. period_hours=1 is the crux: it
	// is the minimum recurring period, so the cap resets ~1h after the first pull
	// and the SECOND pull can prove the recurrence.
	const amount = uint64(1_000_000) // 1 USDC
	const periodHours = uint64(1)
	handle, err := planSvc.PublishPlan(ctx, PublishPlanInput{
		TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
		TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours,
	})
	require.NoError(t, err, "PublishPlan should create the 1-hour plan on-chain")
	t.Logf("1) PublishPlan OK (period_hours=1): plan_pda=%s sig=%s", handle.PlanPDA, handle.Signature)

	// 2) Prepare + sign+send the subscribe transaction(s) as the wallet.
	for step := 0; step < 2; step++ {
		res, perr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: subscriber.PublicKey().String(),
			PlanID: handle.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours, PlanCreatedAt: handle.CreatedAt,
		})
		require.NoError(t, perr)
		t.Logf("2) Prepare step=%s authorityExists=%v txns=%d", res.Step, res.AuthorityExists, len(res.Transactions))
		signAndSendBase64(ctx, t, rc, raw, subscriber, res.Transactions)
		if res.Step == "subscribe" {
			break
		}
	}

	row := subscriptionRowFromHandle(handle, subscriber.PublicKey().String())

	// 3) FIRST pull: the merchant pulls 1 USDC (real CrankService path).
	before1, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	sig1, cerr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
	require.NoError(t, cerr, "first Crank should pull 1 USDC")
	after1, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	require.Equal(t, before1+amount, after1, "merchant should receive 1 USDC from the first crank")
	t.Logf("3) First Crank OK: pulled 1 USDC; merchant %d->%d; sig=%s", before1, after1, sig1)

	// 3b) Immediate re-pull MUST be rejected as already-paid (Custom:400): the cap
	// is reached and the period has not rolled over. This proves the cap actually
	// gates a double-charge within the same period (the safety property the
	// fast test models as AlreadyPaid).
	_, earlyErr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
	require.Error(t, earlyErr, "an immediate second pull must be rejected (period not rolled over)")
	require.Equal(t, declinecode.AlreadyPaid, ClassifyCrankError(earlyErr).Category,
		"immediate re-pull should classify as AlreadyPaid (Custom:400), got: %v", earlyErr)
	t.Logf("3b) Immediate re-pull correctly rejected as already-paid: %v", earlyErr)

	// 4) SECOND pull after the period rolls over (~1h). Poll: a too-early pull
	// returns Custom:400 (AlreadyPaid) — retry until it SUCCEEDS, which can only
	// happen once the on-chain period boundary passes and the cap resets. That
	// success is the proof of a genuine recurring rebill (NOT a replay).
	before2, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	var sig2 string
	// Period is 1h; poll for up to ~80 minutes to absorb clock skew at the
	// boundary. 90s between attempts keeps RPC load light over the long wait.
	const pollEvery = 90 * time.Second
	deadline := time.Now().Add(80 * time.Minute)
	t.Logf("4) waiting for the on-chain period (1h) to roll over so the second pull succeeds (polling every %s)...", pollEvery)
	for {
		s, perr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
		if perr == nil {
			sig2 = s
			break // period rolled over: the rebill went through.
		}
		cf := ClassifyCrankError(perr)
		switch cf.Category {
		case declinecode.AlreadyPaid:
			// Still within the paid period — expected; keep polling.
			if time.Now().After(deadline) {
				t.Fatalf("period did not roll over within the poll window; last error: %v", perr)
			}
			t.Logf("   still in current period (already-paid); sleeping %s", pollEvery)
		case declinecode.Operational:
			// Transient RPC/network blip — retry, but still respect the deadline.
			if time.Now().After(deadline) {
				t.Fatalf("operational failures persisted past the poll window; last error: %v", perr)
			}
			t.Logf("   operational pull failure (will retry): %v", perr)
		default:
			// A recoverable/terminal decline here is a real failure of the test.
			t.Fatalf("second pull failed with a non-retryable decline (%s): %v", cf.Category, perr)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context expired while waiting for the period to roll over: %v", ctx.Err())
		case <-time.After(pollEvery):
		}
	}
	after2, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	require.Equal(t, before2+amount, after2, "merchant should receive a SECOND 1 USDC after the period rolled over")
	require.NotEqual(t, sig1, sig2, "the second rebill must be a distinct on-chain transaction (not a replay)")
	t.Logf("4) Second Crank OK (REBILL PROVEN): pulled another 1 USDC; merchant %d->%d; sig=%s", before2, after2, sig2)

	// 5) Cancel: build + sign+send the on-chain cancel_subscription tx.
	cancelSvc := NewPrepareCancelService(fixedSubReader{row: row}, rc)
	cres, xerr := cancelSvc.Prepare(ctx, row.SubscriptionID)
	require.NoError(t, xerr)
	signAndSendBase64(ctx, t, rc, raw, subscriber, []string{cres.Transaction})
	t.Logf("5) Cancel OK: cancel_subscription submitted for %s", cres.SubscriptionPDA)

	// Sanity: the second tx string must look like a real signature.
	require.True(t, strings.TrimSpace(sig2) != "", "second pull signature should be non-empty")

	t.Log("✅ MULTI-REBILL VALIDATED ON DEVNET WITH REAL USDC: publish(1h) -> subscribe -> pull -> wait period -> pull again (cap reset) -> cancel")
}
