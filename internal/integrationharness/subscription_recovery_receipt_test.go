//go:build integration

package integrationharness

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/dunning"
	"github.com/stretchr/testify/require"
)

// Recovery regressions exercise the authenticated HTTP surface. Fixture-only lifecycle time
// advancement avoids waiting a month; every A/B charge and replay goes through
// the ordinary authenticated HTTP Client and the real rebill runner.
func TestSubscriptionRecoveryReceiptRegressions(t *testing.T) {
	ctx := context.Background()
	h := &Harness{t: t, ctx: ctx, DSN: dbtest.SharedPostgresDSN(t), SuperDSN: dbtest.SharedSuperuserDSN(t)}
	gateway := NewFakeNMIGateway(t)
	// Capture the ordinary adapter's exact wire against the existing loopback
	// gateway. Redact the credential; never invent amount/currency fields in
	// the classic Direct Post response, which carries only an approval and ID.
	wireGateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		if r.Form.Get("type") != "sale" {
			gateway.serve(w, r)
			return
		}
		fields := url.Values{}
		for key, values := range r.Form {
			if key != "security_key" {
				fields[key] = append([]string(nil), values...)
			}
		}
		fields.Set("security_key", "REDACTED")
		recorded := httptest.NewRecorder()
		gateway.serve(recorded, r)
		t.Logf("NMI Direct Post request=%s; response=%s", fields.Encode(), recorded.Body.String())
		for key, values := range recorded.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(recorded.Code)
		_, _ = w.Write(recorded.Body.Bytes())
	}))
	t.Cleanup(wireGateway.Close)
	surface := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: wireGateway.URL}
	}))
	client := surface.Client(openrails.WithTimeout(0))
	seed := func() SubscriptionFixture {
		f := h.SeedPastDueSubscription(surface.App().Runtime, dbtest.TestMerchantID)
		gateway.RegisterPlan(f.RailSubscriptionID, "12.00", "USD")
		return f
	}
	retry := func(f SubscriptionFixture, key string) (*openrails.SubscriptionRetryNowResult, error) {
		return client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(f.Customer), SubscriptionID: openrails.SubscriptionID(f.Subscription), IdempotencyKey: key})
	}
	t.Run("old_declined_period_A_key_does_not_mutate_period_B", func(t *testing.T) {
		f := seed()
		keyA := "period-A-" + uuid.NewString()
		gateway.SetMode(NMISaleDecline)
		_, err := retry(f, keyA)
		requireDeclined(t, err)
		gateway.SetMode(NMISaleApprove)
		recovered, err := retry(f, "period-A-recovery-"+uuid.NewString())
		require.NoError(t, err)
		require.Equal(t, "active", recovered.Subscription.Status)
		// Advance the fixture to the next period after A's committed payment, then
		// let the real B attempt decline at ordinal zero (B now has one failure).
		_, err = h.Pool().Exec(ctx, `UPDATE openrails.subscriptions SET status='past_due',current_period_starts_at=current_period_ends_at-interval '30 days',current_period_ends_at=now(),retry_attempts=0,last_retry_at=NULL,next_retry_at=NULL WHERE id=$1`, f.Subscription)
		require.NoError(t, err)
		gateway.SetMode(NMISaleDecline)
		_, err = retry(f, "period-B-"+uuid.NewString())
		requireDeclined(t, err)
		before := h.SubscriptionState(f.Subscription)
		require.NotNil(t, before.RetryAttempts)
		require.EqualValues(t, 1, *before.RetryAttempts)
		paymentsBefore := len(h.SubscriptionPayments(f.Subscription))
		gateway.SetMode(NMISaleApprove)
		salesBefore := gateway.SaleCount()
		_, err = retry(f, keyA)
		requireDeclined(t, err)
		after := h.SubscriptionState(f.Subscription)
		require.Equal(t, salesBefore, gateway.SaleCount(), "old key must not send a charge")
		t.Logf("B failure count before=%d after=%d; payment rows before=%d after=%d; no new provider sale", *before.RetryAttempts, *after.RetryAttempts, paymentsBefore, len(h.SubscriptionPayments(f.Subscription)))
		require.Equal(t, *before.RetryAttempts, *after.RetryAttempts, "period A's decline must not advance period B")
		require.Equal(t, paymentsBefore, len(h.SubscriptionPayments(f.Subscription)))
	})
	t.Run("synchronous_approval_must_match_frozen_amount_and_currency", func(t *testing.T) {
		for _, tc := range []struct{ name, amount, currency string }{{"amount", "0.01", "USD"}, {"currency", "12.00", "EUR"}} {
			t.Run(tc.name, func(t *testing.T) {
				f := seed()
				gateway.SetMode(NMISaleApprove)
				gateway.SetVisible(true)
				gateway.RegisterPlan(f.RailSubscriptionID, tc.amount, tc.currency)
				key := "contradictory-sync-" + uuid.NewString()
				result, err := retry(f, key)
				require.NoError(t, err)
				sales := gateway.Sales()
				sale := sales[len(sales)-1]
				t.Logf("provider sale=%s %s; operation=%s; local payment=%+v", sale.Amount, sale.Currency, result.Operation.Status, result.Payment)
				require.Equal(t, "unknown_needs_verify", result.Operation.Status, "approved receipt must not settle a different frozen charge")
				require.Empty(t, h.SubscriptionPayments(f.Subscription), "no incorrect financial fact should be recorded")
				count := gateway.SaleCount()
				replayed, err := retry(f, key)
				require.NoError(t, err)
				require.Equal(t, "unknown_needs_verify", replayed.Operation.Status)
				require.Equal(t, count, gateway.SaleCount(), "replay of an unqualified approval never resubmits")
			})
		}
	})
	t.Run("future_schedule_is_not_a_live_claim", func(t *testing.T) {
		for _, tc := range []struct{ name, last string }{{"previous_attempt", "now()-interval '2 days'"}, {"never_retried", "NULL"}} {
			t.Run(tc.name, func(t *testing.T) {
				f := seed()
				gateway.SetMode(NMISaleApprove)
				gateway.SetVisible(true)
				_, err := h.Pool().Exec(ctx, `UPDATE openrails.subscriptions SET last_retry_at=`+tc.last+`,next_retry_at=now()+interval '10 minutes' WHERE id=$1`, f.Subscription)
				require.NoError(t, err)
				result, err := retry(f, "scheduled-"+uuid.NewString())
				require.NoError(t, err)
				require.Equal(t, "succeeded", result.Operation.Status)
			})
		}
	})
	t.Run("real_worker_claim_cannot_be_stolen", func(t *testing.T) {
		f := seed()
		gateway.SetMode(NMISaleApprove)
		_, err := h.Pool().Exec(ctx, `UPDATE openrails.subscriptions SET next_retry_at=now()-interval '1 second' WHERE id=$1`, f.Subscription)
		require.NoError(t, err)
		now := time.Now().UTC()
		changed, err := gen.New(h.Pool()).ClaimDunningAttempt(ctx, gen.ClaimDunningAttemptParams{ID: f.Subscription, MerchantID: f.Merchant.UUID(), Holder: "review-worker-" + uuid.NewString(), ClaimedAt: now, LeaseSeconds: int32(dunning.AttemptLease / time.Second)})
		require.NoError(t, err)
		require.EqualValues(t, 1, changed)
		count := gateway.SaleCount()
		_, err = retry(f, "claimed-"+uuid.NewString())
		require.ErrorIs(t, err, openrails.ErrSubscriptionRetryInProgress)
		require.Equal(t, count, gateway.SaleCount())
	})
	t.Run("body_bound_key_and_same_PSP_are_enforced", func(t *testing.T) {
		f := seed()
		other := h.SeedPastDueSubscription(surface.App().Runtime, dbtest.TestMerchantID, SubscriptionForCustomer(f.Customer))
		gateway.RegisterPlan(other.RailSubscriptionID, "12.00", "USD")
		gateway.SetMode(NMISaleDecline)
		key := "body-key-" + uuid.NewString()
		_, err := retry(f, key)
		requireDeclined(t, err)
		_, err = retry(other, key)
		require.ErrorIs(t, err, openrails.ErrSubscriptionRetryIdempotencyConflict)
		method := openrails.PaymentMethodID(other.Method)
		_, err = client.RetrySubscriptionNow(ctx, openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(f.Customer), SubscriptionID: openrails.SubscriptionID(f.Subscription), PaymentMethodID: &method, IdempotencyKey: key})
		require.ErrorIs(t, err, openrails.ErrSubscriptionRetryIdempotencyConflict)
		h.ReattributeToAnotherPSP(other)
		_, err = retry(other, "mismatched-"+uuid.NewString())
		require.ErrorIs(t, err, openrails.ErrPaymentMethodPSPMismatch)
	})
}
