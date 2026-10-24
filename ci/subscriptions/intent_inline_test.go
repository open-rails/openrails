//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payment"
)

// Inline execution uses the same durable ledger and Stripe handler as queued
// execution. Re-enqueue revives canceled/expired non-money work, preserves
// settled outcomes, and cannot steal an operation from a live executor.
func TestInlineProviderOperationUsesDurableLedger(t *testing.T) {
	cases := []struct {
		status   string
		executes bool
	}{
		{intents.StatusPending, true},
		{intents.StatusFailedRetryable, true},
		{intents.StatusSuperseded, true},
		{intents.StatusExpired, true},
		{intents.StatusSucceeded, false},
		{intents.StatusFailedTerminal, false},
		{intents.StatusUnknownNeedsVerify, false},
		{intents.StatusInFlight, false},
	}
	w := newWorld(t, func(cfg *config.Config) {
		// This ledger scenario cancels nine distinct customers in one merchant;
		// give its fixture that ordinary documented hourly allowance.
		cfg.UserActionRateLimit.MinimumCustomersPerHour = int64(len(cases) + 1)
	})
	w.armDestructive()
	// Import while the fixture's ordinary workers can settle their setup, then
	// stop them so only the inline callers under test can claim these rows.
	members := make([]*legacy, len(cases)+1)
	for i := range members {
		members[i] = importLegacy(t, w, "stripe", embedded)
	}
	require.NoError(t, w.jobs.Stop(t.Context()))
	runtime := engine.Graph(w.rt).Runtime
	store, runner := intents.NewStore(runtime.DB), runtime.IntentRunner()
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	params := func(l *legacy) intents.EnqueueParams {
		sub := l.sub.UUID()
		var key, origin string
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT idempotency_key, origin FROM billing.provider_intents
			WHERE subscription_id = $1 AND intent_type = $2`), sub, intents.TypeStripeCancelSubscription).Scan(&key, &origin))
		return intents.EnqueueParams{
			MerchantID: w.client[embedded].MerchantID().UUID(), Provider: "stripe",
			IntentType: intents.TypeStripeCancelSubscription, SubscriptionID: &sub,
			PspID: w.psp["stripe"].UUID(), Origin: intents.Origin(origin),
			Payload:        intents.StripeCancelPayload{RailSubscriptionID: l.railSub},
			IdempotencyKey: key, NextAttemptAt: w.clock.Now(),
		}
	}
	for i, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			l := members[i]
			status, body := l.meCancel(l.sub)
			require.Equal(t, http.StatusOK, status, "%v", body)
			p := params(l)
			prior, err := store.Enqueue(ctx, p)
			require.NoError(t, err)
			attempts := int32(3)
			if tc.status == intents.StatusPending {
				attempts = 0
			}
			_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.provider_intents
				SET status = $2, attempts = $3, lease_expires_at = $4,
				    executed_at = CASE WHEN $2 = 'succeeded' THEN $4::timestamptz ELSE NULL END WHERE id = $1`),
				prior.ID, tc.status, attempts, w.clock.Now().Add(time.Hour))
			require.NoError(t, err)
			before, err := store.Get(ctx, prior.ID)
			require.NoError(t, err)
			row, err := runner.EnqueueAndExecute(ctx, p)
			require.NoError(t, err)
			require.Equal(t, prior.ID, row.ID, "the original operation keeps its identity")
			durable, err := store.Get(ctx, row.ID)
			require.NoError(t, err)
			require.Equal(t, row, durable, "the inline response is the committed ledger row")
			if tc.executes {
				require.Equal(t, intents.StatusSucceeded, row.Status)
				require.Equal(t, true, w.stripe.Subscription(l.railSub)["cancel_at_period_end"])
				require.Len(t, l.stripeSubWrites(), 1)
				if tc.status == intents.StatusSuperseded || tc.status == intents.StatusExpired {
					require.EqualValues(t, 1, row.Attempts, "revival starts a fresh attempt")
				} else {
					require.Equal(t, attempts+1, row.Attempts)
				}
				replay, err := runner.EnqueueAndExecute(ctx, p)
				require.NoError(t, err)
				require.Equal(t, row.ID, replay.ID)
				require.Equal(t, intents.StatusSucceeded, replay.Status)
				require.Equal(t, row.Attempts, replay.Attempts)
				require.Len(t, l.stripeSubWrites(), 1, "successful replay never cancels twice")
			} else {
				require.Equal(t, before.Status, row.Status)
				require.Equal(t, before.Attempts, row.Attempts)
				require.Equal(t, before.Payload, row.Payload)
				require.Empty(t, l.stripeSubWrites())
			}
		})
	}

	t.Run("competing caller", func(t *testing.T) {
		l := members[len(cases)]
		status, body := l.meCancel(l.sub)
		require.Equal(t, http.StatusOK, status, "%v", body)
		p := params(l)
		g := newGate(func(r *http.Request) bool {
			return r.Method == http.MethodGet && r.URL.Path == "/v1/subscriptions/"+l.railSub
		}, false)
		release := sync.OnceFunc(func() { close(g.release) })
		t.Cleanup(release)
		w.stripe.hold(g)
		t.Cleanup(w.stripe.unhold)
		type answer struct {
			row gen.BillingProviderIntent
			err error
		}
		first := make(chan answer, 1)
		go func() {
			row, err := runner.EnqueueAndExecute(ctx, p)
			first <- answer{row, err}
		}()
		select {
		case <-g.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the first executor never reached Stripe")
		}
		other, err := runner.EnqueueAndExecute(ctx, p)
		require.NoError(t, err)
		require.Equal(t, intents.StatusInFlight, other.Status)
		require.EqualValues(t, 1, other.Attempts, "the second caller cannot steal the live claim")
		require.Empty(t, l.stripeSubWrites())
		release()
		var got answer
		select {
		case got = <-first:
		case <-time.After(10 * time.Second):
			t.Fatal("the first executor did not finish after Stripe was released")
		}
		require.NoError(t, got.err)
		require.Equal(t, other.ID, got.row.ID)
		require.Equal(t, intents.StatusSucceeded, got.row.Status)
		require.Len(t, l.stripeSubWrites(), 1)
	})

	// Tier changes freeze a commercial decision, unlike cancellation work.
	// A replay cannot revive an expired/superseded decision or replace its terms.
	for _, status := range []string{intents.StatusSuperseded, intents.StatusExpired} {
		t.Run("frozen tier change "+status, func(t *testing.T) {
			p := params(members[0])
			p.IntentType, p.IdempotencyKey = payment.TypeStripeTierChange, uuid.NewString()
			p.Payload = payment.StripeTierChangePayload{Action: "upgrade", Currency: "USD", AmountDueNow: 9_990_000}
			prior, err := store.Enqueue(ctx, p)
			require.NoError(t, err)
			_, err = w.pool.Exec(t.Context(), w.q(`UPDATE billing.provider_intents SET status = $2 WHERE id = $1`), prior.ID, status)
			require.NoError(t, err)
			p.Payload = payment.StripeTierChangePayload{Action: "downgrade", Currency: "USD", AmountDueNow: 0}
			p.NextAttemptAt = p.NextAttemptAt.Add(time.Hour)
			before := len(members[0].stripeSubWrites())
			row, err := runner.EnqueueAndExecute(ctx, p)
			require.NoError(t, err)
			require.Equal(t, prior.ID, row.ID)
			require.Equal(t, status, row.Status)
			require.JSONEq(t, string(prior.Payload), string(row.Payload))
			require.True(t, prior.NextAttemptAt.Equal(row.NextAttemptAt))
			require.Len(t, members[0].stripeSubWrites(), before, "a frozen decision makes no provider write")
		})
	}
	require.Empty(t, w.stripe.Unexpected())
}
