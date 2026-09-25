//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
)

// #1089: Stripe and CCBill bill the subscriptions they own; OpenRails mirrors
// them. Access follows payment evidence, and revoking access stops provider
// billing.

func (f *stripeFake) subscriptionWritesDown(down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subsDown = down
}

// setStatus moves a Stripe subscription to status; retrying says whether
// Stripe still has a payment attempt scheduled on its open invoice.
func (f *stripeFake) setStatus(subID, status string, retrying bool) obj {
	f.mu.Lock()
	s := f.subs[subID]
	s["status"] = status
	if inv, ok := s["latest_invoice"].(obj); ok && inv["status"] == "open" {
		inv["next_payment_attempt"] = nil
		if retrying {
			inv["next_payment_attempt"] = time.Now().Add(72 * time.Hour).Unix()
		}
	}
	f.mu.Unlock()
	return f.subscriptionObject(subID)
}

func (l *legacy) stripeSubWrites() []providerCall {
	return l.w.stripe.mutations("/v1/subscriptions/" + l.railSub)
}

// A dispute or a refunded charge revokes a Stripe member's access, and the
// same transaction queues the remote cancel: Stripe never bills a member
// who has no access.
func TestStripeRevokeStopsStripeBilling(t *testing.T) {
	t.Parallel()
	t.Run("dispute", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importLegacy(t, w, "stripe", embedded)
		w.converge()
		p := completed(w.payments(embedded, l.c.id))[0]
		for range 2 {
			require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.created", "dp_stop", "needs_response", p)))
		}
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
		require.False(t, l.c.entitled(l.ent))
		remote := w.stripe.subscriptionObject(l.railSub)
		require.Equal(t, true, remote["cancel_at_period_end"], "Stripe does not renew a disputed membership")
		require.Equal(t, "active", remote["status"])
		require.Len(t, l.stripeSubWrites(), 1, "one remote cancel for a redelivered dispute")

		// A won dispute restores the paid period; Stripe still stops at its end.
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.closed", "dp_stop", "won", p)))
		require.Equal(t, "active", w.subscription(embedded, l.sub).Status)
		require.Equal(t, true, w.stripe.subscriptionObject(l.railSub)["cancel_at_period_end"])
		require.Zero(t, l.engineCharges())
	})
	t.Run("dispute_while_delinquent", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importLegacy(t, w, "stripe", embedded)
		w.converge()
		p := completed(w.payments(embedded, l.c.id))[0]
		w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
		require.Equal(t, http.StatusOK, w.deliver("stripe", l.providerRenewal(false)))
		require.Equal(t, "past_due", w.subscription(embedded, l.sub).Status)
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.created", "dp_late", "needs_response", p)))
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
		require.False(t, l.c.entitled(l.ent))
		require.Equal(t, "canceled", w.stripe.subscriptionObject(l.railSub)["status"], "a delinquent subscription ends now, so its open invoice stops retrying")
	})
	t.Run("dashboard_refund", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importLegacy(t, w, "stripe", embedded)
		w.converge()
		w.stripe.dashboardRefund(w.stripe.latestCharge(l.railSub), 999)
		require.Equal(t, http.StatusOK, w.deliver("stripe", w.refundNotice("stripe")))
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
		require.False(t, l.c.entitled(l.ent))
		require.Equal(t, true, w.stripe.subscriptionObject(l.railSub)["cancel_at_period_end"])
		require.Len(t, l.stripeSubWrites(), 1)
	})
	t.Run("stripe_unavailable", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importLegacy(t, w, "stripe", embedded)
		w.converge()
		p := completed(w.payments(embedded, l.c.id))[0]
		w.stripe.subscriptionWritesDown(true)
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeDisputeEvent("charge.dispute.created", "dp_down", "needs_response", p)))
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status, "the revoke never waits on Stripe")
		require.False(t, l.c.entitled(l.ent))
		require.Equal(t, false, w.stripe.subscriptionObject(l.railSub)["cancel_at_period_end"])

		w.stripe.subscriptionWritesDown(false)
		w.until(func() bool { return w.stripe.subscriptionObject(l.railSub)["cancel_at_period_end"] == true }, "the queued cancel reaches Stripe")
		require.Len(t, l.stripeSubWrites(), 1)
	})
}

// Stripe's status is authoritative for the subscriptions it bills: unpaid,
// paused, and a past_due invoice Stripe has stopped retrying grant nothing.
// A past_due subscription Stripe still retries keeps access until Stripe
// gives up.
func TestStripeOwnedAccessFollowsStripe(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, status string
		declined     bool
	}{
		{"unpaid", "unpaid", true},
		{"paused", "paused", false},
		{"retries_exhausted", "past_due", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			l := importLegacy(t, w, "stripe", embedded)
			w.converge()
			w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
			if tc.declined {
				require.Equal(t, http.StatusOK, w.deliver("stripe", l.providerRenewal(false)))
				require.Equal(t, "past_due", w.subscription(embedded, l.sub).Status)
				require.True(t, l.c.entitled(l.ent), "access while Stripe retries")
			}
			require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("customer.subscription.updated", w.stripe.setStatus(l.railSub, tc.status, false))))
			require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
			require.False(t, l.c.entitled(l.ent), "no access without payment")
			require.Zero(t, l.engineCharges())
		})
	}
	t.Run("past_due_until_stripe_gives_up", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		l := importLegacy(t, w, "stripe", embedded)
		w.converge()
		w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
		require.Equal(t, http.StatusOK, w.deliver("stripe", l.providerRenewal(false)))
		w.advance(20 * day)
		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("customer.subscription.updated", w.stripe.setStatus(l.railSub, "past_due", true))))
		require.NotEqual(t, "cancelled", w.subscription(embedded, l.sub).Status)
		require.True(t, l.c.entitled(l.ent), "Stripe is still retrying")

		require.Equal(t, http.StatusOK, w.deliver("stripe", stripeEvent("customer.subscription.updated", w.stripe.setStatus(l.railSub, "past_due", false))))
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
		require.False(t, l.c.entitled(l.ent))
	})
}

// dataLinkFake is CCBill DataLink: an ACTIVEMEMBERS roster and empty
// transaction exports.
type dataLinkFake struct {
	*httptest.Server
	mu      sync.Mutex
	members []string
}

func newDataLinkFake(t *testing.T) *dataLinkFake {
	f := &dataLinkFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Form.Get("transactionTypes") == "ACTIVEMEMBERS" {
			_, _ = io.WriteString(rw, strings.Join(f.members, "\n"))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

// list sets the roster to one active member with CCBill's dates.
func (f *dataLinkFake) list(subscriptionID, rebill, expiry string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members = []string{fmt.Sprintf(`"ACTIVEMEMBERS","945280","x","%s","2020-01-01","member","member@example.test","1","%s","%s"`, subscriptionID, rebill, expiry)}
}

// The DataLink lane restores access only on a paid-through (expiry) date,
// and only once the merchant is armed for enforcement. A member CCBill still
// lists with just a next rebill date is a finding, never free access.
func TestCCBillDataLinkNeedsPaidThrough(t *testing.T) {
	t.Parallel()
	dl := newDataLinkFake(t)
	w := prepareWorld(t, 12, func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{CCBillDataLinkURL: dl.URL}
	})
	w.declare = func(psps map[string]embed.PSPConfig) {
		account := psps["ccbill"]["ccbill"]
		account.Secrets = map[string]string{"salt": "greenfield-ccbill-salt", "datalink_username": "greenfield-datalink", "datalink_password": "greenfield-datalink-password"}
		psps["ccbill"]["ccbill"] = account
	}
	w.start()
	m := importCCBill(t, w)
	w.advance(20 * day)
	w.deliverCCBill("RenewalFailure", map[string]string{
		"subscriptionId": m.railSub, "transactionId": ccbillNumericID(),
		"failureReason": "Insufficient funds", "failureCode": "BE-140",
		"renewalDate": ccbillDate(m.paidThrough), "nextRetryDate": ccbillDate(m.paidThrough.Add(2 * day)),
		"cardType": "VISA", "paymentType": "CREDIT",
	})
	require.Equal(t, "past_due", w.subscription(embedded, m.sub).Status)
	dl.list(m.railSub, ccbillDate(w.clock.Now().Add(2*day)), "")

	// Surveyed but never armed: the pass is advisory and changes nothing.
	_, err := w.pool.Exec(t.Context(), w.q(`UPDATE openrails.destructive_action_switch SET enabled = true, updated_by = 'greenfield'`))
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO openrails.merchant_destructive_policy (merchant_id, destructive_actions_enabled, updated_by, reason)
		SELECT id, true, 'greenfield', 'survey' FROM openrails.merchants WHERE slug = $1`), w.slug)
	require.NoError(t, err)
	w.pull()
	require.Equal(t, "past_due", w.subscription(embedded, m.sub).Status, "an advisory pass reactivates nothing")
	finding := "subscription:" + m.sub.UUID().String()
	require.NotContains(t, w.openFindings("pull.ccbill.active_without_paid_through"), finding)

	w.armDestructive()
	w.pull()
	require.Equal(t, "past_due", w.subscription(embedded, m.sub).Status, "a future rebill date is not payment")
	require.Contains(t, w.openFindings("pull.ccbill.active_without_paid_through"), finding)

	paidThrough := w.clock.Now().Add(25 * day)
	dl.list(m.railSub, "", ccbillDate(paidThrough))
	w.pull()
	sub := w.subscription(embedded, m.sub)
	require.Equal(t, "active", sub.Status, "a paid-through date is evidence")
	require.True(t, sub.CurrentPeriodEndsAt.Equal(endOfDay(paidThrough)), "%v", sub.CurrentPeriodEndsAt)
	require.True(t, m.c.entitled(m.ent))
	require.Zero(t, w.engineCharges())
}

// A CCBill RenewalFailure racing a chargeback reads the row under its lock,
// so it can never write back the pre-chargeback state.
func TestCCBillRenewalFailureCannotUndoAChargeback(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	m := importCCBill(t, w)
	w.advance(20 * day)
	ctx := t.Context()

	tx, err := w.pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	_, err = tx.Exec(ctx, w.q(`SELECT 1 FROM openrails.subscriptions WHERE id = $1 FOR UPDATE`), m.sub.UUID())
	require.NoError(t, err)

	form := url.Values{"clientAccnum": {"945280"}, "clientSubacc": {"0000"}, "timestamp": {ccbillTimestamp(w.clock.Now())},
		"subscriptionId": {m.railSub}, "transactionId": {ccbillNumericID()}, "failureReason": {"Insufficient funds"}, "failureCode": {"BE-140"},
		"renewalDate": {ccbillDate(m.paidThrough)}, "nextRetryDate": {ccbillDate(m.paidThrough.Add(2 * day))}, "cardType": {"VISA"}, "paymentType": {"CREDIT"}}
	done := make(chan int, 1)
	go func() {
		query := url.Values{"eventType": {"RenewalFailure"}, "eventGroupType": {"Subscription"}}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, w.server.URL+mountPrefix+"/v1/webhooks/ccbill/"+ccbillAcct+"?"+query.Encode(), strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Forwarded-For", ccbillSourceIP)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- 0
			return
		}
		_ = res.Body.Close()
		done <- res.StatusCode
	}()
	require.Eventually(t, func() bool {
		var waiting int
		err := w.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND datname = current_database() AND query LIKE '%' || $1 || '%subscriptions%'`, w.schema).Scan(&waiting)
		return err == nil && waiting > 0
	}, 20*time.Second, 10*time.Millisecond, "the renewal failure reaches the row lock")

	// The chargeback commits while the renewal failure waits.
	_, err = tx.Exec(ctx, w.q(`UPDATE openrails.subscriptions SET status = 'cancelled', cancel_type = 'chargeback', cancelled_at = now(), ended_at = now(), next_retry_at = NULL, grace_ends_at = NULL WHERE id = $1`), m.sub.UUID())
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	require.Equal(t, http.StatusOK, <-done)
	w.settle()

	sub := w.subscription(embedded, m.sub)
	require.Equal(t, "cancelled", sub.Status, "the chargeback stands")
	require.NotNil(t, sub.CancelType)
	require.Equal(t, "chargeback", *sub.CancelType)
}
