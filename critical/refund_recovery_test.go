//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func purchasedSale(t *testing.T) (*authorityWorld, *providerLedger, billing.Order) {
	t.Helper()
	a, p := newPaymentWorld(t)
	input := saleInput(t, a, p)
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": "sale-for-refund"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status)
	require.NotNil(t, order.Payment.PaymentID)
	return a, p, order
}

func TestRefundRecovery(t *testing.T) {
	t.Run("OR-J20-S02/overlapping-refunds-conserve-original-charge", func(t *testing.T) {
		a, p, order := purchasedSale(t)
		payment := *order.Payment.PaymentID
		type answer struct {
			refund *billing.Payment
			err    error
		}
		answers := make(chan answer, 2)
		start := make(chan struct{})
		for _, key := range []string{"overlap-first", "overlap-second"} {
			go func() {
				<-start
				refund, err := a.alpha.RefundPayment(t.Context(), payment, billing.RefundPaymentParams{Amount: 20_000_000, Reason: "Overlapping request", IdempotencyKey: key})
				answers <- answer{refund, err}
			}()
		}
		close(start)
		succeeded := 0
		for range 2 {
			result := <-answers
			if result.err == nil {
				succeeded++
				require.NotNil(t, result.refund)
				require.Equal(t, billing.PaymentSucceeded, result.refund.Status)
				require.EqualValues(t, -20_000_000, result.refund.Amount)
			}
		}
		require.Equal(t, 1, succeeded, "only one overlapping refund fits the settled charge")
		read, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.EqualValues(t, 20_000_000, read.AmountRefunded)
		require.Len(t, read.Refunds, 1)
		final, err := a.alpha.RefundPayment(t.Context(), payment, billing.RefundPaymentParams{Full: true, Reason: "Return remainder", IdempotencyKey: "overlap-remainder"})
		require.NoError(t, err)
		require.EqualValues(t, -10_000_000, final.Amount)
		var debited, refunded int64
		for _, effect := range p.effects(t) {
			if effect.Kind == "charge" {
				debited += effect.Amount
			} else if effect.Kind == "refund" {
				refunded += effect.Amount
			} else {
				t.Fatalf("unknown effect %s", effect.Kind)
			}
		}
		require.EqualValues(t, 3000, debited)
		require.EqualValues(t, 3000, refunded)
		read, err = a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentRefunded, read.Status)
		require.EqualValues(t, 30_000_000, read.AmountRefunded)
	})
	t.Run("OR-J20-S03/lost-refund-response-resolves-original-provider-refund", func(t *testing.T) {
		a, p, order := purchasedSale(t)
		payment := *order.Payment.PaymentID
		p.mu.Lock()
		p.lostRefundResponse = true
		p.mu.Unlock()
		params := billing.RefundPaymentParams{Amount: 10_000_000, Reason: "Refund with response lost", IdempotencyKey: "refund-response-lost"}
		_, _ = a.alpha.RefundPayment(t.Context(), payment, params)
		effects := p.effects(t)
		require.Len(t, effects, 2, "provider must actually have returned money before the loss")
		var refundID string
		for _, effect := range effects {
			if effect.Kind == "refund" {
				refundID = effect.ID
				require.EqualValues(t, 1000, effect.Amount)
			}
		}
		require.NotEmpty(t, refundID)
		before, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Zero(t, before.AmountRefunded, "the fault must leave local refund receipt unresolved")
		p.mu.Lock()
		p.lostRefundResponse = false
		p.mu.Unlock()
		a.clock.Advance(20 * time.Minute)
		fleet := startFleet(t, a.world)
		jobs, err := fleet.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		for _, job := range jobs.Jobs {
			if job.State != rivertype.JobStateCompleted && job.State != rivertype.JobStateRunning {
				_, err = fleet.JobRetry(t.Context(), job.ID)
				require.NoError(t, err)
			}
		}
		require.Eventually(t, func() bool {
			read, err := a.alpha.GetPayment(t.Context(), payment)
			return err == nil && read.AmountRefunded == 10_000_000
		}, 20*time.Second, 20*time.Millisecond)
		replay, err := a.alpha.RefundPayment(t.Context(), payment, params)
		require.NoError(t, err)
		require.Equal(t, billing.PaymentSucceeded, replay.Status)
		require.EqualValues(t, -10_000_000, replay.Amount)
		read, err := a.alpha.GetPayment(t.Context(), payment)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 1)
		require.Len(t, p.effects(t), 2, "recovery and caller replay cannot refund twice")
	})
}
