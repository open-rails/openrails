//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func membershipAtPrice(t *testing.T, a *authorityWorld, p *providerLedger, price billing.PriceID, token, key string) *billing.Subscription {
	t.Helper()
	input := billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price}}, ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", token, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	require.Len(t, order.Lines, 1)
	require.NotNil(t, order.Lines[0].SubscriptionID)
	sub, err := a.alpha.GetSubscription(t.Context(), *order.Lines[0].SubscriptionID)
	require.NoError(t, err)
	return sub
}

func migrationPrice(t *testing.T, a *authorityWorld, sub *billing.Subscription, key string, amount int64) *billing.Price {
	t.Helper()
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sub.ProductID, Key: key, UnitAmount: amount, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
	require.NoError(t, err)
	return price
}

func TestPriceMigrationContracts(t *testing.T) {
	t.Run("OR-J19-S02/failed-provider-push-is-retried-by-its-operation", func(t *testing.T) {
		a, p, sub := nmiMembership(t, func(s *nmimock.Schedule) { s.Plan, s.Custom = "", true })
		target, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sub.ProductID, Key: "replacement-weekly", UnitAmount: 20_000_000, Currency: "USD", BillingIntervalHours: ptr(168), AccessDurationHours: ptr(168)})
		require.NoError(t, err)
		p.core.FailScheduleUpdates(1)
		migration, err := a.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: sub.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true})
		require.NoError(t, err)
		require.Equal(t, 1, migration.Scheduled, "the push is recorded with the move")
		require.Zero(t, migration.Blocked)
		require.False(t, migrationSettled(t, a.world, migration.ID))
		fleet := manualFleet(t, a.world)
		require.Eventually(t, func() bool {
			wakeOperations(t, fleet)
			return len(p.core.ScheduleUpdates(*sub.RailSubscriptionID)) > 0
		}, 20*time.Second, 50*time.Millisecond)
		// The clock moves only once the failed push has recorded its retry.
		require.Eventually(t, func() bool { return operationsIdle(t, fleet) }, 20*time.Second, 50*time.Millisecond)
		a.clock.Advance(time.Hour)
		require.Eventually(t, func() bool {
			wakeOperations(t, fleet)
			result, err := a.alpha.GetPriceMigration(t.Context(), migration.ID)
			return err == nil && result.Applied == 1
		}, 20*time.Second, 50*time.Millisecond)
		result, err := a.alpha.GetPriceMigration(t.Context(), migration.ID)
		require.NoError(t, err)
		require.Zero(t, result.Blocked)
		require.Zero(t, result.Scheduled)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, target.ID, current.PriceID)
		require.Equal(t, sub.CurrentPeriodEndsAt, current.CurrentPeriodEndsAt)
		require.Equal(t, "20.00", p.core.Schedule(*sub.RailSubscriptionID).Amount)
		require.Len(t, p.core.ScheduleUpdates(*sub.RailSubscriptionID), 2, "one failed push and one retry")
		require.True(t, migrationSettled(t, a.world, migration.ID), "the command settles with its last move")
		p.money(t, 1, 1000)
	})
	t.Run("OR-J19-S03", func(t *testing.T) {
		for _, profile := range []string{"pending", "already-applied"} {
			t.Run(profile, func(t *testing.T) {
				a, p, alice := purchasedMembership(t)
				bob := membershipAtPrice(t, a, p, alice.PriceID, a.bobToken, "ineligible-membership")
				target := migrationPrice(t, a, alice, "cancelable-migration", 30_000_000)
				reserved := upgradePrice(t, a, alice)
				_, err := a.alpha.ChangeSubscription(t.Context(), bob.ID, billing.ChangeSubscriptionParams{PriceID: &reserved.ID, Reason: "Retain this accepted change", IdempotencyKey: "ineligible-change"})
				require.NoError(t, err)
				migration, err := a.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: alice.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true})
				require.NoError(t, err)
				require.Equal(t, 1, migration.Scheduled)
				require.Equal(t, 1, migration.Skipped)
				fleet := startFleet(t, a.world)
				if profile == "already-applied" {
					a.clock.Advance(alice.CurrentPeriodEndsAt.Sub(a.clock.Now()))
					runRegistered(t, fleet, "openrails.dunning", 2)
					require.Eventually(t, func() bool {
						current, err := a.alpha.GetPriceMigration(t.Context(), migration.ID)
						return err == nil && current.Applied == 1
					}, 20*time.Second, 20*time.Millisecond)
				}
				canceled, err := a.alpha.CancelPriceMigration(t.Context(), migration.ID)
				require.NoError(t, err)
				if profile == "pending" {
					require.Equal(t, 1, canceled.Canceled)
					a.clock.Advance(alice.CurrentPeriodEndsAt.Sub(a.clock.Now()))
				} else {
					require.Zero(t, canceled.Canceled)
					require.Equal(t, 1, canceled.PriceMigration.Applied)
				}
				runRegistered(t, fleet, "openrails.dunning", 2)
				alicePrice := alice.PriceID
				aliceAmount := int64(1000)
				if profile == "already-applied" {
					alicePrice, aliceAmount = target.ID, 3000
				}
				for _, expected := range []struct {
					sub   *billing.Subscription
					price billing.PriceID
				}{{alice, alicePrice}, {bob, reserved.ID}} {
					require.Eventually(t, func() bool {
						current, err := a.alpha.GetSubscription(t.Context(), expected.sub.ID)
						return err == nil && current.PriceID == expected.price && current.CurrentPeriodStartsAt.Equal(*expected.sub.CurrentPeriodEndsAt)
					}, 20*time.Second, 20*time.Millisecond)
				}
				var amounts []int64
				for _, effect := range p.effects(t) {
					amounts = append(amounts, effect.Amount)
				}
				require.ElementsMatch(t, []int64{1000, 1000, aliceAmount, 2000}, amounts)
				repeated, err := a.alpha.CancelPriceMigration(t.Context(), migration.ID)
				require.NoError(t, err)
				require.Zero(t, repeated.Canceled)
				require.Len(t, p.effects(t), 4)
			})
		}
	})
	t.Run("OR-J19-S04/active-renewal-keeps-its-admitted-price", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		target := migrationPrice(t, a, sub, "after-accepted-renewal", 30_000_000)
		due := *sub.CurrentPeriodEndsAt
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		p.mu.Lock()
		p.responseGate = gate
		p.mu.Unlock()
		defer gate.unblock()
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.dunning", 1)
		select {
		case <-gate.arrived:
		case <-time.After(15 * time.Second):
			t.Fatal("renewal did not reach provider acceptance")
		}
		migration, err := a.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: sub.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true})
		require.NoError(t, err)
		require.Equal(t, 1, migration.Skipped)
		require.Zero(t, migration.Scheduled)
		require.Zero(t, migration.Applied)
		pending, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Nil(t, pending.ScheduledChange)
		require.Equal(t, sub.PriceID, pending.PriceID)
		gate.unblock()
		renewalSettled(t, a, p, sub, due)
		result, err := a.alpha.GetPriceMigration(t.Context(), migration.ID)
		require.NoError(t, err)
		require.Equal(t, 1, result.Skipped)
		require.Zero(t, result.Applied)
	})
	t.Run("OR-J19-S01/only-eligible-subscription-moves-at-the-paid-boundary", func(t *testing.T) {
		a, p, alice := purchasedMembership(t)
		bob := membershipAtPrice(t, a, p, alice.PriceID, a.bobToken, "bob-membership")
		target := migrationPrice(t, a, alice, "migration-thirty", 30_000_000)
		reserved := upgradePrice(t, a, alice)
		_, err := a.alpha.ChangeSubscription(t.Context(), bob.ID, billing.ChangeSubscriptionParams{PriceID: &reserved.ID, Reason: "Already accepted customer change", IdempotencyKey: "bob-reserved-change"})
		require.NoError(t, err)
		params := billing.CreatePriceMigrationParams{FromPriceID: alice.PriceID, ToPriceID: target.ID, EffectiveAt: *alice.CurrentPeriodEndsAt, AcknowledgeShortNotice: true}
		preview, err := a.alpha.PreviewPriceMigration(t.Context(), params)
		require.NoError(t, err)
		require.Equal(t, 2, preview.Matched)
		require.Equal(t, 1, preview.Scheduled)
		require.Equal(t, 1, preview.Skipped)
		migration, err := a.alpha.CreatePriceMigration(t.Context(), params)
		require.NoError(t, err)
		require.Equal(t, 1, migration.Scheduled)
		require.Equal(t, 1, migration.Skipped)
		before, err := a.alpha.GetSubscription(t.Context(), alice.ID)
		require.NoError(t, err)
		require.Equal(t, alice.PriceID, before.PriceID)
		require.NotNil(t, before.ScheduledChange)
		require.Equal(t, migration.ID, *before.ScheduledChange.PriceMigrationID)
		fleet := startFleet(t, a.world)
		a.clock.Advance(alice.CurrentPeriodEndsAt.Sub(a.clock.Now()) - time.Microsecond)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Len(t, p.effects(t), 2, "migration cannot collect early")
		a.clock.Advance(time.Microsecond)
		runRegistered(t, fleet, "openrails.dunning", 2)
		for _, expected := range []struct {
			sub   *billing.Subscription
			price billing.PriceID
		}{{alice, target.ID}, {bob, reserved.ID}} {
			require.Eventually(t, func() bool {
				current, err := a.alpha.GetSubscription(t.Context(), expected.sub.ID)
				return err == nil && current.PriceID == expected.price && current.CurrentPeriodStartsAt.Equal(*expected.sub.CurrentPeriodEndsAt)
			}, 20*time.Second, 20*time.Millisecond)
		}
		result, err := a.alpha.GetPriceMigration(t.Context(), migration.ID)
		require.NoError(t, err)
		require.Equal(t, 1, result.Applied)
		require.Zero(t, result.Scheduled)
		require.Equal(t, 1, result.Skipped)
		var amounts []int64
		for _, effect := range p.effects(t) {
			amounts = append(amounts, effect.Amount)
		}
		require.ElementsMatch(t, []int64{1000, 1000, 2000, 3000}, amounts)
		runRegistered(t, fleet, "openrails.dunning", 2)
		require.Len(t, p.effects(t), 4)
	})
	t.Run("OR-J19-S05/move-that-no-longer-applies-is-skipped-and-settles-its-migration", func(t *testing.T) {
		a, p, sub := nmiMembership(t, func(s *nmimock.Schedule) { s.Plan, s.Custom = "", true })
		target, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sub.ProductID, Key: "withdrawn-weekly", UnitAmount: 20_000_000, Currency: "USD", BillingIntervalHours: ptr(168), AccessDurationHours: ptr(168)})
		require.NoError(t, err)
		migration, err := a.alpha.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{FromPriceID: sub.PriceID, ToPriceID: target.ID, AcknowledgeShortNotice: true})
		require.NoError(t, err)
		require.Equal(t, 1, migration.Scheduled)
		require.False(t, migrationSettled(t, a.world, migration.ID))
		_, err = a.alpha.UpdatePrice(t.Context(), target.ID, billing.UpdatePriceParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		fleet := manualFleet(t, a.world)
		require.Eventually(t, func() bool {
			wakeOperations(t, fleet)
			result, err := a.alpha.GetPriceMigration(t.Context(), migration.ID)
			return err == nil && result.Skipped == 1
		}, 20*time.Second, 50*time.Millisecond)
		result, err := a.alpha.GetPriceMigration(t.Context(), migration.ID)
		require.NoError(t, err)
		require.Zero(t, result.Scheduled)
		require.Zero(t, result.Blocked)
		require.Zero(t, result.Applied)
		require.True(t, migrationSettled(t, a.world, migration.ID), "a skipped move is terminal and settles its migration")
		require.Empty(t, p.core.ScheduleUpdates(*sub.RailSubscriptionID))
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, sub.PriceID, current.PriceID)
		require.Nil(t, current.ScheduledChange)
	})
}

// manualFleet runs registered workers with no periodic pass.
func manualFleet(t *testing.T, w *world) *river.Client[pgx.Tx] {
	t.Helper()
	fleet := hostFleet(t, w)
	fleet.PeriodicJobs().Clear()
	require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.NoError(t, fleet.Start(t.Context()))
	return fleet
}

// wakeOperations makes provider operations River scheduled on business time
// available now; each still checks its own due time.
func wakeOperations(t *testing.T, fleet *river.Client[pgx.Tx]) {
	t.Helper()
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation").States(rivertype.JobStateScheduled, rivertype.JobStateRetryable))
	require.NoError(t, err)
	for _, job := range jobs.Jobs {
		_, err = fleet.JobRetry(t.Context(), job.ID)
		require.NoError(t, err)
	}
}

// migrationSettled reports whether the migration's command finished.
func migrationSettled(t *testing.T, w *world, id billing.PriceMigrationID) bool {
	t.Helper()
	var state string
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT a.state FROM "+pgx.Identifier{w.schema, "destructive_actions"}.Sanitize()+" a JOIN "+
		pgx.Identifier{w.schema, "price_migrations"}.Sanitize()+" m ON m.merchant_id = a.merchant_id AND m.destructive_action_id = a.id WHERE m.id = $1", id.UUID()).Scan(&state))
	return state == "succeeded"
}
