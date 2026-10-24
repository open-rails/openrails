//go:build integration

package critical_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func hostFleet(t *testing.T, w *world, concurrency ...int) *river.Client[pgx.Tx] {
	t.Helper()
	workers := 2
	if len(concurrency) > 0 {
		workers = concurrency[0]
	}
	fleet, err := riverhelpers.New(t.Context(), w.pool, &river.Config{Schema: w.schema, Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: workers}}}, w.engine.RiverJobs())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(t, fleet.StopAndCancel(ctx))
	})
	return fleet
}

func TestProviderRecovery(t *testing.T) {
	t.Run("OR-J11-S01/accepted-payment-response-loss-recovers-without-new-debit", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := hostFleet(t, a.world)
		input := saleInput(t, a, p)
		p.interruptPaymentResponses()
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "ambiguous-purchase"})
		t.Logf("caller response after provider acceptance/loss: HTTP%d", status)
		require.NotEqual(t, http.StatusUnauthorized, status, string(raw))
		effects := p.effects(t)
		require.Len(t, effects, 1)
		require.EqualValues(t, 3000, effects[0].Amount)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		order := orders.Items[0]
		require.NotEqual(t, billing.OrderComplete, order.Status, "the network cut must actually leave an unresolved accepted payment")
		p.restorePaymentResponses()
		if order.Status != billing.OrderComplete {
			require.Equal(t, billing.OrderPaymentProcessing, order.Payment.Status, "an ambiguous accepted debit is not a card decline")
			access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sale:owned"}})
			require.NoError(t, err)
			require.False(t, access.Entitlements["sale:owned"], "unverified money does not grant access")
			a.clock.Advance(20 * time.Minute)
			jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
			require.NoError(t, err)
			require.NotEmpty(t, jobs.Jobs, "accepted financial work must have a durable host-queue job")
			for _, job := range jobs.Jobs {
				if job.State != rivertype.JobStateCompleted {
					_, err = fleet.JobRetry(t.Context(), job.ID)
					require.NoError(t, err)
				}
			}
			finished, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
			defer unsubscribe()
			require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
			require.NoError(t, fleet.Start(t.Context()))
			timer := time.NewTimer(20 * time.Second)
			defer timer.Stop()
			completed := false
			for !completed {
				select {
				case event := <-finished:
					if event.Job.Kind != "openrails.provider_operation" {
						continue
					}
					require.Equal(t, river.EventKindJobCompleted, event.Kind, "financial recovery worker must complete, not fail")
					completed = true
				case <-timer.C:
					snapshot, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
					require.NoError(t, err)
					t.Fatalf("accepted operation failed to recover through the actual fleet: %+v", snapshot.Jobs)
				}
			}
		}
		recovered, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderComplete, recovered.Status)
		require.NotNil(t, recovered.Payment.PaymentID)
		payment, err := a.alpha.GetPayment(t.Context(), *recovered.Payment.PaymentID)
		require.NoError(t, err)
		require.EqualValues(t, 30_000_000, payment.Amount)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
		require.Len(t, p.effects(t), 1, "readback/recovery must not add another external economic debit")
		calls := p.core.Submitted("/v1/payment_intents")
		require.NotEmpty(t, calls)
		key := calls[0].IdempotencyKey
		require.NotEmpty(t, key)
		for _, call := range calls {
			require.Equal(t, key, call.IdempotencyKey, "a retry retains the accepted provider identity")
		}
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["sale:owned"])
	})
	t.Run("OR-J11-S02/unavailable-evidence-keeps-money-unresolved", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		fleet := hostFleet(t, a.world, 1)
		input := saleInput(t, a, p)
		p.interruptPaymentResponses()
		p.unavailableEvidence(true)
		_, _ = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "unavailable-evidence-purchase"})
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		order := orders.Items[0]
		require.Equal(t, billing.OrderPaymentProcessing, order.Payment.Status)
		require.Len(t, p.effects(t), 1)
		a.clock.Advance(20 * time.Minute)
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		require.NotEmpty(t, jobs.Jobs)
		_, err = fleet.JobRetry(t.Context(), jobs.Jobs[0].ID)
		require.NoError(t, err)
		finished, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobSnoozed, river.EventKindJobFailed)
		defer unsubscribe()
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		select {
		case <-p.evidenceRequested:
		case <-time.After(15 * time.Second):
			t.Fatal("actual provider verifier did not request evidence")
		}
		for {
			select {
			case event := <-finished:
				if event.Job.ID != jobs.Jobs[0].ID {
					continue
				}
				require.NotEqual(t, river.EventKindJobFailed, event.Kind)
			case <-time.After(15 * time.Second):
				t.Fatal("financial job never durably retained its unresolved outcome")
			}
			break
		}
		held, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderPaymentProcessing, held.Payment.Status)
		require.NotEqual(t, billing.OrderComplete, held.Status)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		require.False(t, access.Entitlements["sale:owned"])
		require.Len(t, p.effects(t), 1)
		p.unavailableEvidence(false)
		p.restorePaymentResponses()
		a.clock.Advance(time.Hour)
		jobs, err = fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			if job.State != rivertype.JobStateCompleted {
				_, err = fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
		deadline := time.NewTimer(20 * time.Second)
		defer deadline.Stop()
		for {
			read, err := a.alpha.GetOrder(t.Context(), order.ID)
			require.NoError(t, err)
			if read.Status == billing.OrderComplete {
				break
			}
			select {
			case event := <-finished:
				if event.Job.Kind == "openrails.provider_operation" {
					require.NotEqual(t, river.EventKindJobFailed, event.Kind)
				}
			case <-deadline.C:
				t.Fatal("restored provider evidence did not resolve the existing order")
			}
		}
		require.Len(t, p.effects(t), 1, "restoring evidence resolves the original economic effect")
	})

}
