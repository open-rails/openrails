//go:build devnet

// REAL on-chain MULTI-CYCLE rebill against devnet with REAL USDC (#275-B) — the
// long-running sibling of the fast, network-free state-machine test in
// internal/river/jobs_solana_crank_statemachine_test.go. Where that test asserts
// the dunning/rebill STATE TRANSITIONS over a fake clock, THIS test proves genuine
// recurrence on actual chain across consecutive 1-hour periods.
//
// The proof: a 1-USDC / 1-HOUR plan. The ATOMIC subscribe pulls PERIOD 1 in the
// subscribe tx itself (#286). Each later cycle then pulls EXACTLY once — and can
// only succeed AFTER the on-chain period genuinely rolls over (~1h), because the
// program's amount_pulled_in_period cap resets only at the boundary. A too-early
// re-pull returns Custom:400 (AlreadyPaid); we poll past it. A second successful,
// DISTINCT pull can happen only if the period really rolled over and the cap
// reset — that is the proof of a real recurring rebill, not a replay. N completed
// cycles = N independent proofs of recurrence with no replay and no drift.
//
// WHY IT IS SLOW / NOT RUN IN CI: the minimum on-chain period is one hour, so each
// cycle sits idle ~1h waiting for the boundary. It also spends scarce devnet USDC.
// Run it manually or on a schedule — NEVER in the normal test run. Default 2
// cycles (one real rollover = the minimum recurrence proof, ~1h); set
// SUSTAINED_REBILL_CYCLES higher for an overnight soak.
//
//	SOLANA_DEVNET_PAYER_KEY=<funded> SOLANA_DEVNET_SUBSCRIBER_KEY=<usdc-funded> \
//	HELIUS_API_KEY=<key> SUSTAINED_REBILL_CYCLES=8 \
//	go test -tags devnet -run TestDevnetSustainedRebill -v -timeout 9h ./internal/modules/solana/recurring/...
package recurring

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/internal/billing/declinecode"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestDevnetSustainedRebill(t *testing.T) {
	// Cycle 1 is the period pulled by the atomic subscribe; cycles 2..N are
	// separate cranks, each needing a real ~1h rollover. Default 2 (= one rollover
	// = the minimum recurrence proof). Override for an overnight soak.
	cycles := 2
	if v := os.Getenv("SUSTAINED_REBILL_CYCLES"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err, "SUSTAINED_REBILL_CYCLES must be an integer")
		require.Greater(t, n, 1, "SUSTAINED_REBILL_CYCLES must be > 1 (need at least one rollover)")
		cycles = n
	}
	// Each rollover is ~1h; budget an hour of slack per cycle for boundary skew +
	// RPC retries so a slow rollover never times out the whole run.
	ctxTimeout := time.Duration(cycles+1)*time.Hour + 30*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()
	t.Logf("SUSTAINED rebill: %d cycles (cycle 1 = atomic subscribe; %d separate rollovers), ctx timeout %s", cycles, cycles-1, ctxTimeout)

	rc, raw := devnetClients(t)
	merchant, funder := devnetKeys(t)
	usdc := solanago.MustPublicKeyFromBase58(devnetUSDCMintStr)
	const amount = uint64(1_000_000) // 1 USDC / cycle
	// One USDC per cycle + a little buffer.
	needBaseUnits := uint64(cycles+1) * amount
	requireFunderUSDC(ctx, t, rc, funder, usdc, needBaseUnits)
	ensureATA(ctx, t, rc, raw, merchant, merchant.PublicKey(), usdc) // merchant receiving ATA

	sub := freshFundedSubscriber(ctx, t, rc, raw, merchant, funder, usdc, 40_000_000, needBaseUnits)

	signer := newMerchantSigner(merchant)
	submitter := NewSignerSubmitter(signer, rc)
	planSvc := newDevnetPlanService(submitter)
	prepSvc := newDevnetPrepareSubscribeService(submitter, signer, rc)
	crankSvc := NewCrankService(submitter)

	const periodHours = uint64(1)
	h := publishUSDCPlan(ctx, t, planSvc, amount, periodHours)
	t.Logf("published 1-USDC/1h plan %s", h.PlanPDA)

	// Cycle 1: the ATOMIC subscribe creates the sub AND pulls period 1 in one tx.
	before1, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)
	row := devnetSubscribe(ctx, t, rc, raw, prepSvc, sub, h, amount, periodHours)
	awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, before1+amount, 90*time.Second,
		"cycle 1: atomic subscribe should pull exactly period 1 (1 USDC)")
	t.Logf("✅ cycle 1/%d OK (atomic subscribe): pulled 1 USDC; merchant %d->%d", cycles, before1, before1+amount)

	// An immediate separate crank now is period 2 — rejected as already-paid until
	// the period rolls over (the cap gates a same-period double-charge).
	_, earlyErr := crankSvc.Crank(ctx, dbtest.TestMerchantID, row, amount)
	require.Error(t, earlyErr, "an immediate crank after the atomic subscribe must be rejected (period 1 already paid)")
	require.Equal(t, declinecode.AlreadyPaid, ClassifyCrankError(earlyErr).Category,
		"immediate post-subscribe crank should classify as AlreadyPaid (Custom:400), got: %v", earlyErr)
	t.Logf("   immediate re-pull correctly rejected as already-paid: %v", earlyErr)

	const pollEvery = 90 * time.Second
	start := time.Now()
	lastSig := "" // distinct-tx guard across all separate pulls

	// Cycles 2..N: each is a SEPARATE crank that can only land after a real ~1h
	// rollover. The success is the proof of recurrence (NOT a replay of period 1).
	for c := 2; c <= cycles; c++ {
		before, _ := rc.GetTokenBalanceForMint(ctx, merchant.PublicKey(), usdc)

		var sig string
		for {
			s, perr := crankSvc.Crank(ctx, dbtest.TestMerchantID, row, amount)
			if perr == nil {
				sig = s
				break // period rolled over: the rebill went through.
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

		awaitTokenCredit(ctx, t, rc, merchant.PublicKey(), usdc, before+amount, 90*time.Second,
			"cycle %d: merchant should receive exactly 1 USDC", c)
		require.NotEqual(t, lastSig, sig, "cycle %d: rebill must be a DISTINCT on-chain tx (not a replay)", c)
		lastSig = sig
		t.Logf("✅ cycle %d/%d OK (REBILL PROVEN): pulled 1 USDC; merchant %d->%d; sig=%s; elapsed=%s",
			c, cycles, before, before+amount, sig, time.Since(start).Round(time.Second))

		// The cap must now block a same-period double-charge.
		_, eErr := crankSvc.Crank(ctx, dbtest.TestMerchantID, row, amount)
		require.Error(t, eErr, "cycle %d: immediate re-pull must be rejected (period not rolled over)", c)
		require.Equal(t, declinecode.AlreadyPaid, ClassifyCrankError(eErr).Category,
			"cycle %d: immediate re-pull should classify as AlreadyPaid (Custom:400), got: %v", c, eErr)
	}

	// Cancel on-chain after the last cycle.
	cancelSvc := NewPrepareCancelService(fixedSubReader{row: row}, rc)
	cres, xerr := cancelSvc.Prepare(ctx, row.SubscriptionID)
	require.NoError(t, xerr)
	signAndSendBase64(ctx, t, rc, raw, sub, []string{cres.Transaction})
	t.Logf("Cancel OK: cancel_subscription submitted for %s", cres.SubscriptionPDA)

	t.Logf("✅ SUSTAINED REBILL VALIDATED ON DEVNET: %d cycles (atomic subscribe + %d real rollovers), each a distinct on-chain pull with the cap gating double-charges; total elapsed %s",
		cycles, cycles-1, time.Since(start).Round(time.Second))
}
