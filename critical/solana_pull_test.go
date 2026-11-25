//go:build integration

package critical_test

import (
	"strings"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pullMargin is how long after its period end a Solana pull is due: the
// chain's clock may lag ours.
const pullMargin = time.Minute

// operationJobs are the provider operations' River wakes in states.
func operationJobs(t *testing.T, fleet *river.Client[pgx.Tx], states ...rivertype.JobState) []*rivertype.JobRow {
	t.Helper()
	params := river.NewJobListParams().Kinds("openrails.provider_operation")
	if len(states) > 0 {
		params = params.States(states...)
	}
	jobs, err := fleet.JobList(t.Context(), params)
	require.NoError(t, err)
	return jobs.Jobs
}

// settleOperations waits until no provider operation is being worked; on
// failure it shows what each operation's job was doing.
func settleOperations(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	if assert.Eventually(t, func() bool { return operationsIdle(t, fleet) }, 20*time.Second, 20*time.Millisecond) {
		return
	}
	for _, job := range operationJobs(t, fleet) {
		t.Logf("provider operation job %d: %s, attempt %d, scheduled %s from now, %s", job.ID, job.State, job.Attempt, time.Until(job.ScheduledAt).Round(time.Millisecond), job.EncodedArgs)
	}
	t.FailNow()
}

func TestSolanaPulls(t *testing.T) {
	t.Run("OR-J37-S60/a-due-pull-runs-as-the-operation-its-period-scheduled", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		fleet := startFleet(t, s.world)
		require.NotEmpty(t, operationJobs(t, fleet), "enrollment schedules the next period's pull with its own wake")
		settleOperations(t, fleet)
		sends := s.wire.sends()
		s.clock.Advance(due.Sub(s.clock.Now()))
		wakeOperations(t, fleet)
		settleOperations(t, fleet)
		require.Equal(t, sends, s.wire.sends(), "at the period end the chain's clock may not have reached it: the pull waits its margin")
		s.clock.Advance(pullMargin)
		landedAt := s.clock.Now()
		wakeOperations(t, fleet)
		s.renewed(t, sub, landedAt.Add(168*time.Hour))
		require.Equal(t, sends+1, s.wire.sends())
		settleOperations(t, fleet)
		require.NotEmpty(t, operationJobs(t, fleet, rivertype.JobStateScheduled), "the renewal schedules the next period's pull")
	})
	t.Run("OR-J37-S61/a-pull-the-chain-refuses-before-its-period-end-is-retried-not-taken-as-paid", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		// The chain's clock runs 90 seconds behind ours, past the margin.
		s.node.SetClock(func() time.Time { return s.clock.Now().Add(-90 * time.Second) })
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)
		s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
		sends := s.wire.sends()
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool { return s.wire.sends() > sends }, 20*time.Second, 20*time.Millisecond, "the pull is sent on time and the chain refuses it")
		settleOperations(t, fleet)
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, current.Status, "a pull refused before the chain's period end is no evidence the period was paid")
		unbooked, err := s.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "consistency.ledger.unbooked"})
		require.NoError(t, err)
		require.Empty(t, unbooked.Items)
		s.clock.Advance(2 * time.Minute)
		landedAt := s.clock.Now().Add(-90 * time.Second)
		wakeOperations(t, fleet)
		s.renewed(t, sub, landedAt.Add(168*time.Hour))
	})
	t.Run("OR-J37-S07/retried-pull-keeps-its-backoff-then-renews-once", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		// The node cannot hand out a blockhash: the pull fails before anything
		// is signed, a clean retryable failure.
		s.wire.refuse("getLatestBlockhash")
		sends := s.wire.sends()
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)
		s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool { return s.wire.count("getLatestBlockhash") > 0 }, 20*time.Second, 20*time.Millisecond, "the due pull must actually be attempted")
		settleOperations(t, fleet)
		attempted := s.wire.count("getLatestBlockhash")
		require.Equal(t, sends, s.wire.sends(), "nothing was signed or sent")
		wakeOperations(t, fleet)
		settleOperations(t, fleet)
		require.Equal(t, attempted, s.wire.count("getLatestBlockhash"), "another wake does not retry the pull ahead of its backoff")
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, due, *current.CurrentPeriodEndsAt)
		require.Equal(t, billing.SubscriptionActive, current.Status)

		s.wire.refuse("")
		s.clock.Advance(3 * time.Minute)
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool {
			current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.CurrentPeriodEndsAt.After(due)
		}, 20*time.Second, 20*time.Millisecond, "the retried pull renews the period")
		settleOperations(t, fleet)
		wakeOperations(t, fleet)
		settleOperations(t, fleet)
		payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 2, "the subscribe payment and exactly one renewal")
		require.Equal(t, sends+1, s.wire.sends())
		require.EqualValues(t, 20_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	})
	t.Run("OR-J37-S62/a-dropped-pull-is-signed-again-only-once-it-provably-never-landed", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)
		// The leader drops the period's pull: the node acknowledges it and it
		// never lands.
		s.node.Drop(1)
		sends := s.wire.sends()
		s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool { return s.wire.sends() > sends }, 20*time.Second, 20*time.Millisecond, "the due pull is signed and sent")
		// The cluster moves past the last block height that could include it.
		s.node.AdvanceBlocks(200)
		settleOperations(t, fleet)
		require.Equal(t, sends+1, s.wire.sends(), "an unseen pull is not re-sent before its expiry is proven")
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, current.Status, "a dropped pull is no subscriber decline")
		require.Equal(t, due, *current.CurrentPeriodEndsAt)

		// The verifier proves it never landed: finalized past its last valid
		// height, unknown to a history search. That proof sends nothing.
		s.clock.Advance(2 * time.Minute)
		wakeOperations(t, fleet)
		settleOperations(t, fleet)
		require.Equal(t, sends+1, s.wire.sends())

		// The next attempt signs the pull again; it lands once.
		s.clock.Advance(5 * time.Minute)
		landedAt := s.clock.Now()
		wakeOperations(t, fleet)
		s.renewed(t, sub, landedAt.Add(168*time.Hour))
		require.Equal(t, sends+2, s.wire.sends())
		settleOperations(t, fleet)
		s.clock.Advance(5 * time.Minute)
		wakeOperations(t, fleet)
		settleOperations(t, fleet)
		require.Equal(t, sends+2, s.wire.sends(), "a pull that landed is never repeated")
		s.renewed(t, sub, landedAt.Add(168*time.Hour))
	})
	t.Run("OR-J37-S72/a-pull-confirmed-and-not-finalized-books-nothing-and-is-not-signed-again", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)
		s.node.WithholdFinality(1)
		sends := s.wire.sends()
		s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
		landedAt := s.clock.Now()
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool { return s.wire.sends() > sends }, 20*time.Second, 20*time.Millisecond, "the due pull is signed and sent")
		pull := s.wire.lastSignature()
		// The pull is confirmed: the run ends with its settlement pending.
		settleOperations(t, fleet)
		s.pending(t, fleet, sub, due, pull, sends+1)
		// The chain moves past its last valid height while it is still only
		// confirmed: a confirmed signature has landed.
		s.node.AdvanceBlocks(200)
		s.pending(t, fleet, sub, due, pull, sends+1)

		s.node.Finalize(pull)
		s.clock.Advance(time.Hour)
		wakeOperations(t, fleet)
		s.renewed(t, sub, landedAt.Add(168*time.Hour))
		settleOperations(t, fleet)
		s.clock.Advance(time.Hour)
		wakeOperations(t, fleet)
		settleOperations(t, fleet)
		require.Equal(t, sends+1, s.wire.sends(), "the finalized pull is booked once and never repeated")
		s.renewed(t, sub, landedAt.Add(168*time.Hour))
	})
	t.Run("OR-J37-S74/a-confirmed-pull-its-fork-drops-books-nothing-and-is-signed-again-only-after-its-blockhash-expires", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		fleet := startFleet(t, s.world)
		settleOperations(t, fleet)
		s.node.WithholdFinality(1)
		sends := s.wire.sends()
		s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool { return s.wire.sends() > sends }, 20*time.Second, 20*time.Millisecond, "the due pull is signed and sent")
		pull := s.wire.lastSignature()
		settleOperations(t, fleet)
		s.pending(t, fleet, sub, due, pull, sends+1)

		// The fork that held it is dropped while its blockhash can still land.
		s.node.Abandon(pull)
		s.pending(t, fleet, sub, due, pull, sends+1)

		// Past its last valid height the chain proves it never landed: it is
		// signed again, once, and lands.
		s.node.AdvanceBlocks(200)
		s.clock.Advance(time.Hour)
		wakeOperations(t, fleet)
		settleOperations(t, fleet)
		require.Equal(t, sends+1, s.wire.sends(), "the proof that it never landed sends nothing")
		s.clock.Advance(time.Hour)
		landedAt := s.clock.Now()
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool { return s.wire.sends() == sends+2 }, 20*time.Second, 20*time.Millisecond, "the proven-unlanded pull is signed again")
		require.Eventually(t, func() bool {
			current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.CurrentPeriodEndsAt.After(due)
		}, 20*time.Second, 20*time.Millisecond, "the second pull renews the period")
		settleOperations(t, fleet)
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.False(t, current.CurrentPeriodEndsAt.Before(landedAt.Add(168*time.Hour)))
		require.Equal(t, sends+2, s.wire.sends())
		payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 2, "the subscribe payment and exactly one renewal")
		require.EqualValues(t, 20_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	})
	t.Run("OR-J37-S09/low-cranker-gas-after-a-signed-pull-is-a-finding-until-funded", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		cranker := s.cranker.PublicKey().String()
		lowGas := func() []billing.Finding {
			page, err := s.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.solana.cranker_low_gas"})
			require.NoError(t, err)
			var open []billing.Finding
			for _, f := range page.Items {
				if strings.HasSuffix(f.SubjectKey, ":"+cranker) {
					open = append(open, f)
				}
			}
			return open
		}
		fleet := startFleet(t, s.world)
		renew := func() {
			t.Helper()
			settleOperations(t, fleet)
			current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
			require.NoError(t, err)
			due := *current.CurrentPeriodEndsAt
			s.clock.Advance(due.Add(pullMargin).Sub(s.clock.Now()))
			wakeOperations(t, fleet)
			require.Eventually(t, func() bool {
				renewed, err := s.alpha.GetSubscription(t.Context(), sub.ID)
				return err == nil && renewed.CurrentPeriodEndsAt.After(due)
			}, 20*time.Second, 20*time.Millisecond, "the scheduled pull signs and lands")
			settleOperations(t, fleet)
		}

		s.node.SetSOLBalance(s.cranker.PublicKey(), 1_000)
		renew()
		require.Len(t, lowGas(), 1, "a pull signed with a low cranker raises its finding")
		require.Equal(t, "high", lowGas()[0].Severity)

		s.node.SetSOLBalance(s.cranker.PublicKey(), 10_000_000_000)
		renew()
		require.Empty(t, lowGas(), "the next signed pull finds it funded and resolves it")
	})
}

// pending runs three verifier passes over a pull that is not finalized and
// proves each read the chain and changed nothing: no booking, no extension,
// no further send.
func (s *solanaWorld) pending(t *testing.T, fleet *river.Client[pgx.Tx], sub *billing.Subscription, due time.Time, pull string, sends int) {
	t.Helper()
	for range 3 {
		reads := s.wire.readCount(pull)
		s.clock.Advance(time.Hour)
		wakeOperations(t, fleet)
		require.Eventually(t, func() bool { return s.wire.readCount(pull) > reads }, 20*time.Second, 20*time.Millisecond, "the verifier reads the pull again")
		settleOperations(t, fleet)
		require.Equal(t, sends, s.wire.sends(), "a pull not proven unlanded is never signed again")
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionActive, current.Status)
		require.Equal(t, due, *current.CurrentPeriodEndsAt, "a pull not finalized extends nothing")
		payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 1, "a pull not finalized books nothing")
	}
}
