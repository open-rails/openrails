//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func TestUnresolvedMoneyContracts(t *testing.T) {
	t.Run("OR-J29-S02/checkout-process-death-recovers-accepted-money-once", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		input := saleInput(t, a, p)
		queue := hostFleet(t, a.world)
		child := startWorkerProcess(t, a.world, p, func(c *workerProcessConfig) {
			c.HTTPAuthTenant, c.HTTPAuthPrivate = a.tenant, a.privateSigner
		})
		gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
		p.mu.Lock()
		p.responseGate = gate
		p.mu.Unlock()
		defer gate.unblock()
		body, err := json.Marshal(input)
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, child.url+"/v1/me/orders", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Authorization", "Bearer "+a.aliceToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "checkout-process-death")
		done := make(chan error, 1)
		go func() {
			response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
			if response != nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			done <- err
		}()
		select {
		case <-gate.arrived:
		case err := <-done:
			t.Fatalf("checkout finished before provider acceptance barrier: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatal("checkout never reached its real external payment boundary")
		}
		require.Len(t, p.effects(t), 1)
		require.EqualValues(t, 3000, p.effects(t)[0].Amount)
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Len(t, orders.Items, 1)
		require.NotEqual(t, billing.OrderComplete, orders.Items[0].Status)
		jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		require.NotEmpty(t, jobs.Jobs, "accepted checkout must have a durable recovery job before the external request")
		child.kill(t)
		gate.unblock()
		require.Error(t, <-done, "the buyer's connection must actually be lost")
		a.clock.Advance(20 * time.Minute)
		replacement := startWorkerProcess(t, a.world, p)
		for _, old := range jobs.Jobs {
			for range 2 {
				_, err := queue.Insert(t.Context(), replayedOperation{Payload: old.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
			}
		}
		settled := settledSale(t, a, p, orders.Items[0].ID)
		require.Equal(t, orders.Items[0].ID, settled.ID)
		require.Len(t, p.core.Submitted("/v1/payment_intents"), 1, "a replacement process must observe the accepted debit, never create another")
		replacement.kill(t)
	})
	t.Run("OR-J29-S05/catalog-and-account-retirement-preserve-accepted-terms", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		_, order, _ := unknownSale(t, a, p)
		products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		product := products.Items[0]
		_, err = a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "sale", UnitAmount: 99_000_000, Currency: "USD"})
		require.NoError(t, err)
		_, err = a.alpha.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		// Manifest-owned credentials change through a deployment, not the
		// merchant API (which requires managed Vault configuration).
		require.NoError(t, a.engine.Close(t.Context()))
		m := a.cfg.Merchants["alpha"]
		psp := m.PSPs["stripe"]
		psp.Archived = true
		m.PSPs["stripe"] = psp
		m.CardVault = ""
		a.cfg.Merchants["alpha"] = m
		a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
		require.NoError(t, err)
		a.alpha, err = a.engine.MerchantBySlug("alpha")
		require.NoError(t, err)
		queue := hostFleet(t, a.world)
		settled := recoverSale(t, a, p, queue, order.ID)
		require.EqualValues(t, 30_000_000, settled.Total)
		require.Equal(t, order.ID, settled.ID)
		current, err := a.alpha.GetProduct(t.Context(), product.ID)
		require.NoError(t, err)
		require.True(t, current.Archived)
		retired, err := a.alpha.GetPSP(t.Context(), psps.Items[0].ID)
		require.NoError(t, err)
		require.True(t, retired.Archived)
		require.Len(t, p.effects(t), 1)
	})
	t.Run("OR-J29-S06/retention-and-export-cannot-discard-unknown-money", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { w.clock.Advance(-400 * 24 * time.Hour) })
		queue, order, _ := unknownSale(t, a, p)
		require.True(t, order.CreatedAt.Before(time.Now().Add(-399*24*time.Hour)), "the pending order must actually predate the retention window")
		p.mu.Lock()
		p.evidenceUnavailable = true
		p.mu.Unlock()
		a.clock.Advance(400 * 24 * time.Hour)
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(queue)))
		require.NoError(t, queue.Start(t.Context()))
		for _, kind := range []string{"openrails.order_expiry", "openrails.cleanup_expired_data"} {
			runRegistered(t, queue, kind, 2)
		}
		retained, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderPaymentProcessing, retained.Payment.Status)
		require.EqualValues(t, 30_000_000, retained.Total)
		var archive bytes.Buffer
		require.Error(t, a.alpha.ExportBillingArchive(t.Context(), &archive), "export cannot turn unresolved money into an apparently complete book")
		payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: order.CustomerID})
		require.NoError(t, err)
		require.Empty(t, payments.Items, "an unverified accepted debit is retained as pending work, not fabricated settled money")
		p.mu.Lock()
		p.evidenceUnavailable = false
		p.mu.Unlock()
		a.clock.Advance(20 * time.Minute)
		jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		require.NotEmpty(t, jobs.Jobs)
		for _, row := range jobs.Jobs {
			if row.State == rivertype.JobStateScheduled || row.State == rivertype.JobStateRetryable {
				_, err = queue.JobRetry(t.Context(), row.ID)
				require.NoError(t, err)
			}
		}
		settledSale(t, a, p, order.ID)
	})
}
