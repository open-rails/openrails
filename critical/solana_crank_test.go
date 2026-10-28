//go:build integration

package critical_test

import (
	"context"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/solanafake"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func TestSolanaCrank(t *testing.T) {
	t.Run("OR-J37-S06/retried-pull-keeps-its-backoff-then-renews-once", func(t *testing.T) {
		s := newSolanaWorld(t, true)
		_, sub := s.subscribe(t)
		due := *sub.CurrentPeriodEndsAt
		s.clock.Advance(due.Sub(s.clock.Now()))
		// The node cannot hand out a blockhash: the pull fails before anything
		// is signed, a clean retryable failure.
		s.wire.refuse("getLatestBlockhash")
		sends := s.wire.sends()
		fleet := startFleet(t, s.world)
		runRegistered(t, fleet, "openrails.solana_crank", 1)
		attempted := s.wire.count("getLatestBlockhash")
		require.Positive(t, attempted, "the due pull must actually be attempted")
		require.Equal(t, sends, s.wire.sends(), "nothing was signed or sent")
		runRegistered(t, fleet, "openrails.solana_crank", 1)
		require.Equal(t, attempted, s.wire.count("getLatestBlockhash"), "a later crank pass does not retry the pull ahead of its backoff")
		current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, due, *current.CurrentPeriodEndsAt)
		require.Equal(t, billing.SubscriptionActive, current.Status)

		s.wire.refuse("")
		s.clock.Advance(3 * time.Minute)
		// River uses wall time; the backoff is business time. Make the
		// operation's job available and let the worker check its own due time.
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		require.NotEmpty(t, jobs.Jobs)
		for _, job := range jobs.Jobs {
			if job.State == rivertype.JobStateScheduled || job.State == rivertype.JobStateRetryable {
				_, err = fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
		require.Eventually(t, func() bool {
			current, err := s.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.CurrentPeriodEndsAt.After(due)
		}, 20*time.Second, 20*time.Millisecond, "the retried pull renews the period")
		runRegistered(t, fleet, "openrails.solana_crank", 2)
		payments, err := s.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, payments.Items, 2, "the subscribe payment and exactly one renewal")
		require.Equal(t, sends+1, s.wire.sends())
		require.EqualValues(t, 20_000_000, s.node.Balance(s.treasury.PublicKey(), solanago.MustPublicKeyFromBase58(solanafake.DevnetUSDCMint)))
	})
	t.Run("OR-J37-S07/crank-runs-only-with-a-cranker", func(t *testing.T) {
		for _, recurring := range []bool{false, true} {
			name := "without-cranker"
			if recurring {
				name = "with-cranker"
			}
			t.Run(name, func(t *testing.T) {
				s := newSolanaWorld(t, recurring, solanaFixtureOptions{manualFleet: true})
				fleet := startFleet(t, s.world)
				kinds := func(kind string) int {
					jobs, err := fleet.JobList(context.Background(), river.NewJobListParams().Kinds(kind).States(rivertype.JobStateAvailable, rivertype.JobStateRunning, rivertype.JobStateCompleted, rivertype.JobStateScheduled, rivertype.JobStateRetryable, rivertype.JobStateDiscarded))
					if err != nil {
						return -1
					}
					return len(jobs.Jobs)
				}
				// Periodic work runs once on start; Solana Pay GC is always there.
				require.Eventually(t, func() bool { return kinds("openrails.solana_pay_gc") > 0 }, 20*time.Second, 50*time.Millisecond)
				if recurring {
					require.Eventually(t, func() bool { return kinds("openrails.solana_crank") > 0 }, 20*time.Second, 50*time.Millisecond)
					return
				}
				require.Never(t, func() bool { return kinds("openrails.solana_crank") > 0 }, time.Second, 50*time.Millisecond,
					"a deployment with no cranker has no recurring pulls to run")
			})
		}
	})
}
