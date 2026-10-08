//go:build e2e && integration && audit1137

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// These opt-in audit probes record current behavior; they are not acceptance
// tests declaring that every observed behavior is desirable. See tracker1137.
func TestAudit1137PendingProviderStop(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "active_subscription_control"
		if cancel {
			name = "provider_stop_pending"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.armDestructive()
			l := importLegacy(t, w, "nmi", remote)
			w.converge()
			end := l.periodEnd()
			w.advanceHealthyTo(end.Add(-time.Hour))
			method := l.c.saveCard("nmi", visa)
			before := len(w.nmi.Ledger(""))
			if cancel {
				w.nmi.FailRequests(func(r *http.Request) bool {
					if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "subscriptions") {
						return true
					}
					if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "transact.php") {
						return false
					}
					_ = r.ParseForm()
					return r.Form.Get("recurring") == "delete_subscription"
				}, http.StatusServiceUnavailable, 100)
				status, body := l.meCancel(l.sub)
				require.Equal(t, http.StatusOK, status, "%v", body)
				w.settle()
				require.Equal(t, billing.SubscriptionCanceled, w.subscription(remote, l.sub).Status)
				require.True(t, w.nmi.ScheduleLive(l.railSub), "the provider stop has not succeeded")
			}
			attempt, err := w.client[remote].CreateCheckoutAttempt(t.Context(), billing.CreateCheckoutAttemptParams{
				OfferKind: billing.OfferRecurring, Customer: l.c.identity(), Entitlement: l.ent, PriceID: l.price.ID,
				IdempotencyKey: "audit-enroll-" + uuid.NewString(), PaymentOptions: billing.CheckoutPaymentOptions{PSP: "nmi", PaymentMethodID: pmid(method)},
				SuccessURL: "https://e2e.test/return", CancelURL: "https://e2e.test/return?canceled=1",
			})
			if !cancel {
				require.Error(t, err, "a currently active membership must hold the product slot")
				require.Len(t, w.nmi.Ledger(""), before)
				t.Log("OBSERVATION: active membership blocks another enrollment before provider charge")
				return
			}
			require.NoError(t, err)
			require.Equal(t, billing.CheckoutAttemptSucceeded, attempt.Status)
			require.NotNil(t, attempt.SubscriptionID)
			require.NotEqual(t, l.sub, *attempt.SubscriptionID)
			require.Len(t, w.nmi.Ledger(""), before+1, "replacement enrollment collected immediately")
			require.True(t, w.nmi.ScheduleLive(l.railSub))
			newSub := w.subscription(remote, *attempt.SubscriptionID)
			w.advance(end.Add(time.Second).Sub(w.clock.Now()))
			oldRebill := w.nmi.RenewSchedule(l.railSub, true)
			require.True(t, oldRebill.Approved())
			require.Len(t, w.nmi.Ledger(""), before+2, "old provider schedule and replacement both collected")
			require.True(t, newSub.CurrentPeriodStartsAt.Before(end))
			require.True(t, newSub.CurrentPeriodEndsAt.After(end))
			require.Equal(t, 0, w.nmi.ScheduleDeletes(l.railSub))
			t.Logf("OBSERVATION: same customer/product has replacement payment plus old scheduled rebill; overlapping period at %s; old_schedule_live=true", end.Format(time.RFC3339))
		})
	}
}

func TestAudit1137OffChannelPurchaseAndRenewal(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "normal_retry_control"
		if manual {
			name = "off_channel_purchase"
		}
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			e := enroll(t, w, "nmi", remote)
			end := e.periodEnd()
			e.refreshBeforePeriodEnd()
			e.setDecline(visa.Last4, "insufficient_funds", "202")
			e.toPeriodEnd()
			w.runRenewals()
			before := w.subscription(remote, e.sub)
			require.Equal(t, billing.SubscriptionPastDue, before.Status)
			require.NotNil(t, before.NextRetryAt)
			require.Equal(t, 2, e.providerAttempts())
			require.Len(t, e.providerLedger(), 1)
			payments := 2
			if manual {
				params := billing.CreateOffChannelPaymentParams{PriceID: pid(e.price), TransactionID: "external-" + uuid.NewString()}
				payment, err := w.client[remote].CreateOffChannelPayment(t.Context(), e.c.cid(), params)
				require.NoError(t, err)
				require.Equal(t, "manual", payment.Channel)
				require.Nil(t, payment.SubscriptionID, "off-channel API does not identify a subscription period")
				replay, err := w.client[remote].CreateOffChannelPayment(t.Context(), e.c.cid(), params)
				require.NoError(t, err)
				require.Equal(t, payment.ID, replay.ID, "same remittance is recorded once")
				after := w.subscription(remote, e.sub)
				require.Equal(t, before.Status, after.Status)
				require.Equal(t, before.CurrentPeriodEndsAt, after.CurrentPeriodEndsAt)
				require.Equal(t, before.NextRetryAt, after.NextRetryAt)
				payments++
			}
			e.setDecline(visa.Last4, "", "")
			w.advanceHealthyTo(before.NextRetryAt.Add(time.Second))
			w.runRenewals()
			require.Equal(t, billing.SubscriptionActive, w.subscription(remote, e.sub).Status)
			require.True(t, e.periodEnd().Equal(end.Add(monthHours*time.Hour)))
			require.Equal(t, 3, e.providerAttempts())
			require.Len(t, e.providerLedger(), 2)
			require.Len(t, completed(w.payments(remote, e.c.id)), payments)
			w.runRenewals()
			require.Len(t, e.providerLedger(), 2, "repeat due pass cannot charge twice")
			t.Logf("OBSERVATION: off_channel=%t; native renewal settled once; successful_rail_payments=2; total_local_payments=%d", manual, payments)
		})
	}
}
