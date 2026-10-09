//go:build e2e && integration

package subscriptions_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/stretchr/testify/require"
)

func TestNMIPaidUpgradeRecoversMoneyBeforeScheduleWrite(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 1, 999, monthHours, true)
			next := w.tierPrice(group, 2, 1999, monthHours, true)
			end := w.clock.Now().Add(10 * day).UTC().Truncate(day).Add(12 * time.Hour)
			l := w.legacyOnTierEndingAt(tp, old, 999, monthHours, end)
			sales := len(l.tierSales())
			paid := len(completed(w.payments(tp, l.c.id)))
			boundary := w.nmi.Schedule(l.railSub).NextBilling
			// A successful POST cannot yet be qualified by the independent read.
			// Retain the operation across a stale restart, with no fabricated fence.
			w.nmi.QueryUnavailable(true)
			answer, err := l.c.change(l.sub, billing.ChangeSubscriptionParams{PriceID: priceRef(next.ID), IdempotencyKey: "held-upgrade"})
			require.NoError(t, err)
			require.NotEqual(t, billing.PaymentOperationID{}, answer.OperationID)
			require.Len(t, l.tierSales(), sales+1)
			require.Len(t, completed(w.payments(tp, l.c.id)), paid)
			w.stop()
			w.advance(5 * time.Hour)
			w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeReadOnly }
			w.nmi.QueryUnavailable(false)
			w.start()
			w.until(func() bool { return len(completed(w.payments(tp, l.c.id))) == paid+1 }, "readonly recovers the qualified proration money")
			require.Equal(t, old.ID, w.subscription(tp, l.sub).PriceID)
			require.True(t, l.c.entitled(old.ent))
			require.False(t, l.c.entitled(next.ent), "money recording does not grant the target tier")
			require.Empty(t, w.nmi.ScheduleUpdates(l.railSub), "Verify does not write a schedule")
			require.Equal(t, "9.99", w.nmi.Schedule(l.railSub).Amount)
			require.Equal(t, boundary, w.nmi.Schedule(l.railSub).NextBilling)
			require.Len(t, l.tierSales(), sales+1)
			var approved int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_attempts WHERE provider_intent_id=$1 AND payment_id IS NOT NULL AND category='approved'`), answer.OperationID.UUID()).Scan(&approved))
			require.Equal(t, 1, approved, "qualified payment and approved attempt commit together")
			// Actual read/apply catch-up sees the payment. It cannot update the
			// schedule under readonly; configured full subsequently resumes it.
			w.refreshProviders()
			require.Empty(t, w.nmi.ScheduleUpdates(l.railSub))
			w.stop()
			w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeFull }
			w.start()
			w.until(func() bool { return w.subscription(tp, l.sub).PriceID == next.ID }, "full resumes the retained schedule update after real catch-up")
			require.True(t, l.c.entitled(next.ent))
			require.Len(t, l.tierSales(), sales+1, "no second proration sale")
			require.Len(t, completed(w.payments(tp, l.c.id)), paid+1, "no second payment during final tier commit")
			require.Len(t, w.nmi.ScheduleUpdates(l.railSub), 1)
			require.Equal(t, boundary, w.nmi.Schedule(l.railSub).NextBilling)
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payment_attempts WHERE provider_intent_id=$1 AND payment_id IS NOT NULL AND category='approved'`), answer.OperationID.UUID()).Scan(&approved))
			require.Equal(t, 1, approved)
		})
	}
}

func TestNMIUpgradeHiddenChargeNeverReleasesAnotherOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	old := w.tierPrice(group, 1, 999, monthHours, true)
	next := w.tierPrice(group, 2, 1999, monthHours, true)
	l := w.legacyOnTier(embedded, old, 999, monthHours, 10*day)
	sales := len(l.tierSales())
	paid := len(completed(w.payments(embedded, l.c.id)))
	w.nmi.HideSales(1)
	w.nmi.DropSaleResponses(1)
	answer, err := l.c.change(l.sub, billing.ChangeSubscriptionParams{PriceID: priceRef(next.ID), IdempotencyKey: "hidden-upgrade"})
	require.NoError(t, err)
	require.Len(t, l.tierSales(), sales+1)
	require.NoError(t, w.jobs.Stop(t.Context()))
	w.advance(2 * time.Hour)
	runtime := engine.Graph(w.rt).Runtime
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	runner := runtime.IntentRunner()
	row, err := runner.VerifyByID(ctx, answer.OperationID.UUID())
	require.NoError(t, err)
	_, retryErr := l.c.change(l.sub, billing.ChangeSubscriptionParams{PriceID: priceRef(next.ID), IdempotencyKey: "another-upgrade-order"})
	require.Len(t, l.tierSales(), sales+1, "an empty lookup must not free a second order that charges again")
	require.Error(t, retryErr, "the unresolved operation keeps ownership of the tier change")
	require.Equal(t, intents.StatusUnknownNeedsVerify, row.Status, "empty Query after the settle interval is not proof of non-execution")
	_, err = runner.Resolve(ctx, answer.OperationID.UUID(), intents.Resolution{Step: "proration", NotExecuted: true, Actor: "test-operator", Reason: "Query has no visible transaction"})
	require.ErrorContains(t, err, "cannot prove")
	require.Len(t, l.tierSales(), sales+1)
	require.Empty(t, w.nmi.ScheduleUpdates(l.railSub))
	require.Equal(t, old.ID, w.subscription(embedded, l.sub).PriceID)
	w.nmi.Reveal()
	require.NoError(t, w.jobs.Start(t.Context()))
	w.until(func() bool { return w.subscription(embedded, l.sub).PriceID == next.ID }, "the original order recovers its later exact receipt")
	require.Len(t, l.tierSales(), sales+1)
	require.Len(t, completed(w.payments(embedded, l.c.id)), paid+1)
	require.Len(t, w.nmi.ScheduleUpdates(l.railSub), 1)
}
