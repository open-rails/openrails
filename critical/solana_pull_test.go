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

// settleOperations waits until no provider operation is being worked.
func settleOperations(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	require.Eventually(t, func() bool { return operationsIdle(t, fleet) }, 20*time.Second, 20*time.Millisecond)
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
