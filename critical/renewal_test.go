//go:build integration

package critical_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func startFleet(t *testing.T, w *world) *river.Client[pgx.Tx] {
	t.Helper()
	fleet := hostFleet(t, w, 2)
	require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.NoError(t, fleet.Start(t.Context()))
	return fleet
}

// Insert a normal host job and observe the actual registered worker's durable
// completion. This drives a due pass; it makes no claim about scheduler cadence.
func runRegistered(t *testing.T, fleet *river.Client[pgx.Tx], kind string, copies int) {
	t.Helper()
	finished := finishPass(t, fleet, kind, func() []int64 {
		ids := make([]int64, 0, copies)
		for range copies {
			job, err := fleet.Insert(t.Context(), registeredJob{KindName: kind}, &river.InsertOpts{Queue: registeredQueue(kind)})
			require.NoError(t, err)
			ids = append(ids, job.Job.ID)
		}
		return ids
	})
	for _, event := range finished {
		require.Equal(t, river.EventKindJobCompleted, event.Kind, "%s: %+v", kind, event.Job.Errors)
	}
	// River uses wall time while business time is advanced independently. Make
	// newly accepted work available through River's host API; worker admission
	// still checks its own persisted eligibility at the controlled clock.
	if kind == "openrails.billing_pass" {
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			if jobWaits(job) {
				_, err = fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
	}
}

// registeredQueue is the queue OpenRails serves kind on.
func registeredQueue(kind string) string {
	if kind == "openrails.nmi_history" {
		return "nmi_history"
	}
	return openrails.QueueBilling
}

func renewalSettled(t *testing.T, a *authorityWorld, p *providerLedger, sub *billing.Subscription, due time.Time) {
	t.Helper()
	require.Eventually(t, func() bool {
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		return err == nil && current.CurrentPeriodStartsAt != nil && current.CurrentPeriodStartsAt.Equal(due)
	}, 20*time.Second, 20*time.Millisecond, "the accepted renewal must settle through the registered provider worker")
	current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
	require.NoError(t, err)
	require.Equal(t, billing.SubscriptionStatus("active"), current.Status)
	require.Equal(t, due, *current.CurrentPeriodStartsAt)
	require.Equal(t, due.Add(sub.CurrentPeriodEndsAt.Sub(*sub.CurrentPeriodStartsAt)), *current.CurrentPeriodEndsAt)
	effects := p.effects(t)
	require.Len(t, effects, 2, "initial purchase plus exactly one renewal")
	for _, effect := range effects {
		require.Equal(t, "charge", effect.Kind)
		require.Equal(t, "usd", effect.Currency)
		require.EqualValues(t, 1000, effect.Amount)
	}
	cycles, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: sub.ID})
	require.NoError(t, err)
	require.Len(t, cycles.Items, 1)
	require.Equal(t, due, cycles.Items[0].DueAt)
	require.EqualValues(t, 10_000_000, cycles.Items[0].Amount)
	require.Equal(t, "collected", cycles.Items[0].Outcome)
	payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
	require.NoError(t, err)
	require.Len(t, payments.Items, 2)
	for _, payment := range payments.Items {
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
		require.EqualValues(t, 10_000_000, payment.Amount)
	}
	events, err := a.alpha.ListEvents(t.Context(), billing.EventListParams{Types: []string{"payment.settled"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, events.Events, 2)
	require.NotEqual(t, events.Events[0].ID, events.Events[1].ID)
}

func TestRenewal(t *testing.T) {
	t.Run("OR-J14-S14/stuck-renewal-verification-slows-with-age-and-stops-at-the-cap", func(t *testing.T) {
		a, p, sub := nmiEngineMembership(t)
		p.core.DropSaleResponses(1)
		p.core.HideSales(1)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		operations := pgx.Identifier{a.schema, "provider_intents"}.Sanitize()
		var id uuid.UUID
		require.Eventually(t, func() bool {
			return a.pool.QueryRow(t.Context(), "SELECT id FROM "+operations+" WHERE subscription_id=$1 AND intent_type='subscription_collection' AND status='unknown_needs_verify' AND lease_expires_at IS NULL", sub.ID.UUID()).Scan(&id) == nil
		}, 20*time.Second, 20*time.Millisecond)
		require.NoError(t, fleet.StopAndCancel(t.Context()))
		// Match row creation to the controlled business clock.
		created := a.clock.Now().Truncate(time.Microsecond)
		_, err := a.pool.Exec(t.Context(), "UPDATE "+operations+" SET created_at=$1 WHERE id=$2", created, id)
		require.NoError(t, err)
		rt := embedded.Graph(a.engine).Runtime
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		for _, tc := range []struct{ age, delay time.Duration }{
			{time.Minute, 2 * time.Minute},
			{20 * time.Minute, 10 * time.Minute},
			{time.Hour, 30 * time.Minute},
			{4 * time.Hour, 2 * time.Hour},
			{24 * time.Hour, 2 * time.Hour},
		} {
			a.clock.Advance(created.Add(tc.age).Sub(a.clock.Now()))
			var row gen.BillingProviderIntent
			require.NoError(t, rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
				var err error
				row, err = rt.IntentRunner().VerifyByID(ctx, id)
				return err
			}))
			require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status)
			require.EqualValues(t, 1, row.Attempts, "verification does not consume execution attempts")
			require.WithinDuration(t, a.clock.Now().Add(tc.delay), row.NextAttemptAt, time.Microsecond, "operation age %s", tc.age)
			if tc.age >= 20*time.Minute {
				findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.submission.unresolved"})
				require.NoError(t, err)
				require.Len(t, findings.Items, 1)
			}
		}
		p.money(t, 2, 2000)
	})
	t.Run("OR-J14-S01/before-boundary-workers-cannot-admit-renewal", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) - time.Microsecond)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, *sub.CurrentPeriodEndsAt, *current.CurrentPeriodEndsAt)
		cycles, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		require.Empty(t, cycles.Items)
		require.Len(t, p.effects(t), 1)
	})
	t.Run("OR-J14-S02/two-due-workers-settle-one-period", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		renewalSettled(t, a, p, sub, due)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		renewalSettled(t, a, p, sub, due)
		paid, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: sub.ID})
		require.NoError(t, err)
		require.Len(t, paid.Items, 2)
		for _, payment := range paid.Items {
			require.NotNil(t, payment.Card, "the enrollment and the renewal name the card they charged")
			require.Equal(t, "visa", *payment.Card.Brand)
			require.Equal(t, "4242", *payment.Card.Last4)
		}
	})
	t.Run("OR-J14-S03/overdue-engine-restart-keeps-original-obligation", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		require.NoError(t, a.engine.Close(context.Background()))
		a.clock.Advance(due.Sub(a.clock.Now()) + 2*time.Hour)
		var err error
		a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
		require.NoError(t, err)
		a.alpha, err = declaredMerchant(a.engine, "alpha")
		require.NoError(t, err)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		renewalSettled(t, a, p, sub, due)
	})
	t.Run("OR-J14-S04/admitted-renewal-keeps-terms-after-price-replacement", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		hold := p.core.Hold(func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents" }, stripemock.HoldRequest)
		defer hold.Release()
		a.clock.Advance(due.Sub(a.clock.Now()))
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		select {
		case <-hold.Arrived():
		case <-time.After(10 * time.Second):
			t.Fatal("accepted renewal never reached the provider wire")
		}
		newPrice, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: sub.ProductID, Key: "membership-day", UnitAmount: 50_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		require.NotEqual(t, sub.PriceID, newPrice.ID)
		hold.Release()
		renewalSettled(t, a, p, sub, due)
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, sub.PriceID, current.PriceID)
	})
}
