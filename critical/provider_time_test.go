//go:build integration

package critical_test

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantdocs"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/pspimport"
	"github.com/open-rails/openrails/internal/reconcile"
	riverjobs "github.com/open-rails/openrails/internal/river"
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

// ccbillRebills lists a member in dataLink for each time, rebilled then, and
// imports alpha's book of them with each rebill declared without its amount,
// which the provider pull records. It answers each rebill's customer and time
// by transaction id.
func ccbillRebills(t *testing.T, w *world, dataLink *ccbillmock.Mock, times []time.Time) (map[string]billing.CustomerID, map[string]time.Time) {
	t.Helper()
	price := ccbillMemberPrice(t, w.alpha)
	book := billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"}}
	members, rebills := map[string]billing.CustomerID{}, map[string]time.Time{}
	for i, rebill := range times {
		sub, initial, txn := fmt.Sprintf("0223344556600000%03d", i), fmt.Sprintf("0812233445500000%03d", i), fmt.Sprintf("0912233445500000%03d", i)
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
	return members, rebills
}

// importCCBillMember imports alpha's CCBill member on sub, paid through
// paid, and answers its customer, sub and paid.
func importCCBillMember(t *testing.T, w *world, sub string, paid time.Time) (billing.CustomerID, string, time.Time) {
	t.Helper()
	price := ccbillMemberPrice(t, w.alpha)
	customer := billing.CustomerID(uuid.New())
	start := paid.Add(-720 * time.Hour)
	txns := []billing.DeclaredTransaction{{RailSubscriptionID: sub, TransactionID: "08" + sub[2:], Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start}}
	res, err := w.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"},
		Customers: []billing.DeclaredCustomer{{Customer: customer}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "ccbill-member-" + sub, Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: sub, StartedAt: start, PaidThrough: &paid,
			AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: sub, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: paid}}},
		Transactions: txns})
	require.NoError(t, err)
	require.Empty(t, res.Blocked, "%+v", res.Reasons)
	return customer, sub, paid
}

// liveDataLink makes a sandbox CCBill PSP's DataLink clients ask as a live
// account's do: no live PSP is declared beside a fake DataLink.
func liveDataLink(dl *ccbill.DataLinkClient) { dl.DevMode = false }

// liveCCBillRefresh is the refresh of alpha's one CCBill PSP with its
// DataLink asking as a live account's: the merchant's pass, and the PSP's
// paced pass, run as their workers run them.
func liveCCBillRefresh(t *testing.T, w *world) (uuid.UUID, func() error, func() error) {
	t.Helper()
	rt := embedded.Graph(w.engine).Runtime
	refresh := &riverjobs.ProviderRefreshWorker{DB: rt.DB, Config: rt.Config, Endpoints: rt.ProviderEndpoints, Clock: w.clock, Merchants: rt.Merchants, Contacts: rt.Contacts,
		StripeClients: rt.StripeClients, NMIClients: rt.NMIClients, DataLinkPacing: rt.DataLinkPacing(), WireDataLink: liveDataLink, Log: slog.Default()}
	psps, err := w.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	psp, mid := psps.Items[0].ID.UUID(), w.alpha.MerchantID().UUID()
	paced := &riverjobs.PacedProviderRefreshWorker{Refresh: refresh}
	return psp, func() error {
			return refresh.Work(t.Context(), &river.Job[riverjobs.ProviderRefreshRequestArgs]{JobRow: &rivertype.JobRow{}, Args: riverjobs.ProviderRefreshRequestArgs{MerchantID: mid}})
		}, func() error {
			return paced.Work(t.Context(), &river.Job[riverjobs.PacedProviderRefreshArgs]{JobRow: &rivertype.JobRow{}, Args: riverjobs.PacedProviderRefreshArgs{MerchantID: mid, PSPID: psp}})
		}
}

// drainPaced runs a paced refresh until it is done, advancing the clock over
// each snooze, and answers how many requests it waited between.
func drainPaced(t *testing.T, w *world, run func() error) int {
	t.Helper()
	passes := 0
	for err := run(); err != nil; err = run() {
		var snooze *rivertype.JobSnoozeError
		require.ErrorAs(t, err, &snooze)
		require.Equal(t, ccbill.DataLinkInterval, snooze.Duration)
		passes++
		require.Less(t, passes, 200, "the backlog catches up")
		w.clock.Advance(snooze.Duration)
	}
	return passes
}

// liveRequestsAnHourApart requires the live requests DataLink answered an
// hour apart, and answers them.
func liveRequestsAnHourApart(t *testing.T, dataLink *ccbillmock.Mock) []ccbillmock.Request {
	t.Helper()
	var live []ccbillmock.Request
	for _, r := range dataLink.Requests() {
		if !r.Live || r.Refused {
			continue
		}
		if n := len(live); n > 0 {
			require.GreaterOrEqual(t, r.At.Sub(live[n-1].At), time.Hour, "live requests at %s and %s", live[n-1].At, r.At)
		}
		live = append(live, r)
	}
	return live
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

	t.Run("OR-J28-S10/an-nmi-delivery-dated-before-its-replay-record-is-kept-applies-nothing", func(t *testing.T) {
		a, _, sub := nmiMembership(t)
		queue := hostFleet(t, a.world)
		converges := func() int {
			t.Helper()
			jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.subscription_converge"))
			require.NoError(t, err)
			return len(jobs.Jobs)
		}
		sale := func(id string, at time.Time) {
			t.Helper()
			status, raw := nmiSignedDelivery(t, a, map[string]any{"event_id": id, "event_type": "transaction.sale.success", "event_body": map[string]any{
				"transaction_id": "9" + id[len(id)-3:], "subscription": map[string]any{"subscription_id": *sub.RailSubscriptionID}, "merchant": map[string]string{"id": "critical-gateway"},
				"action": map[string]any{"amount": "10.00", "action_type": "sale", "date": at.UTC().Format("20060102150405")}}}, uuid.NewString())
			require.Equal(t, http.StatusOK, status, raw)
		}
		// Signed, and its event_id long forgotten: a sale dated before the
		// replay record would be kept is a replay, acknowledged unapplied, and
		// a person is told.
		sale("evt_replayed_old", a.clock.Now().Add(-91*24*time.Hour))
		require.Zero(t, converges())
		refused, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.webhook.replay_refused"})
		require.NoError(t, err)
		require.Len(t, refused.Items, 1)
		require.Equal(t, "nmi:evt_replayed_old", refused.Items[0].SubjectKey)
		sale("evt_recent_new", a.clock.Now().Add(-time.Hour))
		require.Equal(t, 1, converges(), "a delivery of its own time applies")
	})

	t.Run("OR-J28-S16/access-restored-after-an-nmi-chargeback-survives-a-replayed-batch", func(t *testing.T) {
		a, p := newNMIWorld(t, func(w *world) { w.posture = "full" })
		sub := nmiMembershipOn(t, a, p, catalog.Field[int]{})
		receipts, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, receipts.Items, 1)
		receipt := receipts.Items[0]
		batch := func(id string) {
			t.Helper()
			status, raw := nmiSignedDelivery(t, a, map[string]any{"event_id": id, "event_type": "chargeback.batch.complete", "event_body": map[string]any{
				"merchant": map[string]string{"id": "critical-gateway"}, "batch": map[string]any{"count": 1, "total_amount": "10.00"},
				"chargebacks": []map[string]string{{"id": "cb_won_1", "date": receipt.CreatedAt.UTC().Format(time.DateOnly), "cc_number": "************4242", "amount": "10.00", "reason_code": "10.4", "reason": "Fraud"}}}}, uuid.NewString())
			require.Equal(t, http.StatusOK, status, raw)
		}
		a.clock.Advance(24 * time.Hour)
		batch("evt_chargeback_first")
		require.False(t, entitledTo(t, a.alpha, sub.CustomerID, "nmi:member"), "the chargeback ends access")
		// The merchant wins the dispute and gives the member back access.
		_, err = a.alpha.CreateProductAccess(t.Context(), "dispute-won", []billing.CreateProductAccessParams{{CustomerID: sub.CustomerID, ProductID: sub.ProductID, Hours: ptr(24 * 30), Reason: billing.GrantReasonComp}})
		require.NoError(t, err)
		require.True(t, entitledTo(t, a.alpha, sub.CustomerID, "nmi:member"))
		// The batch again, under another event id: the reversal is booked, so
		// nothing is reversed or revoked twice.
		batch("evt_chargeback_replayed")
		require.True(t, entitledTo(t, a.alpha, sub.CustomerID, "nmi:member"), "the restored access survives the replay")
		read, err := a.alpha.GetPayment(t.Context(), receipt.ID)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 1, "the chargeback is booked once")
	})

	t.Run("OR-J28-S12/an-nmi-chargeback-of-a-sale-120-days-old-is-applied", func(t *testing.T) {
		// The sale is made 120 days before the world's present, which stays
		// behind the database's own: a payment is never dated in its future.
		a, p := newNMIWorld(t, func(w *world) { w.clock.Advance(-120 * 24 * time.Hour) })
		sub := nmiMembershipOn(t, a, p, catalog.Field[int]{})
		receipts, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, receipts.Items, 1)
		receipt := receipts.Items[0]
		// A chargeback comes months after its sale, and its entry carries the
		// sale's date, not the batch's.
		a.clock.Advance(120 * 24 * time.Hour)
		status, raw := nmiSignedDelivery(t, a, map[string]any{"event_id": "evt_late_chargeback", "event_type": "chargeback.batch.complete", "event_body": map[string]any{
			"merchant": map[string]string{"id": "critical-gateway"}, "batch": map[string]any{"count": 1, "total_amount": "10.00"},
			"chargebacks": []map[string]string{{"id": "cb_late_1", "date": receipt.CreatedAt.UTC().Format(time.DateOnly), "cc_number": "************4242", "amount": "10.00", "reason_code": "10.4", "reason": "Fraud"}}}}, uuid.NewString())
		require.Equal(t, http.StatusOK, status, raw)
		read, err := a.alpha.GetPayment(t.Context(), receipt.ID)
		require.NoError(t, err)
		require.Len(t, read.Refunds, 1, "the chargeback is booked")
		require.Equal(t, billing.PaymentChargeback, read.Refunds[0].Kind)
		after, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionCanceled, after.Status)
		refused, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.webhook.replay_refused"})
		require.NoError(t, err)
		require.Empty(t, refused.Items)
	})

	t.Run("OR-J15-S73/prune-keeps-an-nmi-sale-settled-refunded-or-voided-after-its-window", func(t *testing.T) {
		for name, later := range map[string]func(p *nmiJournal, sale string){
			"settled":  func(p *nmiJournal, sale string) { p.core.Settle(sale) },
			"refunded": func(p *nmiJournal, sale string) { p.core.Refund(sale, 500) },
			"voided":   func(p *nmiJournal, sale string) { p.core.Void(sale) },
		} {
			t.Run(name, func(t *testing.T) {
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
				// NMI's report proves absence in the window: a receipt it does
				// not list is excess.
				p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Hidden = true })
				require.Equal(t, []uuid.UUID{receipt.ID.UUID()}, excess(nmiPrune(t, a, since, until, nil)))
				p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Hidden = false })
				// Changed after the window, the sale's last change is outside
				// it; the read reaches it and the receipt stays.
				a.clock.Advance(2 * time.Hour)
				later(p, sale.TransactionID)
				require.Empty(t, excess(nmiPrune(t, a, since, until, nil)))
				zero := 0
				require.Zero(t, nmiPrune(t, a, since, until, &zero).Payments)
				kept, err := a.alpha.GetPayment(t.Context(), receipt.ID)
				require.NoError(t, err)
				require.Equal(t, sale.TransactionID, kept.TransactionID)
			})
		}
	})

	t.Run("OR-J15-S74/an-nmi-refresh-backlog-is-read-a-link-at-a-time-by-last-change", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		start := *sub.CurrentPeriodStartsAt
		sales := p.core.Sales()
		require.Len(t, sales, 1)
		// Four days unread; the sale settles on the second, moving its last
		// change there.
		a.clock.Advance(36 * time.Hour)
		p.core.Settle(sales[0].TransactionID)
		a.clock.Advance(60 * time.Hour)
		rt := embedded.Graph(a.engine).Runtime
		refresh := &riverjobs.ProviderRefreshWorker{DB: rt.DB, Config: rt.Config, Clock: a.clock, Merchants: rt.Merchants, Contacts: rt.Contacts,
			StripeClients: rt.StripeClients, NMIClients: rt.NMIClients, Batch: 24 * time.Hour, Overlap: time.Hour, Log: slog.Default()}
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		psp, mid := psps.Items[0].ID.UUID(), a.alpha.MerchantID().UUID()
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		scoped := func(fn func(context.Context, *gen.Queries) error) {
			t.Helper()
			require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), a.alpha.MerchantID()), func(ctx context.Context) error { return fn(ctx, database.Gen(ctx)) }))
		}
		scoped(func(ctx context.Context, q *gen.Queries) error {
			return q.UpsertPSPAppliedRefreshWatermark(ctx, gen.UpsertPSPAppliedRefreshWatermarkParams{MerchantID: mid, PspID: psp, WatermarkAt: start})
		})
		applied := func() (at time.Time) {
			scoped(func(ctx context.Context, q *gen.Queries) (err error) {
				at, err = q.GetPSPAppliedRefreshWatermark(ctx, gen.GetPSPAppliedRefreshWatermarkParams{MerchantID: mid, PspID: psp})
				return err
			})
			return at.UTC()
		}
		run := func() error {
			return refresh.Work(t.Context(), &river.Job[riverjobs.ProviderRefreshRequestArgs]{JobRow: &rivertype.JobRow{}, Args: riverjobs.ProviderRefreshRequestArgs{MerchantID: mid}})
		}
		// The refresh's window reads: those with an end, and those through
		// now.
		reads := func(from int) (bounded, open []string) {
			for _, q := range p.core.Searches()[from:] {
				if q.Get("start_date") == "" || q.Get("subscription_id") != "" || q.Get("transaction_id") != "" || q.Get("invoice_id") != "" {
					continue
				}
				if q.Get("end_date") != "" {
					bounded = append(bounded, q.Get("end_date"))
				} else {
					open = append(open, q.Get("start_date"))
				}
			}
			return bounded, open
		}
		seen := len(p.core.Searches())

		// A run reads one link, to its end, and keeps its progress.
		var snooze *rivertype.JobSnoozeError
		require.ErrorAs(t, run(), &snooze, "a backlog is read a link a run")
		require.Equal(t, time.Second, snooze.Duration)
		first := start.Add(23 * time.Hour)
		require.True(t, applied().Equal(first), "applied through %s, want the first link's end %s", applied(), first)
		bounded, open := reads(seen)
		require.Equal(t, []string{first.Add(-time.Second).UTC().Format(nmi.QueryTimeFormat)}, bounded)
		require.Empty(t, open)

		runs := 1
		for {
			err := run()
			runs++
			if err == nil {
				break
			}
			require.ErrorAs(t, err, &snooze)
			require.Equal(t, time.Second, snooze.Duration)
			require.Less(t, runs, 10)
		}
		require.GreaterOrEqual(t, runs, 4, "four days of day-long links")
		horizon := applied()
		require.WithinDuration(t, a.clock.Now().Add(-5*time.Minute), horizon, time.Second, "the chain reaches the horizon")
		bounded, open = reads(seen)
		started := a.clock.Now().Truncate(time.Second)
		require.Len(t, bounded, runs, "each link ends at its end, and the chain's end at its own start")
		require.Equal(t, started.Format(nmi.QueryTimeFormat), bounded[len(bounded)-1])
		require.Equal(t, []string{started.Add(time.Second).Format(nmi.QueryTimeFormat)}, open, "then the tail through now")
		receipts, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
		require.NoError(t, err)
		require.Len(t, receipts.Items, 1, "the settled sale, read in the link of its last change, is booked once")
	})

	t.Run("OR-J38-S44/a-ccbill-refresh-reads-datalink-a-day-at-a-time-and-loses-nothing", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		w := newWorld(t, func(w *world) { declareCCBill(w, dataLink.URL()) })
		now := w.clock.Now().Truncate(time.Second)
		// Nine members, each rebilled once, over nine days at drifting
		// times of day.
		var times []time.Time
		for i := range 9 {
			times = append(times, now.Add(-time.Hour-time.Duration(i)*(26*time.Hour+37*time.Minute+13*time.Second)))
		}
		members, rebills := ccbillRebills(t, w, dataLink, times)
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
		var exports []ccbillmock.Request
		for _, e := range dataLink.Requests() {
			if !e.Start.IsZero() {
				require.LessOrEqual(t, e.End.Sub(e.Start), 24*time.Hour-time.Second)
			}
			if strings.Contains(e.Types, "CHARGEBACK") {
				exports = append(exports, e)
			}
		}
		slices.SortFunc(exports, func(a, b ccbillmock.Request) int { return a.Start.Compare(b.Start) })
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

	t.Run("OR-J38-S45/a-live-ccbill-refresh-reads-a-day-an-hour-and-records-every-rebill", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		// Its own account: DataLink's hour is the account's, kept in Redis.
		account := fmt.Sprintf("%06d", 100000+rand.IntN(900000))
		w := newWorld(t, func(w *world) { declareCCBillAccount(w, account, dataLink.URL()) })
		dataLink.SetClock(w.clock.Now)
		now := w.clock.Now().Truncate(time.Second)
		times := []time.Time{now.Add(-50 * time.Hour), now.Add(-27 * time.Hour), now.Add(-3 * time.Hour)}
		members, _ := ccbillRebills(t, w, dataLink, times)
		psp, merchantPass, run := liveCCBillRefresh(t, w)
		// The merchant's refresh hands the live account to a refresh of its
		// own, which reads DataLink a request per hour.
		require.NoError(t, merchantPass())
		require.Empty(t, liveRequestsAnHourApart(t, dataLink), "the merchant's refresh reads no DataLink")
		jobs, err := hostFleet(t, w).JobList(t.Context(), river.NewJobListParams().Kinds(riverjobs.KindPacedProviderRefresh))
		require.NoError(t, err)
		require.Len(t, jobs.Jobs, 1)
		require.Contains(t, string(jobs.Jobs[0].EncodedArgs), psp.String())

		// Another system's request makes CCBill refuse the next within the
		// hour: the refresh waits for the hour and reads nothing.
		other := ccbill.NewDataLinkClient(slog.Default(), &config.CCBillConfig{ClientAccNum: account, ClientSubAcc: "0001", DataLinkUsername: "other", DataLinkPassword: "other"})
		other.BaseURL, other.Pacing = dataLink.URL(), ccbill.Pacing{Slots: abusestate.New(slog.Default(), nil), Now: w.clock.Now}
		_, err = other.FetchActiveMembers(t.Context())
		require.NoError(t, err)
		var snooze *rivertype.JobSnoozeError
		require.ErrorAs(t, run(), &snooze)
		require.Equal(t, time.Hour, snooze.Duration)
		require.Equal(t, []string{"live DataLink request within 60 minutes of the last"}, dataLink.Unexpected())
		for txn, customer := range members {
			require.Zero(t, ccbillPayments(t, w, customer)[txn], "nothing is read yet")
		}
		w.clock.Advance(snooze.Duration)

		// A day of transactions an hour, from 90 days back, then the roster.
		passes := drainPaced(t, w, run)
		for txn, customer := range members {
			require.EqualValues(t, 10_000_000, ccbillPayments(t, w, customer)[txn], "the paced refresh records rebill %s", txn)
		}
		live := liveRequestsAnHourApart(t, dataLink)
		require.Equal(t, "ACTIVEMEMBERS", live[len(live)-1].Types, "the roster is read once the days are")
		require.Len(t, live, passes+2, "one request a pass, the other system's first")
		require.Equal(t, []string{"live DataLink request within 60 minutes of the last"}, dataLink.Unexpected())
	})

	t.Run("OR-J38-S46/a-live-ccbill-import-reads-a-day-an-hour-and-resumes-each-time", func(t *testing.T) {
		iw := bootImportWorld(t, "ccbill")
		iw.ccbill.SetClock(iw.clock.Now)
		now := iw.clock.Now().Truncate(time.Second)
		var txns []string
		for i, at := range []time.Time{now.Add(-70 * time.Hour), now.Add(-40 * time.Hour), now.Add(-2 * time.Hour)} {
			next := at.Add(720 * time.Hour)
			sub := iw.ccbill.AddMember(ccbillmock.Member{Username: fmt.Sprintf("member-%d", i), Email: fmt.Sprintf("member-%d@critical.invalid", i), Signup: at.Add(-720 * time.Hour), Rebill: &next})
			txns = append(txns, iw.ccbill.AddRebill(ccbillmock.Rebill{SubscriptionID: sub, At: at, Amount: "10.00"}))
		}
		since := now.Add(-4 * 24 * time.Hour)
		// No fleet runs: the pull's job is run here, as a live account's.
		imp, err := iw.api.CreatePSPImport(t.Context(), billing.CreatePSPImportParams{PSP: billing.PSPRef{Key: "ccbill"}, HistorySince: &since})
		require.NoError(t, err)
		im := embedded.Graph(iw.engine).Runtime.PSPImporter()
		im.WireDataLink = liveDataLink
		pull := &riverjobs.PSPImportWorker{Importer: func() *pspimport.Importer { return im }}
		job := &river.Job[pspimport.JobArgs]{JobRow: &rivertype.JobRow{Attempt: 1, MaxAttempts: pspimport.JobAttempts},
			Args: pspimport.JobArgs{MerchantID: iw.alpha.MerchantID().UUID(), ImportID: imp.ID.UUID(), Step: pspimport.StepPull}}
		through := since
		passes := 0
		for err := pull.Work(t.Context(), job); err != nil; err = pull.Work(t.Context(), job) {
			var snooze *rivertype.JobSnoozeError
			require.ErrorAs(t, err, &snooze, "a paced pull waits; it does not fail")
			view, err := iw.alpha.GetPSPImport(t.Context(), imp.ID)
			require.NoError(t, err)
			require.Equal(t, billing.PSPImportPulling, view.Status)
			if view.HistoryThrough != nil {
				require.LessOrEqual(t, view.HistoryThrough.Sub(through), 24*time.Hour, "one day a request")
				through = *view.HistoryThrough
			}
			passes++
			require.Less(t, passes, 20)
			iw.clock.Advance(snooze.Duration)
		}
		view, err := iw.alpha.GetPSPImport(t.Context(), imp.ID)
		require.NoError(t, err)
		require.Equal(t, billing.PSPImportPulled, view.Status)
		require.True(t, view.PulledAt.Equal(*view.HistoryThrough), "the history is read through the pull")
		records := iw.records(t, imp.ID)
		for _, txn := range txns {
			require.Contains(t, records, txn, "the paced pull stages rebill %s", txn)
		}
		require.GreaterOrEqual(t, passes, 4, "the roster, then each day, an hour apart")
		liveRequestsAnHourApart(t, iw.ccbill)
	})

	t.Run("OR-J38-S47/a-missed-ccbill-cancellation-read-on-a-paced-day-keeps-the-paid-period", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		account := fmt.Sprintf("%06d", 100000+rand.IntN(900000))
		w := newWorld(t, func(w *world) {
			declareCCBillAccount(w, account, dataLink.URL())
			w.posture = "full"
		})
		// Lifecycle repairs are armed: what keeps the paid period is the
		// cancellation's meaning, not a held write.
		armWorldDestructive(t, w, true)
		dataLink.SetClock(w.clock.Now)
		now := w.clock.Now().Truncate(time.Second)
		price := ccbillMemberPrice(t, w.alpha)
		customer := billing.CustomerID(uuid.New())
		sub, start, paid := "0223344556600000777", now.Add(-20*24*time.Hour), now.Add(10*24*time.Hour)
		res, err := w.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"},
			Customers: []billing.DeclaredCustomer{{Customer: customer}},
			Subscriptions: []billing.DeclaredSubscription{{SourceID: "ccbill-member", Customer: customer, Price: price, Rail: "ccbill", RailSubscriptionID: sub, StartedAt: start, PaidThrough: &paid,
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: sub, Amount: 10_000_000, Currency: "USD", IntervalHours: 720, FirstDueAt: paid}}},
			Transactions: []billing.DeclaredTransaction{{RailSubscriptionID: sub, TransactionID: "0812233445500000777", Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: start}}})
		require.NoError(t, err)
		require.Empty(t, res.Blocked, "%+v", res.Reasons)
		require.True(t, entitledTo(t, w.alpha, customer, "ccbill:member"))
		// The member stopped rebilling three hours ago; its webhook never came.
		dataLink.AddMember(ccbillmock.Member{SubscriptionID: sub, Username: "member", Email: "member@critical.invalid", Signup: start, Expiry: &paid})
		dataLink.AddCancellation(ccbillmock.Cancellation{SubscriptionID: sub, At: now.Add(-3 * time.Hour), Expires: paid})
		psp, _, run := liveCCBillRefresh(t, w)
		database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
		require.NoError(t, err)
		require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), w.alpha.MerchantID()), func(ctx context.Context) error {
			return database.Gen(ctx).UpsertPSPRefreshWatermark(ctx, gen.UpsertPSPRefreshWatermarkParams{MerchantID: w.alpha.MerchantID().UUID(), PspID: psp, WatermarkAt: now.Add(-30 * time.Hour)})
		}))

		// Days of transactions only, no roster: the CANCELLATION row alone
		// tells the pull, which keeps the paid period.
		require.Positive(t, drainPaced(t, w, run))
		after := onlySubscription(t, w.alpha, customer)
		require.NotNil(t, after.CanceledAt, "the cancellation is mirrored: status %s", after.Status)
		require.NotNil(t, after.CurrentPeriodEndsAt)
		require.True(t, after.CurrentPeriodEndsAt.Equal(ccbill.DayEnd(paid)), "paid through %s, the end of its expire date", after.CurrentPeriodEndsAt)
		require.True(t, entitledTo(t, w.alpha, customer, "ccbill:member"), "the time paid for is kept")
		require.Len(t, liveRequestsAnHourApart(t, dataLink), 3, "two days of transactions, then the roster")
		require.Empty(t, dataLink.Unexpected())
	})

	t.Run("OR-J38-S73/a-ccbill-status-read-answering-1-keeps-access-to-ccbills-expiry", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		account := fmt.Sprintf("%06d", 100000+rand.IntN(900000))
		w := newWorld(t, func(w *world) {
			declareCCBillAccount(w, account, dataLink.URL())
			w.posture = "full"
		})
		armWorldDestructive(t, w, true)
		dataLink.SetClock(w.clock.Now)
		now := w.clock.Now().Truncate(time.Second)
		customer, sub, lapsed := importCCBillMember(t, w, "0223344556600000778", now.Add(-3*24*time.Hour))
		// CCBill rebilled the member and the member then canceled: neither
		// webhook came, and CCBill holds the time paid for.
		expires := lapsed.Add(720 * time.Hour)
		dataLink.AddMember(ccbillmock.Member{SubscriptionID: sub, Username: "member", Email: "member@critical.invalid", Signup: lapsed.Add(-720 * time.Hour), Expiry: &expires})
		// The due pass asks CCBill about the lapsed period, keeping access.
		rt := embedded.Graph(w.engine).Runtime
		lc := subscriptions.NewSubscriptionLifecycleService(rt.DB, nil, nil, nil, nil, nil, w.clock)
		lc.SetConfig(rt.Config)
		id := onlySubscription(t, w.alpha, customer).ID.UUID()
		require.NoError(t, rt.DB.RunInMerchantScope(t.Context(), w.alpha.MerchantID(), "overdue renewal", func(ctx context.Context) error {
			_, err := lc.AskProviderOverdueRenewal(ctx, rt.DB, id, lapsed)
			return err
		}))
		require.Equal(t, billing.SubscriptionUnverified, onlySubscription(t, w.alpha, customer).Status)
		require.True(t, entitledTo(t, w.alpha, customer, "ccbill:member"), "unverified keeps access")

		// A live account's unverified row is read by a status read, which
		// answers 1 (canceled, access left) and the expiration date.
		_, merchantPass, _ := liveCCBillRefresh(t, w)
		require.NoError(t, merchantPass())
		require.NotEmpty(t, dataLink.Calls())
		for _, call := range dataLink.Calls() {
			require.Equal(t, "viewSubscriptionStatus", call.Form.Get("action"))
		}
		after := onlySubscription(t, w.alpha, customer)
		require.Equal(t, billing.SubscriptionCanceled, after.Status)
		require.NotNil(t, after.CurrentPeriodEndsAt)
		require.True(t, after.CurrentPeriodEndsAt.Equal(expires), "paid through %s", after.CurrentPeriodEndsAt)
		require.True(t, entitledTo(t, w.alpha, customer, "ccbill:member"), "the time CCBill holds paid is kept")
		require.Empty(t, liveRequestsAnHourApart(t, dataLink), "a status read is no DataLink export")
	})

	t.Run("OR-J38-S74/a-ccbill-rebill-and-cancellation-read-together-keep-the-rebilled-period", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		account := fmt.Sprintf("%06d", 100000+rand.IntN(900000))
		w := newWorld(t, func(w *world) {
			declareCCBillAccount(w, account, dataLink.URL())
			w.posture = "full"
			// The world runs a day behind the database's clock, which dates
			// no payment in its future.
			w.clock.Advance(-24 * time.Hour)
		})
		armWorldDestructive(t, w, true)
		dataLink.SetClock(w.clock.Now)
		// Paid an hour ahead, the member is active.
		customer, sub, due := importCCBillMember(t, w, "0223344556600000779", w.clock.Now().Truncate(time.Second).Add(time.Hour))
		// CCBill rebilled the member when due, and the member canceled ten
		// minutes later; neither webhook came. One day's export holds both.
		w.clock.Advance(13 * time.Hour)
		now := w.clock.Now().Truncate(time.Second)
		paid := due.Add(720 * time.Hour)
		dataLink.AddMember(ccbillmock.Member{SubscriptionID: sub, Username: "member", Email: "member@critical.invalid", Signup: due.Add(-720 * time.Hour), Expiry: &paid})
		rebill := dataLink.AddRebill(ccbillmock.Rebill{SubscriptionID: sub, At: due, Amount: "10.00"})
		dataLink.AddCancellation(ccbillmock.Cancellation{SubscriptionID: sub, At: due.Add(10 * time.Minute), Expires: paid})
		psp, _, run := liveCCBillRefresh(t, w)
		database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
		require.NoError(t, err)
		require.NoError(t, database.RunInMerchantConn(merchant.WithID(t.Context(), w.alpha.MerchantID()), func(ctx context.Context) error {
			return database.Gen(ctx).UpsertPSPRefreshWatermark(ctx, gen.UpsertPSPRefreshWatermarkParams{MerchantID: w.alpha.MerchantID().UUID(), PspID: psp, WatermarkAt: now.Add(-30 * time.Hour)})
		}))

		require.Positive(t, drainPaced(t, w, run))
		require.EqualValues(t, 10_000_000, ccbillPayments(t, w, customer)[rebill], "the pull books the rebill")
		after := onlySubscription(t, w.alpha, customer)
		require.NotNil(t, after.CanceledAt, "the cancellation is mirrored: status %s", after.Status)
		require.NotNil(t, after.CurrentPeriodEndsAt)
		require.True(t, after.CurrentPeriodEndsAt.Equal(ccbill.DayEnd(paid)), "paid through %s, the end of the rebilled period", after.CurrentPeriodEndsAt)
		require.True(t, entitledTo(t, w.alpha, customer, "ccbill:member"), "the rebilled period is kept")
		require.Empty(t, dataLink.Unexpected())
	})

	t.Run("OR-J28-S11/a-ccbill-account-another-merchant-declares-is-refused-before-a-probe", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		account := fmt.Sprintf("%06d", 100000+rand.IntN(900000))
		a, _ := newPSPWorld(t, func(w *world) {
			w.deps.ProviderEndpoints = openrails.ProviderEndpoints{CCBillDataLinkURL: dataLink.URL()}
		})
		dataLink.SetClock(a.clock.Now)
		create := func(client *openrails.Client, subacc string) error {
			_, err := client.CreatePSP(t.Context(), billing.CreatePSPParams{Key: "ccbill", Rail: billing.RailCCBill, Environment: billing.EnvironmentSandbox,
				PSPAccount:  billing.PSPAccount{ClientAccnum: account, ClientSubacc: subacc},
				Credentials: map[string]string{"salt": "critical-ccbill-salt", "datalink_username": "critical-datalink", "datalink_password": "critical-datalink-password"}})
			return err
		}
		require.NoError(t, create(a.alphaRemote, "0001"), "alpha proves the account")
		probed := len(dataLink.Calls())
		require.Positive(t, probed, "the probe is a status read")
		// Another subaccount of alpha's CCBill account: DataLink's credentials
		// and its hour are the account's, so beta's claim is refused unread.
		status, code := refusalCode(t, create(a.betaRemote, "0002"))
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, billing.CodePSPExists, code)
		require.Len(t, dataLink.Calls(), probed, "beta's claim reached no CCBill")
	})

	t.Run("OR-J28-S15/a-strangers-refused-ccbill-logins-hold-only-their-own-login", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		dataLink.RequireCredentials("critical-datalink", "critical-datalink-password", false)
		account := fmt.Sprintf("%06d", 100000+rand.IntN(900000))
		a, _ := newPSPWorld(t, func(w *world) {
			w.deps.ProviderEndpoints = openrails.ProviderEndpoints{CCBillDataLinkURL: dataLink.URL()}
		})
		create := func(client *openrails.Client, subacc, username, password string) error {
			_, err := client.CreatePSP(t.Context(), billing.CreatePSPParams{Key: "ccbill", Rail: billing.RailCCBill, Environment: billing.EnvironmentSandbox,
				PSPAccount:  billing.PSPAccount{ClientAccnum: account, ClientSubacc: subacc},
				Credentials: map[string]string{"salt": "critical-ccbill-salt", "datalink_username": username, "datalink_password": password}})
			return err
		}
		// CCBill locks a login after three refused ones in an hour: a
		// stranger's login is refused twice, and its third is not sent.
		for range 2 {
			status, code := refusalCode(t, create(a.alphaRemote, "0001", "stranger", "guess"))
			require.Equal(t, http.StatusBadRequest, status)
			require.Equal(t, billing.CodePSPCredentialsRejected, code)
		}
		var refusal *billing.StatusError
		require.ErrorAs(t, create(a.alphaRemote, "0001", "stranger", "guess"), &refusal)
		require.Equal(t, http.StatusTooManyRequests, refusal.Status)
		require.Equal(t, billing.CodePSPProbeLimited, refusal.Code)
		require.Equal(t, "3600", refusal.RetryAfter)
		require.Len(t, dataLink.Calls(), 2, "the third probe reached no CCBill")
		// The account's owner, on its own login, is not held.
		require.NoError(t, create(a.betaRemote, "0002", "critical-datalink", "critical-datalink-password"))
		require.Len(t, dataLink.Calls(), 3)
	})

	t.Run("OR-J28-S14/a-ccbill-datalink-login-ccbill-refuses-is-not-declared", func(t *testing.T) {
		dataLink := ccbillmock.New()
		t.Cleanup(dataLink.Close)
		dataLink.RequireCredentials("critical-datalink", "critical-datalink-password", false)
		a, _ := newPSPWorld(t, func(w *world) {
			w.deps.ProviderEndpoints = openrails.ProviderEndpoints{CCBillDataLinkURL: dataLink.URL()}
		})
		create := func(password string) error {
			_, err := a.alphaRemote.CreatePSP(t.Context(), billing.CreatePSPParams{Key: "ccbill", Rail: billing.RailCCBill, Environment: billing.EnvironmentSandbox,
				PSPAccount:  billing.PSPAccount{ClientAccnum: fmt.Sprintf("%06d", 100000+rand.IntN(900000)), ClientSubacc: "0001"},
				Credentials: map[string]string{"salt": "critical-ccbill-salt", "datalink_username": "critical-datalink", "datalink_password": password}})
			return err
		}
		// A status read of a subscription no account holds: -1 refuses the
		// login, -3 (no record) proves it.
		status, code := refusalCode(t, create("wrong"))
		require.Equal(t, http.StatusBadRequest, status)
		require.Equal(t, billing.CodePSPCredentialsRejected, code)
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		for _, psp := range psps.Items {
			require.NotEqual(t, billing.RailCCBill, psp.Rail, "a refused login declares nothing")
		}
		require.NoError(t, create("critical-datalink-password"))
		calls := dataLink.Calls()
		require.GreaterOrEqual(t, len(calls), 2)
		for _, call := range calls[:2] {
			require.Equal(t, "viewSubscriptionStatus", call.Form.Get("action"))
		}
	})

	t.Run("OR-J28-S13/a-ccbill-account-archived-or-in-the-other-money-class-is-declarable-by-another-merchant", func(t *testing.T) {
		for name, release := range map[string]func(*testing.T, *authorityWorld, *billing.PSP){
			"archived": func(t *testing.T, a *authorityWorld, psp *billing.PSP) {
				_, err := a.alphaRemote.UpdatePSP(t.Context(), psp.ID, billing.UpdatePSPParams{Archived: true, AllowLast: true})
				require.NoError(t, err)
			},
			"live": func(t *testing.T, a *authorityWorld, psp *billing.PSP) {
				// Alpha's account as a live merchant holds it: no live PSP is
				// declared beside a fake DataLink, so its document and row are
				// written here.
				source, ok := embedded.Graph(a.engine).Runtime.MerchantConfig.Source().(*merchantdocs.VaultSource)
				require.True(t, ok)
				set, err := source.Load(t.Context(), a.alpha.MerchantID())
				require.NoError(t, err)
				doc := set.PSPs["ccbill"]
				doc.Value.Environment = "live"
				_, err = source.PutPSP(t.Context(), a.alpha.MerchantID(), "ccbill", doc.Value, doc.Revision)
				require.NoError(t, err)
				_, err = a.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{a.schema, "psps"}.Sanitize()+" SET environment = 'live' WHERE merchant_id = $1 AND id = $2", a.alpha.MerchantID().UUID(), psp.ID.UUID())
				require.NoError(t, err)
			},
		} {
			t.Run(name, func(t *testing.T) {
				dataLink := ccbillmock.New()
				t.Cleanup(dataLink.Close)
				account := fmt.Sprintf("%06d", 100000+rand.IntN(900000))
				a, _ := newPSPWorld(t, func(w *world) {
					w.deps.ProviderEndpoints = openrails.ProviderEndpoints{CCBillDataLinkURL: dataLink.URL()}
				})
				dataLink.SetClock(a.clock.Now)
				create := func(client *openrails.Client, subacc string) (*billing.PSP, error) {
					return client.CreatePSP(t.Context(), billing.CreatePSPParams{Key: "ccbill", Rail: billing.RailCCBill, Environment: billing.EnvironmentSandbox,
						PSPAccount:  billing.PSPAccount{ClientAccnum: account, ClientSubacc: subacc},
						Credentials: map[string]string{"salt": "critical-ccbill-salt", "datalink_username": "critical-datalink", "datalink_password": "critical-datalink-password"}})
				}
				psp, err := create(a.alphaRemote, "0001")
				require.NoError(t, err)
				release(t, a, psp)
				_, err = create(a.betaRemote, "0002")
				require.NoError(t, err, "alpha holds no current sandbox PSP on the account")
			})
		}
	})
}
