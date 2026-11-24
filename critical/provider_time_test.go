//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/ccbillmock"
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

	t.Run("OR-J38-S44/a-ccbill-refresh-reads-datalink-a-day-at-a-time-and-loses-nothing", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		w := newWorld(t, func(w *world) { declareCCBill(w, dataLink.URL()) })
		price := ccbillMemberPrice(t, w.alpha)
		now := w.clock.Now().Truncate(time.Second)
		// Nine members, each rebilled once, over nine days at drifting
		// times of day.
		book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"}}
		rebills := map[string]time.Time{}
		members := map[string]billing.CustomerID{}
		for i := range 9 {
			sub, initial, txn := fmt.Sprintf("0223344556600000%03d", i), fmt.Sprintf("0812233445500000%03d", i), fmt.Sprintf("0912233445500000%03d", i)
			rebill := now.Add(-time.Hour - time.Duration(i)*(26*time.Hour+37*time.Minute+13*time.Second))
			start, next := rebill.Add(-720*time.Hour), rebill.Add(720*time.Hour)
			customer := billing.CustomerID(uuid.New())
			rebills[txn], members[txn] = rebill, customer
			dataLink.AddMember(ccbillmock.Member{SubscriptionID: sub, Username: "member-" + sub, Email: "member-" + sub + "@critical.invalid", Signup: start, Rebill: &next})
			dataLink.AddRebill(ccbillmock.Rebill{SubscriptionID: sub, TransactionID: txn, At: rebill, Amount: "10.00"})
			book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: customer})
			book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: "ccbill-member-" + sub, Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: sub,
				StartedAt: start, PaidThrough: &rebill,
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: sub, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: rebill}})
			book.Transactions = append(book.Transactions,
				billing.DeclaredTransaction{RailSubscriptionID: sub, TransactionID: initial, Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start},
				billing.DeclaredTransaction{RailSubscriptionID: sub, TransactionID: txn, Type: "sale", Success: true, Currency: "USD", OccurredAt: rebill})
		}
		res, err := w.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, res.Blocked, "%+v", res.Reasons)
		fleet := hostFleet(t, w)
		fleet.PeriodicJobs().Clear()
		require.NoError(t, w.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		require.Eventually(t, func() bool {
			return len(attributionJobs(t, fleet, rivertype.JobStateScheduled)) == 1
		}, 20*time.Second, 20*time.Millisecond, "the import's rebills wait for the pull")
		refreshMerchant(t, fleet, w.alpha.MerchantID().UUID())

		for txn, customer := range members {
			require.EqualValues(t, 10_000_000, ccbillPayments(t, w, customer)[txn], "the pull records the rebill of %s", rebills[txn])
		}
		require.Empty(t, dataLink.Unexpected(), "no export spans more than 24 hours")
		// The pull's exports (every type) cover the nine days without a gap.
		var exports []ccbillmock.Export
		for _, e := range dataLink.Exports() {
			require.LessOrEqual(t, e.End.Sub(e.Start), 24*time.Hour-time.Second)
			if strings.Contains(e.Types, "CHARGEBACK") {
				exports = append(exports, e)
			}
		}
		slices.SortFunc(exports, func(a, b ccbillmock.Export) int { return a.Start.Compare(b.Start) })
		times := slices.Collect(maps.Values(rebills))
		first, last := slices.MinFunc(times, time.Time.Compare), slices.MaxFunc(times, time.Time.Compare)
		var reach time.Time // the end of the run of exports holding first
		for _, e := range exports {
			switch {
			case reach.IsZero():
				if !e.Start.After(first) && !e.End.Before(first) {
					reach = e.End
				}
			case e.Start.After(reach.Add(time.Second)):
			case e.End.After(reach):
				reach = e.End
			}
		}
		require.False(t, reach.Before(last), "the exports holding %s end at %s, before %s", first, reach, last)
	})
}
