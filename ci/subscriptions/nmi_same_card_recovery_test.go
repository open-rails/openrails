//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/intents"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func missingRecurringAgreement(t *testing.T, w *world) *legacy {
	t.Helper()
	l := importLegacy(t, w, "nmi", embedded)
	awaitMissingRecurringAgreement(t, w, l)
	return l
}

func awaitMissingRecurringAgreement(t *testing.T, w *world, l *legacy) {
	t.Helper()
	// An older imported subscription lacks a retained recurring agreement.
	_, err := w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.mandates WHERE subscription_id=$1`), l.sub.UUID())
	require.NoError(t, err)
	w.converge()
	w.advanceHealthyTo(l.periodEnd().Add(time.Hour))
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
	w.settle()
	w.advanceHealthyTo(nextRetry(w.subscription(embedded, l.sub)).Add(time.Second))
	w.runRenewals()
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, l.sub).Status)
	require.Empty(t, w.nmi.Attempts())
}

func missingRecurringAgreementOnDefaultCard(t *testing.T, w *world) *legacy {
	t.Helper()
	l := importLegacy(t, w, "nmi", embedded)
	method := l.c.saveCard("nmi", visa)
	l.c.setDefault(method)
	l.c.setOwnCard(l.sub, method)
	l.railCust = w.vaultOf(method)
	awaitMissingRecurringAgreement(t, w, l)
	return l
}

func confirmCurrentCard(l *legacy, key string) (int, map[string]any) {
	method := l.w.subscription(embedded, l.sub).PaymentMethodID.String()
	return l.c.call(http.MethodPut, "/subscriptions/"+l.sub.String()+"/payment-method", key, map[string]any{"payment_method_id": method})
}

func assertConfirmationCount(t *testing.T, w *world, l *legacy, want int) {
	t.Helper()
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='nmi_recurring_confirmation'`), l.sub.UUID()).Scan(&count))
	require.Equal(t, want, count)
}

func TestNMISameCardRecoveryConcurrentClicks(t *testing.T) {
	w := newWorld(t)
	l := missingRecurringAgreement(t, w)
	before := len(w.nmi.Validations(l.railCust))
	g := w.nmi.hold(newGate(func(r *http.Request) bool {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		return strings.Contains(string(body), "type=validate")
	}, true))
	first := make(chan int, 1)
	go func() { status, _ := confirmCurrentCard(l, "first-click"); first <- status }()
	select {
	case <-g.arrived:
	case <-time.After(10 * time.Second):
		t.Fatal("verification did not reach provider")
	}
	for i := range 3 {
		status, body := confirmCurrentCard(l, fmt.Sprintf("another-click-%d", i))
		require.Equal(t, http.StatusConflict, status, "%v", body)
	}
	assertConfirmationCount(t, w, l, 1)
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, l.sub).Status)
	close(g.release)
	require.Equal(t, http.StatusOK, <-first)
	w.nmi.unhold()
	require.Len(t, w.nmi.Validations(l.railCust), before+1)
	require.Empty(t, w.nmi.Attempts(), "confirmation does not collect")
	require.Equal(t, billing.SubscriptionPastDue, w.subscription(embedded, l.sub).Status)
	for range 2 {
		status, body := confirmCurrentCard(l, "after-success")
		require.Equal(t, http.StatusOK, status, "%v", body)
	}
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
	require.Len(t, w.nmi.Attempts(), 1)
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), 1)
	assertConfirmationCount(t, w, l, 1)
}

func TestNMISameCardRecoveryLostVerificationResponse(t *testing.T) {
	w := newWorld(t)
	l := missingRecurringAgreement(t, w)
	before := len(w.nmi.Validations(l.railCust))
	w.nmi.DropValidationResponses(1)
	w.nmi.HideValidations(true)
	for i := range 3 {
		status, body := confirmCurrentCard(l, fmt.Sprintf("click-%d", i))
		require.Equal(t, http.StatusConflict, status, "%v", body)
	}
	require.Len(t, w.nmi.Validations(l.railCust), before+1)
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, l.sub).Status)
	require.Empty(t, w.nmi.Attempts())
	own := unwrap(l.c.must(http.MethodGet, "/subscriptions/"+l.sub.String(), "", nil))
	recovery := own["recovery"].(map[string]any)
	require.Equal(t, false, recovery["retryable"])
	require.Equal(t, "payment_in_progress", recovery["blocked_reason"])
	assertConfirmationCount(t, w, l, 1)
	w.restart()
	w.nmi.HideValidations(false)
	w.advance(2 * time.Minute)
	w.wake()
	status, body := confirmCurrentCard(l, "after-restart")
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Len(t, w.nmi.Validations(l.railCust), before+1, "query recovery never resubmits validate")
	require.Empty(t, w.nmi.Attempts())
	require.Equal(t, billing.SubscriptionPastDue, w.subscription(embedded, l.sub).Status)
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
	require.Len(t, w.nmi.Attempts(), 1)
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), 1)
}

func TestNMISameCardRecoveryDeclineStaysHeld(t *testing.T) {
	w := newWorld(t)
	l := missingRecurringAgreement(t, w)
	before := len(w.nmi.Validations(l.railCust))
	w.nmi.DeclineValidations(1)
	for i := range 2 {
		status, _ := confirmCurrentCard(l, fmt.Sprintf("declined-%d", i))
		require.Equal(t, http.StatusPaymentRequired, status)
	}
	require.Len(t, w.nmi.Validations(l.railCust), before+1)
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, l.sub).Status)
	require.Empty(t, w.nmi.Attempts())
	assertConfirmationCount(t, w, l, 1)
}

func TestNMISameCardRecoverySelectsDefaultCardExplicitly(t *testing.T) {
	w := newWorld(t)
	l := missingRecurringAgreementOnDefaultCard(t, w)
	method := w.subscription(embedded, l.sub).PaymentMethodID.String()
	l.c.setOwnCard(l.sub, "")
	own, _ := w.ownCard(embedded, l.sub)
	require.Empty(t, own)
	before := len(w.nmi.Validations(l.railCust))
	w.nmi.DropValidationResponses(1)
	w.nmi.HideValidations(true)
	for _, key := range []string{"accepted", "repeated"} {
		status, body := l.c.call(http.MethodPut, "/subscriptions/"+l.sub.String()+"/payment-method", key, map[string]any{"payment_method_id": method})
		require.Equal(t, http.StatusConflict, status, "%v", body)
	}
	own, _ = w.ownCard(embedded, l.sub)
	require.Empty(t, own, "uncertain confirmation does not change selection")
	w.nmi.HideValidations(false)
	status, body := l.c.call(http.MethodPut, "/subscriptions/"+l.sub.String()+"/payment-method", "resolved", map[string]any{"payment_method_id": method})
	require.Equal(t, http.StatusOK, status, "%v", body)
	own, _ = w.ownCard(embedded, l.sub)
	require.Equal(t, method, own, "explicit selection makes this the subscription's own card")
	require.Len(t, w.nmi.Validations(l.railCust), before+1)
	assertConfirmationCount(t, w, l, 1)
	l.c.setDefault(l.c.saveCard("nmi", mastercard))
	w.settle()
	own, _ = w.ownCard(embedded, l.sub)
	require.Equal(t, method, own)
	require.Equal(t, l.railCust, w.nmi.Schedule(l.railSub).Vault, "later default changes do not move the confirmed subscription")
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status)
	require.Len(t, w.nmi.Attempts(), 1)
}

func TestNMISameCardRecoveryChangedWhileVerifying(t *testing.T) {
	for _, change := range []string{"cancel", "card", "version", "follow-default"} {
		t.Run(change, func(t *testing.T) {
			w := newWorld(t)
			setup := missingRecurringAgreement
			if change == "follow-default" {
				setup = missingRecurringAgreementOnDefaultCard
			}
			l := setup(t, w)
			original := w.subscription(embedded, l.sub).PaymentMethodID.String()
			w.nmi.DropValidationResponses(1)
			w.nmi.HideValidations(true)
			status, _ := confirmCurrentCard(l, "accepted")
			require.Equal(t, http.StatusConflict, status)
			if change == "cancel" {
				w.armDestructive()
				_, err := w.client[embedded].CancelSubscription(t.Context(), l.sub, billing.CancelSubscriptionParams{Reason: "customer canceled"})
				require.NoError(t, err)
			} else if change == "version" {
				l.c.must(http.MethodPatch, "/payment-methods/"+original, "", map[string]any{"exp_month": 9, "exp_year": 2031})
			} else if change == "follow-default" {
				l.c.setOwnCard(l.sub, "")
			} else {
				replacement := l.c.saveCard("nmi", mastercard)
				l.c.setOwnCard(l.sub, replacement)
			}
			before := w.subscription(embedded, l.sub)
			w.nmi.HideValidations(false)
			w.advance(2 * time.Minute)
			w.wake()
			after := w.subscription(embedded, l.sub)
			require.Equal(t, before.Status, after.Status)
			require.Equal(t, before.PaymentMethodID, after.PaymentMethodID)
			recurring, _ := w.agreements(original)
			require.Empty(t, recurring)
			var terminal string
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT status FROM billing.provider_intents WHERE subscription_id=$1 AND intent_type='nmi_recurring_confirmation'`), l.sub.UUID()).Scan(&terminal))
			require.Equal(t, "failed_terminal", terminal)
			require.Empty(t, w.nmi.Attempts())
		})
	}
}

func TestNMISameCardRecoveryRetainsAnswerAndAttribution(t *testing.T) {
	w := newWorld(t)
	l := missingRecurringAgreement(t, w)
	before := len(w.nmi.Validations(l.railCust))
	remove := failpoint.Set(func(_ context.Context, s failpoint.Site) error {
		if s.Kind == intents.TypeNMIRecurringConfirmation && s.Point == failpoint.BeforeComplete {
			return errors.New("completion interrupted")
		}
		return nil
	})
	t.Cleanup(remove)
	status, body := confirmCurrentCard(l, "accepted")
	require.Equal(t, http.StatusConflict, status, "%v", body)
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, l.sub).Status)
	recurring, _ := w.agreements(w.subscription(embedded, l.sub).PaymentMethodID.String())
	require.Empty(t, recurring)
	remove()
	w.nmi.QueryUnavailable(true)
	status, body = confirmCurrentCard(l, "resume-local")
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Len(t, w.nmi.Validations(l.railCust), before+1)
	var count int
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.subscription_status_transitions t JOIN billing.provider_intents i ON i.id=t.operation_id AND i.merchant_id=t.merchant_id WHERE t.subscription_id=$1 AND t.decision='recurring_agreement_restored' AND t.actor=$2 AND t.origin='user' AND i.status='succeeded'`), l.sub.UUID(), l.c.id).Scan(&count))
	require.Equal(t, 1, count)
}

func TestNMISameCardRecoveryHonorsWriteModes(t *testing.T) {
	w := newWorld(t)
	l := missingRecurringAgreement(t, w)
	before := len(w.nmi.Validations(l.railCust))
	w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeReadOnly }
	w.restart()
	status, _ := confirmCurrentCard(l, "read-only")
	require.Equal(t, http.StatusConflict, status)
	require.Len(t, w.nmi.Validations(l.railCust), before)
	w.cfg = func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeLimited }
	w.restart()
	status, body := confirmCurrentCard(l, "customer-in-limited")
	require.Equal(t, http.StatusOK, status, "%v", body)
	require.Len(t, w.nmi.Validations(l.railCust), before+1)
	w.runRenewals()
	require.Empty(t, w.nmi.Attempts(), "limited mode still pauses automatic collection")
}

func TestNMISameCardRecoveryRequiresOwner(t *testing.T) {
	w := newWorld(t)
	l := missingRecurringAgreement(t, w)
	stranger := w.newCustomer()
	status, _ := stranger.call(http.MethodPut, "/subscriptions/"+l.sub.String()+"/payment-method", "unauthorized", map[string]any{"payment_method_id": w.subscription(embedded, l.sub).PaymentMethodID.String()})
	require.Equal(t, http.StatusNotFound, status)
	other := w.rival()
	_, err := other.client.UpdateCustomer(t.Context(), l.c.cid(), billing.UpdateCustomerParams{})
	require.NoError(t, err)
	status, body := w.callAt(other.server.URL, other.auth.token(t, l.c.id), http.MethodPut, "/subscriptions/"+l.sub.String()+"/payment-method", "cross-merchant", map[string]any{"payment_method_id": w.subscription(embedded, l.sub).PaymentMethodID.String()})
	require.Equal(t, http.StatusNotFound, status, "%v", body)
	assertConfirmationCount(t, w, l, 0)
}

func TestNMISameCardRecoveryDoesNotReauthorizeEndedConsent(t *testing.T) {
	for _, state := range []string{"requires_reconsent", "revoked"} {
		t.Run(state, func(t *testing.T) {
			w := newWorld(t)
			l := missingRecurringAgreement(t, w)
			var reason *string
			var ended *time.Time
			if state == "revoked" {
				r := "customer_revoked"
				reason = &r
				now := w.clock.Now()
				ended = &now
			}
			_, err := w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.mandates (merchant_id,customer_id,payment_method_id,psp_id,rail,kind,subscription_id,status,end_reason,ended_at,accepted_at) SELECT merchant_id,customer_id,payment_method_id,psp_id,rail,'recurring',id,$2,$3,$4,created_at FROM billing.subscriptions WHERE id=$1`), l.sub.UUID(), state, reason, ended)
			require.NoError(t, err)
			before := len(w.nmi.Validations(l.railCust))
			status, body := confirmCurrentCard(l, "no-reauthorization")
			require.Equal(t, http.StatusOK, status, "%v", body)
			require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, l.sub).Status)
			require.Len(t, w.nmi.Validations(l.railCust), before)
			assertConfirmationCount(t, w, l, 0)
		})
	}
}
