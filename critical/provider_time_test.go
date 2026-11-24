//go:build integration

package critical_test

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// nmiPrune runs `openrails prune` of alpha's NMI PSP over [since, until]:
// a plan, or applied with expect.
func nmiPrune(t *testing.T, a *authorityWorld, since, until time.Time, expect *int) reconcile.PruneResult {
	t.Helper()
	psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	psp := psps.Items[0]
	client, err := nmi.NewAccountClient(slog.Default(), a.alpha.MerchantID().UUID(), psp.ID.UUID(), psp.GatewayID, &config.NMIProviderSettings{SecurityKey: "critical-nmi-key", Environment: config.EnvironmentTest})
	require.NoError(t, err)
	client.ReadOnly = true
	client.UseTransport(a.deps.NMITransport)
	database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
	require.NoError(t, err)
	binding := reconcile.PSPBinding{ID: psp.ID.UUID(), Rail: "nmi", AccountID: psp.GatewayID}
	var res reconcile.PruneResult
	require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error {
		res, err = reconcile.PrunePSPExcess(ctx, database, reconcile.NewNMIFetcher(client), reconcile.ProviderNMI, binding,
			reconcile.PruneParams{Since: since, Until: until, Apply: expect != nil, ExpectedRows: expect, Actor: "critical operator"})
		return err
	}))
	return res
}

// Provider times are read as each provider states them.
func TestProviderTime(t *testing.T) {
	t.Run("OR-J28-S09/an-nmi-nonce-signs-a-delivery-and-its-event-applies-once", func(t *testing.T) {
		a, _, sub := nmiMembership(t)
		queue := hostFleet(t, a.world)
		converges := func() []*rivertype.JobRow {
			t.Helper()
			jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.subscription_converge"))
			require.NoError(t, err)
			return jobs.Jobs
		}
		deliver := func(id, nonce string) {
			t.Helper()
			status, raw := nmiSignedNotice(t, a, id, *sub.RailSubscriptionID, nonce)
			require.Equal(t, http.StatusOK, status, raw)
		}
		// NMI's t is a nonce, not a time: an opaque one signs a delivery.
		deliver("evt_nonce", "5f2b9c0e7a1d4c83")
		woken := converges()
		require.Len(t, woken, 1)
		_, err := queue.JobDelete(t.Context(), woken[0].ID)
		require.NoError(t, err)
		// The delivery replayed, and NMI's retry of it under a nonce that reads
		// as a long-past time, apply nothing: its event_id was applied.
		deliver("evt_nonce", "5f2b9c0e7a1d4c83")
		deliver("evt_nonce", "1000000000")
		require.Empty(t, converges())
		deliver("evt_after", "1000000000")
		require.Len(t, converges(), 1, "another event under that nonce applies")
	})

	t.Run("OR-J15-S73/prune-keeps-an-nmi-sale-settled-after-its-window", func(t *testing.T) {
		a, p, _ := nmiMembership(t)
		sales := p.core.Sales()
		require.Len(t, sales, 1)
		sale := sales[0]
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		receipts, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, receipts.Items, 1)
		receipt := receipts.Items[0]
		require.Equal(t, sale.TransactionID, receipt.TransactionID)
		// NMI also holds a sale OpenRails does not, inside the window.
		p.core.AddSale(nmimock.Sale{Vault: sale.Vault, Amount: "5.00", At: sale.At.Add(-30 * time.Minute)})
		since, until := sale.At.Add(-time.Hour), sale.At.Add(time.Hour)
		excess := func(res reconcile.PruneResult) []uuid.UUID {
			t.Helper()
			require.Empty(t, res.PaymentSkipReason, "NMI's read of the window is complete")
			return append(append([]uuid.UUID{}, res.PaymentIDs...), res.SkippedPaymentIDs...)
		}
		// NMI's report proves absence in the window: a receipt it does not
		// list is excess.
		p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Hidden = true })
		require.Equal(t, []uuid.UUID{receipt.ID.UUID()}, excess(nmiPrune(t, a, since, until, nil)))
		p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Hidden = false })
		// Settled after the window, the sale's last change is outside it; the
		// read reaches it and the receipt stays.
		a.clock.Advance(2 * time.Hour)
		p.core.Settle(sale.TransactionID)
		require.Empty(t, excess(nmiPrune(t, a, since, until, nil)))
		zero := 0
		require.Zero(t, nmiPrune(t, a, since, until, &zero).Payments)
		kept, err := a.alpha.GetPayment(t.Context(), receipt.ID)
		require.NoError(t, err)
		require.Equal(t, sale.TransactionID, kept.TransactionID)
	})
}
