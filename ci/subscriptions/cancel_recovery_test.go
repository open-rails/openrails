//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/providerrecovery"
)

// A customer can always cancel. Provider recovery holds the writes that move
// money or reach a provider, never the cancel itself: it is recorded at once,
// and a provider-billed schedule's stop waits as a durable provider intent.

// holdRecovery opens a financial finding on the rail's PSP, so provider
// recovery holds its writes; release resolves it.
func (w *world) holdRecovery(rail string) (release func()) {
	w.t.Helper()
	subject := "held-" + uuid.NewString()[:8]
	_, err := w.pool.Exec(w.t.Context(), w.q(`INSERT INTO billing.reconciliation_findings (merchant_id, finding_type, subject_key, severity, status, psp_id, rail, source)
		SELECT id, 'pull.charge.missing', $2, 'high', 'reconcile_required', $3, $4, 'pull' FROM billing.merchants WHERE slug = $1`), w.slug, subject, w.psp[rail].UUID(), rail)
	require.NoError(w.t, err)
	require.ErrorIs(w.t, w.recoveryCheck(rail), providerrecovery.ErrPending)
	return func() {
		w.t.Helper()
		_, err := w.pool.Exec(w.t.Context(), w.q(`UPDATE billing.reconciliation_findings SET status = 'fixed', resolution = 'admin_fixed', resolved_at = now() WHERE subject_key = $1`), subject)
		require.NoError(w.t, err)
	}
}

// recoveryCheck is the provider recovery gate of the rail's PSP now.
func (w *world) recoveryCheck(rail string) error {
	w.t.Helper()
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(w.t, err)
	return providerrecovery.CheckPSP(w.t.Context(), database, w.client[embedded].MerchantID().UUID(), w.psp[rail].UUID(), w.clock.Now())
}

// providerIntent is the status of a subscription's one intent of a type and
// whether provider recovery holds it.
func (w *world) providerIntent(intentType string, sub billing.SubscriptionID) (status string, held bool) {
	w.t.Helper()
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT status, coalesce((result_evidence->>'recovery_held')::boolean, false)
		FROM billing.provider_intents WHERE intent_type = $1 AND subscription_id = $2`), intentType, sub.UUID()).Scan(&status, &held))
	return status, held
}

func (e *engineCase) cancel() (int, map[string]any) {
	return e.c.call(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"reason": "moving on"})
}

// Engine memberships on a PSP held by provider recovery: one is canceled
// before its renewal is due, one after recovery held that renewal. Both answer
// 200 at once and are never renewed, while the third member's renewal waits
// for recovery and is then charged once.
func TestCustomerCancelsEngineMembershipDuringProviderRecovery(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			early, late, kept := enroll(t, w, rail, embedded), enroll(t, w, rail, embedded), enroll(t, w, rail, embedded)
			release := w.holdRecovery(rail)

			status, body := early.cancel()
			require.Equal(t, http.StatusOK, status, "%v", body)
			require.Equal(t, "canceled", body["status"])
			require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, early.sub).Status)

			end := kept.periodEnd()
			w.advance(end.Add(time.Second).Sub(w.clock.Now()))
			w.runRenewals()
			require.Len(t, kept.providerLedger(), 3, "recovery holds every renewal: only the three initial charges")

			status, body = late.cancel()
			require.Equal(t, http.StatusOK, status, "a held renewal that was never sent does not trap the cancel: %v", body)
			require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, late.sub).Status)

			release()
			w.refreshProviders() // the month without observation is caught up too
			w.runRenewals()
			w.wake()
			require.Len(t, kept.providerLedger(), 4, "after recovery only the kept member renews")
			require.True(t, kept.periodEnd().After(end))
			require.True(t, kept.c.entitled(kept.ent))
			for _, gone := range []*engineCase{early, late} {
				sub := w.subscription(embedded, gone.sub)
				require.Equal(t, billing.SubscriptionCanceled, sub.Status)
				require.False(t, sub.CurrentPeriodEndsAt.After(end), "a canceled membership is never renewed")
				require.False(t, gone.c.entitled(gone.ent))
				require.Len(t, completed(w.payments(embedded, gone.c.id)), 1)
			}
			require.Empty(t, w.stripe.Unexpected())
			require.Empty(t, w.nmi.Unexpected())
		})
	}
}

// An NMI schedule canceled while recovery holds its PSP: the cancel is
// recorded at once and shows its provider stop pending. The delete waits for
// recovery and is sent exactly once after it. NMI's charge of the still-live
// schedule after the recorded cancel is a charge after cancellation: it renews
// nothing and is refunded once recovery lets money move.
func TestCustomerCancelsNMIScheduleDuringProviderRecovery(t *testing.T) {
	t.Parallel()
	{
		{
			w := newWorld(t)
			w.armDestructive()
			l := importLegacy(t, w, "nmi", embedded)
			w.converge()
			w.refreshProviders()
			end := l.periodEnd()
			release := w.holdRecovery("nmi")

			status, body := l.meCancel(l.sub)
			require.Equal(t, http.StatusOK, status, "%v", body)
			require.Equal(t, "canceled", body["status"])
			require.NotNil(t, body["deletion_scheduled_at"], "the answer shows the provider stop pending")
			w.settle()

			// The delete falls due before NMI's next billing date and is held.
			w.advance(end.Add(-47 * time.Hour).Sub(w.clock.Now()))
			w.wake()
			intent, held := w.providerIntent("nmi_delete_subscription", l.sub)
			require.Equal(t, "pending", intent)
			require.True(t, held, "the delete waits for provider recovery")
			require.Zero(t, w.nmi.ScheduleDeletes(l.railSub))
			sub := w.subscription(embedded, l.sub)
			require.Equal(t, billing.SubscriptionCanceled, sub.Status)
			require.NotNil(t, sub.DeletionScheduledAt, "canceled, provider stop pending")

			w.advance(end.Add(time.Second).Sub(w.clock.Now()))
			renewal := l.providerRenewal(true)
			charged := w.nmi.Ledger("")
			late := charged[len(charged)-1]
			require.Equal(t, http.StatusOK, w.deliver("nmi", renewal))
			w.settle()
			require.Len(t, w.openFindings(chargeAfterCancel), 1, "the charge after the recorded cancel is evidence for its refund")
			require.Zero(t, w.nmi.Ledger("")[len(charged)-1].RefundedCents, "the refund moves money, so it waits for recovery too")

			release()
			w.refreshProviders() // the weeks without observation are caught up too
			w.until(func() bool { return w.nmi.ScheduleDeletes(l.railSub) > 0 }, "the held delete is sent once recovery clears")
			w.advance(time.Hour)
			w.wake()
			require.Equal(t, 1, w.nmi.ScheduleDeletes(l.railSub), "exactly one delete")
			require.False(t, w.nmi.ScheduleLive(l.railSub))
			w.until(func() bool { return w.subscription(embedded, l.sub).DeletionScheduledAt == nil }, "the verified stop clears the pending marker")
			sub = w.subscription(embedded, l.sub)
			require.Equal(t, billing.SubscriptionCanceled, sub.Status)
			require.True(t, sub.CurrentPeriodEndsAt.Equal(end), "the provider's charge after the cancel renews nothing")
			w.until(func() bool {
				for _, e := range w.nmi.Ledger("") {
					if e.TransactionID == late.TransactionID {
						return e.RefundedCents == e.Cents
					}
				}
				return false
			}, "the charge after cancellation is refunded in full")
			require.Empty(t, w.nmi.Unexpected())
		}
	}
}

// A Stripe-run membership canceled while recovery holds its PSP: the cancel is
// recorded at once and Stripe is told after recovery. A resume and a second
// cancel is a second remote cancel.
func TestCustomerCancelsStripeScheduleDuringProviderRecovery(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "stripe", embedded)
	w.converge()
	w.refreshProviders()
	w.settleCollectionScans()
	release := w.holdRecovery("stripe")

	status, body := l.meCancel(l.sub)
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Equal(t, "canceled", body["status"])
	w.settle()
	w.wake()
	intent, held := w.providerIntent("stripe_cancel_subscription", l.sub)
	require.Equal(t, "pending", intent)
	require.True(t, held)
	require.NotNil(t, w.subscription(embedded, l.sub).DeletionScheduledAt, "canceled, provider stop pending")
	require.Equal(t, false, w.stripe.Subscription(l.railSub)["cancel_at_period_end"])
	require.Empty(t, l.stripeSubWrites())

	release()
	w.until(func() bool { return w.stripe.Subscription(l.railSub)["cancel_at_period_end"] == true }, "the held cancel reaches Stripe")
	w.until(func() bool { return w.subscription(embedded, l.sub).DeletionScheduledAt == nil }, "the verified stop clears the pending marker")
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

// Recovery pauses every other write that moves money: a payment is refused
// with a retry, nothing reaches the provider, while the same customer's cancel
// goes through.
func TestPaymentStaysRefusedDuringProviderRecovery(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	method := e.c.saveCard("nmi", visa)
	invoice := observedInvoice(t, w, e.c, 50_000_000)
	w.holdRecovery("nmi")
	before := len(w.nmi.Attempts())

	answer := payNMIInvoice(t.Context(), w, e.c, invoice, method, "during-recovery")
	require.NoError(t, answer.err)
	require.Equal(t, http.StatusServiceUnavailable, answer.status, string(answer.body))
	require.Len(t, w.nmi.Attempts(), before, "no charge is sent while recovery is pending")

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
