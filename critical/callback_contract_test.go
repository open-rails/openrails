//go:build integration

package critical_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// These bytes/signatures are produced on the external provider side, without
// calling OpenRails' signature parser or settlement code to build the oracle.
func stripeNotice(t *testing.T, id, kind string, object stripemock.Object) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"id": id, "object": "event", "type": kind, "created": time.Now().Unix(), "livemode": false, "data": map[string]any{"object": object}})
	require.NoError(t, err)
	return body
}

func stripeSignature(body []byte, secret string) string {
	at := fmt.Sprint(time.Now().Unix())
	h := hmac.New(sha256.New, []byte(secret))
	_, _ = h.Write([]byte(at + "."))
	_, _ = h.Write(body)
	return "t=" + at + ",v1=" + hex.EncodeToString(h.Sum(nil))
}

func callback(t *testing.T, a *authorityWorld, account string, body []byte, signature string) (int, string) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/v1/webhooks/stripe/"+account, bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	if signature != "" {
		r.Header.Set("Stripe-Signature", signature)
	}
	response, err := a.server.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(raw)
}

func unknownSale(t *testing.T, a *authorityWorld, p *providerLedger) (*river.Client[pgx.Tx], billing.Order, stripemock.Object) {
	t.Helper()
	fleet := hostFleet(t, a.world)
	input := saleInput(t, a, p)
	p.interruptPaymentResponses()
	_, _ = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "accepted-unknown-sale"})
	orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
	require.NoError(t, err)
	require.Len(t, orders.Items, 1)
	order := orders.Items[0]
	require.Equal(t, billing.OrderPaymentProcessing, order.Payment.Status)
	require.NotEqual(t, billing.OrderComplete, order.Status)
	effects := p.effects(t)
	require.Len(t, effects, 1)
	require.EqualValues(t, 3000, effects[0].Amount)
	p.restorePaymentResponses()
	return fleet, order, p.core.PaymentIntent(effects[0].Operation)
}

func recoverSale(t *testing.T, a *authorityWorld, p *providerLedger, fleet *river.Client[pgx.Tx], id billing.OrderID) *billing.Order {
	t.Helper()
	a.clock.Advance(20 * time.Minute)
	jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
	require.NoError(t, err)
	require.NotEmpty(t, jobs.Jobs)
	for _, row := range jobs.Jobs {
		if row.State == rivertype.JobStateScheduled || row.State == rivertype.JobStateRetryable {
			_, err := fleet.JobRetry(t.Context(), row.ID)
			require.NoError(t, err)
		}
	}
	require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
	require.NoError(t, fleet.Start(t.Context()))
	return settledSale(t, a, p, id)
}

func settledSale(t *testing.T, a *authorityWorld, p *providerLedger, id billing.OrderID) *billing.Order {
	t.Helper()
	var order *billing.Order
	require.Eventually(t, func() bool {
		var err error
		order, err = a.alpha.GetOrder(t.Context(), id)
		return err == nil && order.Status == billing.OrderComplete
	}, 20*time.Second, 20*time.Millisecond)
	require.NotNil(t, order.Payment.PaymentID)
	payment, err := a.alpha.GetPayment(t.Context(), *order.Payment.PaymentID)
	require.NoError(t, err)
	require.EqualValues(t, 30_000_000, payment.Amount)
	require.Equal(t, billing.PaymentSucceeded, payment.Status)
	events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, events.Events, 2)
	require.Len(t, p.effects(t), 1)
	require.EqualValues(t, 3000, p.effects(t)[0].Amount)
	return order
}

// A signed callback must itself commit a new wakeup. Starting the real host
// workers at the unchanged business clock then proves that the callback
// released verification; no manual retry or fallback sweep can make it pass.
func callbackSettlesSale(t *testing.T, a *authorityWorld, p *providerLedger, queue *river.Client[pgx.Tx], order billing.Order, deliver func()) *billing.Order {
	t.Helper()
	before, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
	require.NoError(t, err)
	at := a.clock.Now()
	deliver()
	after, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
	require.NoError(t, err)
	require.Len(t, after.Jobs, len(before.Jobs)+1, "the signed notice must durably wake the original accepted operation")
	require.Equal(t, at, a.clock.Now())
	require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(queue)))
	require.NoError(t, queue.Start(t.Context()))
	return settledSale(t, a, p, order.ID)
}

func TestCallbackContracts(t *testing.T) {
	t.Run("OR-J28-S05/callback-during-fetch-keeps-a-follow-up-until-new-evidence-is-read", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
		queue := hostFleet(t, a.world)
		// The PSP's onboarding pull also reads the transaction report: it is
		// not under test, so its queued job is cancelled before the hold.
		cancelOnboarding(t, queue)
		gate := p.core.Hold(func(r *http.Request) bool {
			if !strings.HasSuffix(r.URL.Path, "/query.php") {
				return false
			}
			_ = r.ParseForm()
			return r.Form.Get("report_type") == "transaction"
		}, nmimock.HoldResponse)
		defer gate.Release()
		notice := func(id string) {
			body, err := json.Marshal(map[string]any{"event_id": id, "event_type": "recurring.subscription.update", "event_body": map[string]any{"subscription_id": *sub.RailSubscriptionID, "merchant": map[string]string{"id": "critical-gateway"}}})
			require.NoError(t, err)
			at := fmt.Sprint(time.Now().Unix())
			h := hmac.New(sha256.New, []byte("critical-nmi-webhook"))
			_, _ = h.Write([]byte(at + "."))
			_, _ = h.Write(body)
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, a.server.URL+"/v1/webhooks/nmi/critical-gateway", bytes.NewReader(body))
			require.NoError(t, err)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Webhook-Signature", "t="+at+",s="+hex.EncodeToString(h.Sum(nil)))
			response, err := a.server.Client().Do(r)
			require.NoError(t, err)
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, string(raw))
		}
		notice("evt_before_fetch")
		jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.subscription_converge"))
		require.NoError(t, err)
		require.Len(t, jobs.Jobs, 1)
		first := jobs.Jobs[0]
		_, err = queue.JobRetry(t.Context(), first.ID)
		require.NoError(t, err)
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(queue)))
		require.NoError(t, queue.Start(t.Context()))
		select {
		case <-gate.Arrived():
		case <-time.After(20 * time.Second):
			t.Fatal("first fetch never captured its provider snapshot")
		}
		p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
		p.core.EditSchedule(*sub.RailSubscriptionID, func(s *nmimock.Schedule) { s.NextBilling = due.Add(7 * 24 * time.Hour) })
		require.NoError(t, p.persist(t.Context()))
		notice("evt_while_fetch_running")
		jobs, err = queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.subscription_converge"))
		require.NoError(t, err)
		require.Len(t, jobs.Jobs, 2, "a callback cannot disappear into the already captured snapshot")
		var follow *rivertype.JobRow
		for _, job := range jobs.Jobs {
			if job.ID != first.ID {
				follow = job
			}
		}
		require.NotNil(t, follow)
		var followArgs struct {
			After int64 `json:"after"`
		}
		require.NoError(t, json.Unmarshal(follow.EncodedArgs, &followArgs))
		require.Equal(t, first.ID, followArgs.After)
		gate.Release()
		require.Eventually(t, func() bool {
			job, err := queue.JobGet(t.Context(), first.ID)
			return err == nil && job.State == rivertype.JobStateCompleted
		}, 20*time.Second, 20*time.Millisecond)
		_, err = queue.JobRetry(t.Context(), follow.ID)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			job, err := queue.JobGet(t.Context(), follow.ID)
			return err == nil && job.State == rivertype.JobStateCompleted
		}, 20*time.Second, 20*time.Millisecond)
		nmiRenewalReadback(t, a, p, sub)
	})
	t.Run("OR-J28-S01/signed-account-bound-evidence-settles-the-original-order", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet, order, object := unknownSale(t, a, p)
		body := stripeNotice(t, "evt_account_bound", "payment_intent.succeeded", object)
		callbackSettlesSale(t, a, p, fleet, order, func() {
			status, raw := callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
			require.Equal(t, http.StatusOK, status, raw)
		})
		foreign, err := a.beta.ListPayments(t.Context(), billing.PaymentListParams{})
		require.NoError(t, err)
		require.Empty(t, foreign.Items)
	})
	t.Run("OR-J28-S02/invalid-signature-and-account-cannot-admit-money", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet, order, object := unknownSale(t, a, p)
		body := stripeNotice(t, "evt_refused", "payment_intent.succeeded", object)
		beforeJobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		for _, profile := range []struct {
			name, account, signature string
			body                     []byte
		}{
			{"missing", "acct_e2e", "", body},
			{"wrong-secret", "acct_e2e", stripeSignature(body, "whsec_foreign"), body},
			{"tampered", "acct_e2e", stripeSignature(body, "whsec_critical_fixture"), append(append([]byte(nil), body...), ' ')},
			{"foreign-account", "acct_other", stripeSignature(body, "whsec_critical_fixture"), body},
		} {
			t.Run(profile.name, func(t *testing.T) {
				status, raw := callback(t, a, profile.account, profile.body, profile.signature)
				require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status, raw)
				current, err := a.alpha.GetOrder(t.Context(), order.ID)
				require.NoError(t, err)
				require.Equal(t, billing.OrderPaymentProcessing, current.Payment.Status)
				facts, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled", "order.completed"}, Limit: 100})
				require.NoError(t, err)
				require.Empty(t, facts.Events)
				require.Len(t, p.effects(t), 1)
				jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
				require.NoError(t, err)
				require.Len(t, jobs.Jobs, len(beforeJobs.Jobs), "a refused notice cannot wake financial work")
			})
		}
		callbackSettlesSale(t, a, p, fleet, order, func() {
			status, raw := callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
			require.Equal(t, http.StatusOK, status, raw)
		})
	})
	t.Run("OR-J28-S03/previous-signing-secret-expires-at-the-declared-deadline", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) {
			m := w.cfg.Merchants["alpha"]
			psp := m.PSPs["stripe"]
			psp.Secrets["webhook_signing_secret_previous"] = "whsec_previous_fixture"
			psp.Settings["webhook_overlap_expires_at"] = w.clock.Now().Add(time.Hour).Format(time.RFC3339Nano)
			m.PSPs["stripe"] = psp
			w.cfg.Merchants["alpha"] = m
		})
		fleet, order, object := unknownSale(t, a, p)
		body := stripeNotice(t, "evt_overlap", "payment_intent.succeeded", object)
		callbackSettlesSale(t, a, p, fleet, order, func() {
			status, raw := callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_previous_fixture"))
			require.Equal(t, http.StatusOK, status, raw)
		})
		a.clock.Advance(time.Hour)
		status, raw := callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_previous_fixture"))
		require.Equal(t, http.StatusUnauthorized, status, raw)
		status, raw = callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
		require.Equal(t, http.StatusOK, status, raw)
		settledSale(t, a, p, order.ID)
	})
	t.Run("OR-J28-S04/duplicate-and-old-observations-cannot-regress-settlement", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet, order, object := unknownSale(t, a, p)
		body := stripeNotice(t, "evt_once", "payment_intent.succeeded", object)
		settled := callbackSettlesSale(t, a, p, fleet, order, func() {
			for range 2 {
				status, raw := callback(t, a, "acct_e2e", body, stripeSignature(body, "whsec_critical_fixture"))
				require.Equal(t, http.StatusOK, status, raw)
			}
		})
		object["status"] = "processing"
		old := stripeNotice(t, "evt_older_processing", "payment_intent.processing", object)
		for _, notice := range [][]byte{old, body, old} {
			status, raw := callback(t, a, "acct_e2e", notice, stripeSignature(notice, "whsec_critical_fixture"))
			require.Equal(t, http.StatusOK, status, raw)
		}
		require.Equal(t, settled.Payment.PaymentID, settledSale(t, a, p, order.ID).Payment.PaymentID)
	})
}
