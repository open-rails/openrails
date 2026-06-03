//go:build devnet

// REAL on-chain SUSTAINED multi-cycle rebill against devnet with REAL USDC — the
// overnight sibling of service_devnet_multirebill_test.go. Where that test proves
// ONE period rollover (two pulls), this one runs MANY consecutive 1-hour cycles
// back-to-back for ~8 hours, proving the recurring engine keeps billing correctly
// cycle after cycle: each cycle pulls exactly once, an immediate re-pull is
// rejected as already-paid (the cap gates double-charges), and the next pull only
// succeeds after the on-chain period genuinely rolls over. A run that completes N
// cycles is N independent proofs of recurrence with no replay and no drift.
//
// WHY IT IS SLOW: the minimum on-chain period is one hour, so each cycle sits
// idle ~1h waiting for the boundary. SUSTAINED_REBILL_CYCLES cycles ≈ that many
// hours. Manual / scheduled only — never in the normal test run.
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> SUSTAINED_REBILL_CYCLES=8 \
//	go test -tags devnet -run SustainedRebill -v -timeout 9h ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/doujins-org/solana-go/rpc"
	"github.com/open-rails/openrails/internal/billing/declinecode"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/stretchr/testify/require"
)

func TestDevnetSustainedRebill(t *testing.T) {
	cycles := 8
	if v := os.Getenv("SUSTAINED_REBILL_CYCLES"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err, "SUSTAINED_REBILL_CYCLES must be an integer")
		require.Greater(t, n, 0, "SUSTAINED_REBILL_CYCLES must be > 0")
		cycles = n
	}
	// Each cycle is ~1h; budget an hour of slack per cycle for boundary skew + RPC
	// retries so a slow rollover never times out the whole run.
	ctxTimeout := time.Duration(cycles+1)*time.Hour + 30*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()
	t.Logf("SUSTAINED rebill: %d cycles, ctx timeout %s", cycles, ctxTimeout)

	endpoint := "https://devnet.helius-rpc.com/?api-key=" + devnetEnv(t, "HELIUS_API_KEY")
	rc := solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: endpoint, Network: "devnet"})
	raw := rpc.New(endpoint)

	merchant, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_PAYER_KEY"))
	require.NoError(t, err)
	funder, err := solanago.PrivateKeyFromBase58(devnetEnv(t, "SOLANA_DEVNET_SUBSCRIBER_KEY"))
	require.NoError(t, err)

	usdc := solanago.MustPublicKeyFromBase58("4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU")
	const amount = uint64(1_000_000) // 1 USDC / cycle
	// Need one USDC per cycle + a little buffer; require the funder can cover it.
	needBaseUnits := uint64(cycles+1) * amount
	funderUSDC, err := rc.GetTokenBalanceForMint(ctx, funder.PublicKey(), usdc)
	if err != nil || funderUSDC < needBaseUnits {
		t.Skipf("funder %s has %d USDC base units (<%d needed for %d cycles) — fund via https://faucet.circle.com then re-run",
			funder.PublicKey(), funderUSDC, needBaseUnits, cycles)
	}
	t.Logf("funder USDC balance: %d base units (need %d for %d cycles)", funderUSDC, needBaseUnits, cycles)

	// Fresh subscriber each run; fund enough USDC for every cycle's pull up front.
	subscriber, err := solanago.NewRandomPrivateKey()
	require.NoError(t, err)
	t.Logf("fresh subscriber: %s", subscriber.PublicKey())
	fundSOL(ctx, t, rc, raw, merchant, subscriber.PublicKey(), 40_000_000)
	ensureATA(ctx, t, rc, raw, merchant, subscriber.PublicKey(), usdc)
	transferUSDC(ctx, t, rc, raw, funder, subscriber.PublicKey(), usdc, needBaseUnits)
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc) // merchant receiving ATA
	t.Logf("funded fresh subscriber with %d USDC base units + SOL gas", needBaseUnits)

	signer := solanaint.NewKeypairSigner(memSecretGetter{key: merchant.String()}, 0)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := NewPlanService(submitter, "devnet")
	prepSvc := NewPrepareSubscribeService(submitter, signer, rc, "devnet")
	crankSvc := NewCrankService(submitter)

	// 1-USDC / 1-HOUR plan — 1 hour is the minimum period, so the cap resets ~1h
	// after each pull and the next cycle's pull proves the boundary rolled over.
	const periodHours = uint64(1)
	handle, err := planSvc.PublishPlan(ctx, PublishPlanInput{
		TenantID: tenant.DefaultID, PlanID: uint64(time.Now().UnixNano()),
		TokenSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours,
	})
	require.NoError(t, err, "PublishPlan should create the 1-hour plan on-chain")
	t.Logf("PublishPlan OK (period_hours=1): plan_pda=%s sig=%s", handle.PlanPDA, handle.Signature)

	for step := 0; step < 2; step++ {
		res, perr := prepSvc.Prepare(ctx, PrepareSubscribeInput{
			TenantID: tenant.DefaultID, SubscriberWallet: subscriber.PublicKey().String(),
			PlanID: handle.PlanID, MintSymbol: "USDC", AmountBaseUnits: amount, PeriodHours: periodHours, PlanCreatedAt: handle.CreatedAt,
		})
		require.NoError(t, perr)
		signAndSendBase64(ctx, t, rc, raw, subscriber, res.Transactions)
		if res.Step == "subscribe" {
			break
		}
	}
	t.Log("subscribed; beginning sustained rebill loop")

	row := subscriptionRowFromHandle(handle, subscriber.PublicKey().String())
	const pollEvery = 90 * time.Second
	start := time.Now()
	var lastSig string

	for c := 1; c <= cycles; c++ {
		before, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)

		// Poll the crank until THIS cycle's pull lands. Cycle 1 succeeds immediately;
		// later cycles return AlreadyPaid (Custom:400) until the period rolls over.
		var sig string
		for {
			s, perr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
			if perr == nil {
				sig = s
				break
			}
			cf := ClassifyCrankError(perr)
			switch cf.Category {
			case declinecode.AlreadyPaid:
				t.Logf("   cycle %d: still in current period (already-paid); waiting %s", c, pollEvery)
			case declinecode.Operational:
				t.Logf("   cycle %d: operational blip (will retry): %v", c, perr)
			default:
				t.Fatalf("cycle %d: non-retryable decline (%s): %v", c, cf.Category, perr)
			}
			select {
			case <-ctx.Done():
				t.Fatalf("cycle %d: context expired waiting for period rollover after %s: %v", c, time.Since(start), ctx.Err())
			case <-time.After(pollEvery):
			}
		}

		// Poll the merchant balance: a load-balanced devnet RPC node can lag the
		// just-confirmed pull by a few seconds, so wait until it reflects the credit
		// (the crank already confirmed on-chain — this only absorbs read lag).
		awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, before+amount, 90*time.Second,
			"cycle %d: merchant should receive exactly 1 USDC", c)
		require.NotEqual(t, lastSig, sig, "cycle %d: rebill must be a DISTINCT on-chain tx (not a replay)", c)
		lastSig = sig
		t.Logf("✅ cycle %d/%d OK: pulled 1 USDC; merchant %d->%d; sig=%s; elapsed=%s",
			c, cycles, before, before+amount, sig, time.Since(start).Round(time.Second))

		// The cap must now block a same-period double-charge.
		_, earlyErr := crankSvc.Crank(ctx, tenant.DefaultID, row, amount)
		require.Error(t, earlyErr, "cycle %d: immediate re-pull must be rejected (period not rolled over)", c)
		require.Equal(t, declinecode.AlreadyPaid, ClassifyCrankError(earlyErr).Category,
			"cycle %d: immediate re-pull should classify as AlreadyPaid (Custom:400), got: %v", c, earlyErr)
	}

	// Cancel on-chain after the last cycle.
	cancelSvc := NewPrepareCancelService(fixedSubReader{row: row}, rc)
	cres, xerr := cancelSvc.Prepare(ctx, row.SubscriptionID)
	require.NoError(t, xerr)
	signAndSendBase64(ctx, t, rc, raw, subscriber, []string{cres.Transaction})
	t.Logf("Cancel OK: cancel_subscription submitted for %s", cres.SubscriptionPDA)

	t.Logf("✅ SUSTAINED REBILL VALIDATED ON DEVNET: %d consecutive 1-hour cycles, each a distinct on-chain pull with the cap gating double-charges; total elapsed %s",
		cycles, time.Since(start).Round(time.Second))
}

// awaitTokenCredit polls the (owner, mint) token balance until it equals `want`,
// failing only if it never reaches it within `timeout`. Devnet RPC is
// load-balanced, so a getTokenAccountBalance issued right after a tx confirms can
// hit a node that still lags the pull by a few seconds; the on-chain pull already
// succeeded (the crank confirmed) — this just absorbs that read-after-confirm lag
// so a balance assertion doesn't flake.
func awaitTokenCredit(ctx context.Context, t *testing.T, rc *solanaint.RPCClient, owner, mint solanago.PublicKey, want uint64, timeout time.Duration, msgf string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last uint64
	for {
		last, _ = rc.GetTokenBalanceForMint(ctx, owner, mint)
		if last == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: balance for %s did not reach %d within %s (last read %d)",
				fmt.Sprintf(msgf, args...), owner, want, timeout, last)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context expired waiting for token credit: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
