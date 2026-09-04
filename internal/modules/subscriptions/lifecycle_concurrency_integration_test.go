//go:build integration

package subscriptions

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
)

// The first clock read in renewal follows its subscription read and terminal
// guard. Pause there while another lifecycle operation contends for that row.
type lifecycleReadBarrier struct {
	clockwork.Clock
	once    sync.Once
	reached chan struct{}
	resume  chan struct{}
}

func (c *lifecycleReadBarrier) Now() time.Time {
	c.once.Do(func() {
		close(c.reached)
		<-c.resume
	})
	return c.Clock.Now()
}

func TestConcurrentRenewalLifecycle(t *testing.T) {
	for _, action := range []string{"chargeback_by_id", "chargeback_by_rail", "distinct_renewal"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(dbtest.WithTestMerchant(t.Context()), 20*time.Second)
			defer cancel()
			database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
			now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
			paidEnd := now.Add(30 * 24 * time.Hour)
			insertCatalogAndSub(ctx, t, database, now, 30, productID, priceID, subID, uuid.NewString(), now, paidEnd)
			railSubID := "lifecycle-" + uuid.NewString()
			_, err := database.Pool().Exec(ctx, "UPDATE openrails.subscriptions SET rail_subscription_id=$2 WHERE id=$1", subID, railSubID)
			require.NoError(t, err)

			barrier := &lifecycleReadBarrier{Clock: clockwork.NewFakeClockAt(now), reached: make(chan struct{}), resume: make(chan struct{})}
			var release sync.Once
			resume := func() { release.Do(func() { close(barrier.resume) }) }
			defer resume()
			first := newLifecycleForTest(database)
			first.SetClock(barrier)
			renewal := func(transactionID string) *RenewMembershipParams {
				return &RenewMembershipParams{Rail: models.RailSolana, RailSubscriptionID: railSubID, TransactionID: transactionID, Amount: 999, AmountProvided: true, Currency: "USD"}
			}
			firstDone := make(chan error, 1)
			go func() { firstDone <- first.RenewMembership(ctx, renewal(uuid.NewString())) }()
			select {
			case <-barrier.reached:
			case err := <-firstDone:
				t.Fatalf("renewal finished before the interleaving point: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}

			second := newLifecycleForTest(database)
			second.SetClock(clockwork.NewFakeClockAt(now))
			secondDone := make(chan error, 1)
			go func() {
				if action == "distinct_renewal" {
					secondDone <- second.RenewMembership(ctx, renewal(uuid.NewString()))
					return
				}
				params := &CancelMembershipParams{CancelType: models.CancelTypeChargeback, RevokeAccess: true}
				if action == "chargeback_by_id" {
					params.SubscriptionID = &subID
				} else {
					rail := models.RailSolana
					params.Rail, params.RailSubscriptionID = &rail, &railSubID
				}
				secondDone <- second.CancelMembership(ctx, params)
			}()

			// Before the fix, the second operation can commit while the first holds
			// a stale snapshot. With row locks it waits; release the first only after
			// observing either outcome, instead of relying on a scheduler sleep.
			secondErr, secondFinished := waitForLifecycleContention(t, ctx, database, secondDone)
			resume()
			require.NoError(t, <-firstDone)
			if !secondFinished {
				secondErr = <-secondDone
			}
			require.NoError(t, secondErr)

			var status string
			var cancelType *string
			var periodEnd time.Time
			require.NoError(t, database.Pool().QueryRow(ctx, "SELECT status, cancel_type, current_period_ends_at FROM openrails.subscriptions WHERE id=$1", subID).Scan(&status, &cancelType, &periodEnd))
			if action == "distinct_renewal" {
				require.Equal(t, string(models.StatusActive), status)
				require.Equal(t, paidEnd.Add(60*24*time.Hour), periodEnd.UTC(), "both distinct renewal periods must survive")
				var count int
				require.NoError(t, database.Pool().QueryRow(ctx, "SELECT count(*) FROM openrails.payments WHERE subscription_id=$1 AND status='completed'", subID).Scan(&count))
				require.Equal(t, 2, count)
			} else {
				require.Equal(t, string(models.StatusCancelled), status, "a concurrent renewal must not undo chargeback cancellation")
				require.NotNil(t, cancelType)
				require.Equal(t, string(models.CancelTypeChargeback), *cancelType)
			}
		})
	}
}

func waitForLifecycleContention(t *testing.T, ctx context.Context, database *db.DB, done <-chan error) (error, bool) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err, true
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
			var blocked bool
			err := database.Pool().QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE datname=current_database() AND wait_event_type='Lock'
				  AND query LIKE '%subscriptions%'
			)`).Scan(&blocked)
			require.NoError(t, err)
			if blocked {
				return nil, false
			}
		}
	}
}

func TestStaleLifecycleSnapshotPreservesChargeback(t *testing.T) {
	for _, action := range []string{"park_unknown", "enter_past_due", "resolve_unknown"} {
		t.Run(action, func(t *testing.T) {
			ctx := dbtest.WithTestMerchant(t.Context())
			database := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
			now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			productID, priceID, subID := uuid.New(), uuid.New(), uuid.New()
			insertCatalogAndSub(ctx, t, database, now, 30, productID, priceID, subID, uuid.NewString(), now, now.Add(30*24*time.Hour))
			if action == "resolve_unknown" {
				_, err := database.Pool().Exec(ctx, "UPDATE openrails.subscriptions SET status='unknown' WHERE id=$1", subID)
				require.NoError(t, err)
			}
			repo := NewSubscriptionRepo(database)
			snapshot, err := repo.GetByID(ctx, subID)
			require.NoError(t, err)
			lifecycle := newLifecycleForTest(database)
			lifecycle.SetClock(clockwork.NewFakeClockAt(now))
			require.NoError(t, lifecycle.CancelMembership(ctx, &CancelMembershipParams{SubscriptionID: &subID, CancelType: models.CancelTypeChargeback, RevokeAccess: true}))
			switch action {
			case "park_unknown":
				err = lifecycle.ApplyLocalUnknown(ctx, database, snapshot)
			case "enter_past_due":
				err = lifecycle.ApplyLocalPastDue(ctx, database, snapshot, now.Add(24*time.Hour))
			case "resolve_unknown":
				paidEnd := now.Add(60 * 24 * time.Hour)
				err = lifecycle.ResolveUnknownSubscription(ctx, database, snapshot, ResolveRenewed, &paidEnd, time.Time{})
			}
			require.NoError(t, err)
			current, err := repo.GetByID(ctx, subID)
			require.NoError(t, err)
			require.Equal(t, models.StatusCancelled, current.Status)
			require.NotNil(t, current.CancelType)
			require.Equal(t, models.CancelTypeChargeback, *current.CancelType)
		})
	}
}
