//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

// holdRecovery keeps a historical fixture spelling. It records an unrelated
// unresolved financial finding; findings never gate customer operations.
func (w *world) holdRecovery(rail string) (release func()) {
	w.t.Helper()
	subject := "unresolved-" + uuid.NewString()[:8]
	_, err := w.pool.Exec(w.t.Context(), w.q(`INSERT INTO billing.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status, psp_id, rail, source)
        SELECT id, 'pull.charge.missing', $2, 'high', 'reconcile_required', $3, $4, 'pull' FROM billing.merchants WHERE slug = $1`), w.slug, subject, w.psp[rail].UUID(), rail)
	require.NoError(w.t, err)
	return func() {
		w.t.Helper()
		_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE billing.reconciliation_findings SET status = 'fixed', resolution = 'admin_fixed', resolved_at = now() WHERE subject_key = $1`), subject)
		require.NoError(w.t, err)
	}
}

func (e *engineCase) cancel() (int, map[string]any) {
	return e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"reason": "moving on"})
}

// An unrelated financial finding blocks neither cancellation nor another
// member's renewal. Canceled memberships still cannot be renewed.
func TestCustomerCancelsEngineMembershipWithUnresolvedFinding(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			early, late, kept := enroll(t, w, rail, embedded), enroll(t, w, rail, embedded), enroll(t, w, rail, embedded)
			release := w.holdRecovery(rail)
			for _, gone := range []*engineCase{early, late} {
				status, body := gone.cancel()
				require.Equal(t, http.StatusOK, status, "%v", body)
				require.Equal(t, "canceled", body["status"])
			}
			end := kept.periodEnd()
			w.advance(end.Add(time.Second).Sub(w.clock.Now()))
			w.runRenewals()
			w.settle()
			require.Len(t, kept.providerLedger(), 4, "the kept member renews despite the unresolved finding")
			require.True(t, kept.periodEnd().After(end))
			require.True(t, kept.c.entitled(kept.ent))
			for _, gone := range []*engineCase{early, late} {
				sub := w.subscription(embedded, gone.sub)
				require.Equal(t, billing.SubscriptionCanceled, sub.Status)
				require.False(t, sub.CurrentPeriodEndsAt.After(end))
				require.False(t, gone.c.entitled(gone.ent))
				require.Len(t, completed(w.payments(embedded, gone.c.id)), 1)
			}
			release()
			w.runRenewals()
			require.Len(t, kept.providerLedger(), 4, "resolving a finding does not create another renewal")
			require.Empty(t, w.stripe.Unexpected())
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

func TestCustomerCancelsNMIScheduleWithUnresolvedFinding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded)
	w.converge()
	w.refreshProviders()
	end := l.periodEnd()
	w.holdRecovery("nmi")
	status, body := l.meCancel(l.sub)
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "canceled", body["status"])
	require.NotNil(t, body["deletion_scheduled_at"], "the contractual deferred stop stays visible")
	w.advance(end.Add(-47 * time.Hour).Sub(w.clock.Now()))
	w.wake()
	w.until(func() bool { return w.nmi.ScheduleDeletes(l.railSub) == 1 }, "the due provider stop proceeds despite an unrelated finding")
	w.until(func() bool { return w.subscription(embedded, l.sub).DeletionScheduledAt == nil }, "confirmed stop clears its pending marker")
	require.False(t, w.nmi.ScheduleLive(l.railSub))
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, l.sub).Status)
	w.advance(end.Add(time.Second).Sub(w.clock.Now()))
	w.wake()
	require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub), "the provider stop is sent once")
	require.True(t, w.subscription(embedded, l.sub).CurrentPeriodEndsAt.Equal(end))
	require.False(t, l.c.entitled(l.ent))
	require.Empty(t, w.nmi.Unexpected())
}

func TestCustomerCancelsStripeScheduleWithUnresolvedFinding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	w.settleCollectionScans()
	w.holdRecovery("stripe")
	status, body := l.meCancel(l.sub)
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "canceled", body["status"])
	w.until(func() bool { return w.stripe.Subscription(l.railSub)["cancel_at_period_end"] == true }, "unresolved findings do not postpone the provider stop")
	w.until(func() bool { return w.subscription(embedded, l.sub).DeletionScheduledAt == nil }, "the verified stop clears its marker")
	require.Len(t, l.stripeSubWrites(), 1)
	require.True(t, l.c.entitled(l.ent), "the paid period is kept")
	status, body = l.c.call(http.MethodPost, "/subscriptions/"+l.sub.String()+"/resume", "", nil)
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, false, w.stripe.Subscription(l.railSub)["cancel_at_period_end"])
	status, body = l.meCancel(l.sub)
	require.Equal(t, http.StatusOK, status, "%v", body)
	w.until(func() bool { return w.stripe.Subscription(l.railSub)["cancel_at_period_end"] == true }, "the second cancel reaches Stripe")
	require.Equal(t, 2, w.providerCancels("stripe_cancel_subscription", l.sub.UUID()))
	require.Len(t, l.stripeSubWrites(), 3, "cancel, resume, cancel")
	require.Zero(t, l.engineCharges())
}

func TestPaymentProceedsWithUnresolvedFinding(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	method := e.c.saveCard("nmi", visa)
	invoice := observedInvoice(t, w, e.c, 50_000_000)
	w.holdRecovery("nmi")
	before := len(w.nmi.Attempts())
	answer := payNMIInvoice(t.Context(), w, e.c, invoice, method, "unresolved-finding")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusOK, answer.status, string(answer.body))
	require.Len(t, w.nmi.Attempts(), before+1, "an unrelated finding cannot refuse a legitimate payment")
	require.Equal(t, billing.OrderComplete, w.invoice(invoice).Status)
	status, body := e.cancel()
	require.Equal(t, http.StatusOK, status, "%v", body)
}

// A provider charge read after a cancel recorded during recovery is
// `pull.charge.after_cancellation` evidence: it renews nothing and grants no
// access.
func TestProviderChargeAfterCancelDuringRecoveryIsEvidence(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.waive("evidenced", "normalized authenticated late receipt exercises local accounting without issuing another gateway charge")
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded)
	w.refreshProviders()
	w.holdRecovery("nmi")
	status, body := l.meCancel(l.sub)
	require.Equal(t, http.StatusOK, status, "%v", body)
	w.settle()
	before := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionCanceled, before.Status)

	ctx := db.WithPSPID(merchant.WithID(t.Context(), w.client[embedded].MerchantID()), w.psp["nmi"].UUID())
	start := *before.CurrentPeriodEndsAt
	end := start.Add(monthHours * time.Hour)
	priceID, productID, subID, pspID := w.priceStorageID(l.price.ID), l.price.ProductID.UUID(), l.sub.UUID(), w.psp["nmi"].UUID()
	hours := monthHours
	payment := &models.Payment{ID: uuid.New(), CustomerID: uuid.MustParse(l.c.id), SubscriptionID: &subID, Rail: models.RailNMI, PspID: &pspID, TransactionID: "late-recovery-capture", Amount: l.price.UnitAmount, ListAmount: l.price.UnitAmount, Currency: "USD", Status: "succeeded", MoneyMovement: models.MoneyMovementRail}
	bill := observed.Bill{PSPID: pspID, CustomerID: payment.CustomerID, SubscriptionID: &subID, Owner: observed.OwnerNMISchedule, Kind: "renewal", SourceKind: "verified_cycle", SourceKey: "late-cycle", Currency: "USD", Total: l.price.UnitAmount, PeriodStart: start, PeriodEnd: end, DueAt: start, Settled: true, SettlementSource: "cash", Payment: payment, Lines: []observed.Line{{PriceID: &priceID, ProductID: &productID, Description: "Accepted provider renewal", UnitAmount: l.price.UnitAmount, Amount: l.price.UnitAmount, BillingIntervalHours: &hours, AccessDurationHours: &hours}}}
	lifecycle := engine.Graph(w.rt).Runtime.SubscriptionLifecycleService
	require.NoError(t, lifecycle.RenewMembership(ctx, &subscriptions.RenewMembershipParams{ProviderBill: &bill, Rail: models.RailNMI, RailSubscriptionID: l.railSub, TransactionID: payment.TransactionID, Amount: bill.Total, AmountProvided: true, Currency: bill.Currency}))

	after := w.subscription(embedded, l.sub)
	require.Equal(t, billing.SubscriptionCanceled, after.Status)
	require.True(t, before.CurrentPeriodEndsAt.Equal(*after.CurrentPeriodEndsAt), "the charge never advances the canceled period")
	require.Equal(t, []string{"payment:" + payment.ID.String()}, w.openFindings("pull.charge.after_cancellation"))
}
