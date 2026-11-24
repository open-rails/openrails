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
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/ccbillmock"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	embedded "github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
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

// liveDataLink makes a sandbox CCBill PSP's DataLink clients ask as a live
// account's do: no live PSP is declared beside a fake DataLink.
func liveDataLink(dl *ccbill.DataLinkClient) { dl.DevMode = false }

// liveCCBillRefresh is the refresh of alpha's one CCBill PSP with its
// DataLink asking as a live account's: the merchant's pass, and the PSP's
// paced pass, run as their workers run them.
func liveCCBillRefresh(t *testing.T, w *world) (uuid.UUID, func() error, func() error) {
	t.Helper()
	rt := embedded.Graph(w.engine).Runtime
	refresh := &riverjobs.ProviderRefreshWorker{DB: rt.DB, Config: rt.Config, Clock: w.clock, Merchants: rt.Merchants, Contacts: rt.Contacts,
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
			w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
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
		require.True(t, after.CurrentPeriodEndsAt.Equal(paid), "paid through %s", after.CurrentPeriodEndsAt)
		require.True(t, entitledTo(t, w.alpha, customer, "ccbill:member"), "the time paid for is kept")
		require.Len(t, liveRequestsAnHourApart(t, dataLink), 3, "two days of transactions, then the roster")
		require.Empty(t, dataLink.Unexpected())
	})
}
