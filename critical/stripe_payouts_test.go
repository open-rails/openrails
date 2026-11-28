//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
	"github.com/open-rails/openrails/internal/stripemock"
)

// TestStripePayouts is the general ledger's deposits and reserves: each
// Stripe payout read from Stripe and booked from its PSP's cash clearing
// through cash in transit to its bank account, with the reserve lines paid
// out in it.
func TestStripePayouts(t *testing.T) {
	t.Run("OR-J40-S60/a-payout-and-its-reserve-lines-are-read-exactly-and-only-read", func(t *testing.T) {
		m := payoutMembership(t)
		w, mid := m.a.world, m.a.alpha.MerchantID().UUID()
		fleet := quietFleet(t, w)
		core := m.p.core
		require.False(t, runPass(t, fleet))

		// Stripe holds back a cent 101 times and pays the rest of the $10.00
		// charge, less its 59-cent fee, out automatically: 840 cents.
		for range 101 {
			core.ReserveHold(1, "usd")
		}
		po := core.Payout(840, "usd", m.a.clock.Now().Add(48*time.Hour), true)
		id := po["id"].(string)
		core.SetPayoutStatus(id, "paid")
		type read struct {
			path    string
			query   url.Values
			version string
		}
		var mu sync.Mutex
		var reads []read
		core.Intercept(func(r *http.Request) bool {
			return strings.HasPrefix(r.URL.Path, "/v1/payouts/") || r.URL.Path == "/v1/balance_transactions"
		}, func(r *http.Request, serve func() *http.Response) (*http.Response, error) {
			mu.Lock()
			reads = append(reads, read{r.Method + " " + r.URL.Path, r.URL.Query(), r.Header.Get(stripeapi.VersionHeader)})
			mu.Unlock()
			return serve(), nil
		})
		writes := len(core.Mutations("/v1/"))
		m.notice(t, "evt_wire_paid", "payout.paid", po)
		require.False(t, runPass(t, fleet))

		// The payout with its balance transactions and bank account expanded,
		// then its reserve lines a hundred at a time, at the pinned version;
		// nothing is written.
		require.Len(t, reads, 3)
		require.Equal(t, "GET /v1/payouts/"+id, reads[0].path)
		require.ElementsMatch(t, []string{"balance_transaction", "failure_balance_transaction", "destination"}, reads[0].query["expand[]"])
		require.Equal(t, "GET /v1/balance_transactions", reads[1].path)
		require.Equal(t, url.Values{"payout": {id}, "type": {"reserved_funds"}, "limit": {"100"}}, reads[1].query)
		require.Equal(t, "GET /v1/balance_transactions", reads[2].path)
		require.Equal(t, []string{id}, reads[2].query["payout"])
		require.NotEmpty(t, reads[2].query.Get("starting_after"))
		for _, r := range reads {
			require.Equal(t, stripeapi.APIVersion, r.version)
		}
		require.Len(t, core.Mutations("/v1/"), writes, "a payout read writes nothing to Stripe")

		// Cents are micros times ten thousand: 840 cents out and in, a cent of
		// reserve 10000 micros, 101 of them 1010000.
		require.Equal(t, [2]int64{8_400_000, 8_400_000}, feeTotal(t, w, mid, "cash_in_transit", "USD"))
		require.Equal(t, [2]int64{0, 8_400_000}, feeTotal(t, w, mid, "bank_cash", "USD"))
		require.Equal(t, [2]int64{0, 1_010_000}, feeTotal(t, w, mid, "processor_reserve", "USD"))
		require.Equal(t, 101, feeTransfers(t, w, mid, "reserve_held"))
		var each int64
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT max(amount) FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND transfer_type='reserve_held'", mid).Scan(&each))
		require.Equal(t, int64(10_000), each)
		require.Equal(t, [2]int64{590_000 + 8_400_000 + 1_010_000, 10_000_000}, feeTotal(t, w, mid, "cash_clearing", "USD"))
		require.Zero(t, payoutReads(t, w, mid))
	})

	t.Run("OR-J40-S61/a-payout-lands-a-reserve-is-held-and-released-and-a-failed-payout-comes-back", func(t *testing.T) {
		m := payoutMembership(t, newYorkBooks)
		w, mid := m.a.world, m.a.alpha.MerchantID().UUID()
		fleet := quietFleet(t, w)
		core := m.p.core
		zone, err := time.LoadLocation("America/New_York")
		require.NoError(t, err)
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{590_000, 10_000_000}, feeTotal(t, w, mid, "cash_clearing", "USD"))

		// Stripe holds a dollar of the charge in reserve and pays $8.41 out
		// automatically, arriving in two days: it leaves cash clearing for
		// cash in transit, and is read again until it is paid.
		core.ReserveHold(100, "usd")
		arrival := m.a.clock.Now().Add(48 * time.Hour)
		po1 := core.Payout(841, "usd", arrival, true)
		m.notice(t, "evt_po1_created", "payout.created", po1)
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{0, 8_410_000}, feeTotal(t, w, mid, "cash_in_transit", "USD"))
		require.Equal(t, [2]int64{0, 0}, feeTotal(t, w, mid, "bank_cash", "USD"))
		require.Equal(t, 1, payoutReads(t, w, mid), "a payout not yet paid is read again")

		// Paid: it lands in its bank account on its arrival date in the
		// merchant's New York books, and its reserve line is held.
		m.notice(t, "evt_po1_paid", "payout.paid", core.SetPayoutStatus(po1["id"].(string), "paid"))
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{8_410_000, 8_410_000}, feeTotal(t, w, mid, "cash_in_transit", "USD"))
		require.Equal(t, [2]int64{0, 8_410_000}, feeTotal(t, w, mid, "bank_cash", "USD"))
		require.Equal(t, [2]int64{0, 1_000_000}, feeTotal(t, w, mid, "processor_reserve", "USD"))
		require.Equal(t, [2]int64{10_000_000, 10_000_000}, feeTotal(t, w, mid, "cash_clearing", "USD"))
		require.Zero(t, payoutReads(t, w, mid))
		y, mo, d := arrival.UTC().Date()
		require.True(t, calendarutil.DayStart(y, mo, d, zone).Equal(transferAt(t, w, mid, "cash_deposited", "cash_deposited", po1["id"].(string))))
		var bankName, last4, bankCurrency string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT name, last4, currency FROM "+booksTable(w, "bank_accounts")+" WHERE merchant_id=$1 AND ref=$2", mid, stripemock.BankAccount["id"]).Scan(&bankName, &last4, &bankCurrency))
		require.Equal(t, []string{"STRIPE TEST BANK", "6789", "USD"}, []string{bankName, last4, bankCurrency})

		// A notice read again posts nothing new.
		m.notice(t, "evt_po1_again", "payout.updated", po1)
		require.False(t, runPass(t, fleet))
		for typ, n := range map[string]int{"payout_sent": 1, "cash_deposited": 1, "reserve_held": 1} {
			require.Equal(t, n, feeTransfers(t, w, mid, typ), typ)
		}

		// It fails after landing: the deposit leaves the bank and the money
		// returns to cash clearing, at the balance transaction that returned it.
		m.notice(t, "evt_po1_failed", "payout.failed", core.SetPayoutStatus(po1["id"].(string), "failed"))
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{8_410_000, 8_410_000}, feeTotal(t, w, mid, "bank_cash", "USD"))
		require.Equal(t, [2]int64{16_820_000, 16_820_000}, feeTotal(t, w, mid, "cash_in_transit", "USD"))
		require.Equal(t, [2]int64{10_000_000, 18_410_000}, feeTotal(t, w, mid, "cash_clearing", "USD"))

		// The reserve is released and paid out with the returned money in the
		// next automatic payout, $9.41.
		m.a.clock.Advance(24 * time.Hour)
		core.ReserveRelease(100, "usd", m.a.clock.Now())
		po2 := core.Payout(941, "usd", m.a.clock.Now().Add(24*time.Hour), true)
		m.notice(t, "evt_po2_paid", "payout.paid", core.SetPayoutStatus(po2["id"].(string), "paid"))
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{1_000_000, 1_000_000}, feeTotal(t, w, mid, "processor_reserve", "USD"))
		require.Equal(t, [2]int64{8_410_000, 17_820_000}, feeTotal(t, w, mid, "bank_cash", "USD"))
		require.Equal(t, [2]int64{26_230_000, 26_230_000}, feeTotal(t, w, mid, "cash_in_transit", "USD"))
		require.Equal(t, [2]int64{19_410_000, 19_410_000}, feeTotal(t, w, mid, "cash_clearing", "USD"))

		// A manual payout canceled before it is paid comes back without
		// landing; a manual payout has no reserve lines to read.
		po3 := core.Payout(500, "usd", m.a.clock.Now().Add(24*time.Hour), false)
		listed := core.Reads("/v1/balance_transactions")
		m.notice(t, "evt_po3_canceled", "payout.canceled", core.SetPayoutStatus(po3["id"].(string), "canceled"))
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{31_230_000, 31_230_000}, feeTotal(t, w, mid, "cash_in_transit", "USD"))
		require.Equal(t, [2]int64{8_410_000, 17_820_000}, feeTotal(t, w, mid, "bank_cash", "USD"))
		require.Equal(t, listed, core.Reads("/v1/balance_transactions"))
		require.Zero(t, payoutReads(t, w, mid))

		// A read Stripe fails waits; after eight failures it waits for
		// books.payout.unread, and the finding fixed reads it again.
		po4 := core.Payout(1, "usd", m.a.clock.Now().Add(24*time.Hour), false)
		core.SetPayoutStatus(po4["id"].(string), "paid")
		core.Intercept(func(r *http.Request) bool { return r.URL.Path == "/v1/payouts/"+po4["id"].(string) }, unavailable)
		m.notice(t, "evt_po4_paid", "payout.paid", po4)
		require.False(t, runPass(t, fleet), "a payout read that fails never fails the pass")
		attempts, parked := payoutRead(t, w, mid)
		require.Equal(t, 1, attempts)
		require.False(t, parked)
		_, err = w.pool.Exec(t.Context(), "UPDATE "+booksTable(w, "stripe_payout_reads")+" SET attempts=7, due_at=now() WHERE merchant_id=$1", mid)
		require.NoError(t, err)
		require.False(t, runPass(t, fleet))
		attempts, parked = payoutRead(t, w, mid)
		require.Equal(t, 8, attempts)
		require.True(t, parked)
		findings, err := m.a.alphaRemote.ListFindings(t.Context(), billing.FindingListParams{Type: "books.payout.unread"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		core.ClearIntercepts()
		_, err = m.a.alphaRemote.ResolveFinding(t.Context(), findings.Items[0].ID, billing.ResolveFindingParams{Outcome: billing.FindingApprove, Notes: "credentials fixed"})
		require.NoError(t, err)
		require.False(t, runPass(t, fleet))
		require.Equal(t, [2]int64{8_410_000, 17_830_000}, feeTotal(t, w, mid, "bank_cash", "USD"))
		require.Zero(t, payoutReads(t, w, mid))

		// Every provider figure names a source of known basis.
		require.ErrorContains(t, booksTx(t, w, func(ctx context.Context, q *gen.Queries) error {
			psp := alphaStripePSP(t, w, mid)
			_, _, err := books.New(q, mid).Post(ctx, books.Posting{Debit: books.Ref{Type: books.ProcessorReserve, PSP: &psp}, Credit: books.Ref{Type: books.CashClearing, PSP: &psp},
				Amount: 1, Currency: "USD", Type: books.ReserveHeld, EffectiveAt: time.Now(),
				Coord: books.Coord{Operation: "reserve_held", Source: "critical", SourceID: "guess"}})
			return err
		}), "source of known basis")

		// No account was opened late, and the books hold.
		var late int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "reconciliation_findings")+" WHERE merchant_id=$1 AND finding_type='books.account.opened_late'"+
			" AND subject_key LIKE ANY (ARRAY['cash_in_transit/%', 'processor_reserve/%', 'bank_cash/%'])", mid).Scan(&late))
		require.Zero(t, late)
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+mid.String()), "Ledger integrity OK")
	})

	t.Run("OR-J40-S62/after-a-known-history-the-books-balance-each-day-ledger-audit-is-clean-and-a-restore-carries-them", func(t *testing.T) {
		m := payoutMembership(t, newYorkBooks)
		w, mid := m.a.world, m.a.alpha.MerchantID().UUID()
		fleet := quietFleet(t, w)
		core := m.p.core
		zone, err := time.LoadLocation("America/New_York")
		require.NoError(t, err)
		psp := alphaStripePSP(t, w, mid)

		// Each New York day's flows on the payout accounts, as the history
		// makes them: [debits, credits] in micros.
		type key struct {
			day     string
			account string
		}
		want := map[key][2]int64{}
		flow := func(at time.Time, account string, debit, credit int64) {
			k := key{at.In(zone).Format(time.DateOnly), account}
			f := want[k]
			want[k] = [2]int64{f[0] + debit, f[1] + credit}
		}
		y0, mo0, d0 := m.a.clock.Now().In(zone).Date()
		day := func(n int, hour int) time.Time {
			return calendarutil.DayStart(y0, mo0, d0+n, zone).Add(time.Duration(hour) * time.Hour)
		}
		start := day(0, 0)
		require.False(t, runPass(t, fleet))
		var sale, charged time.Time
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT (SELECT effective_at FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND transfer_type='cash_received'),"+
			" (SELECT effective_at FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND transfer_type='processor_fee')", mid).Scan(&sale, &charged))
		flow(sale, "cash_clearing", 10_000_000, 0)
		flow(charged, "cash_clearing", 0, 590_000)
		flow(charged, "processor_fees", 590_000, 0)

		// Day 1: a dollar held, $8.41 paid out to arrive on day 3.
		m.a.clock.Advance(day(1, 10).Sub(m.a.clock.Now()))
		core.ReserveHold(100, "usd")
		flow(m.a.clock.Now(), "cash_clearing", 0, 1_000_000)
		flow(m.a.clock.Now(), "processor_reserve", 1_000_000, 0)
		po1 := core.Payout(841, "usd", day(3, 12), true)
		flow(m.a.clock.Now(), "cash_clearing", 0, 8_410_000)
		flow(m.a.clock.Now(), "cash_in_transit", 8_410_000, 0)
		m.notice(t, "evt_h1_created", "payout.created", po1)
		require.False(t, runPass(t, fleet))

		// Day 3: it lands.
		m.a.clock.Advance(day(3, 9).Sub(m.a.clock.Now()))
		m.notice(t, "evt_h1_paid", "payout.paid", core.SetPayoutStatus(po1["id"].(string), "paid"))
		require.False(t, runPass(t, fleet))
		flow(day(3, 0), "cash_in_transit", 0, 8_410_000)
		flow(day(3, 0), "bank_cash", 8_410_000, 0)

		// Day 4: the reserve released and paid out, landing on day 5.
		m.a.clock.Advance(day(4, 11).Sub(m.a.clock.Now()))
		core.ReserveRelease(100, "usd", m.a.clock.Now())
		flow(m.a.clock.Now(), "cash_clearing", 1_000_000, 0)
		flow(m.a.clock.Now(), "processor_reserve", 0, 1_000_000)
		po2 := core.Payout(100, "usd", day(5, 12), true)
		flow(m.a.clock.Now(), "cash_clearing", 0, 1_000_000)
		flow(m.a.clock.Now(), "cash_in_transit", 1_000_000, 0)
		m.notice(t, "evt_h2_created", "payout.created", po2)
		require.False(t, runPass(t, fleet))
		m.a.clock.Advance(day(5, 9).Sub(m.a.clock.Now()))
		m.notice(t, "evt_h2_paid", "payout.paid", core.SetPayoutStatus(po2["id"].(string), "paid"))
		require.False(t, runPass(t, fleet))
		flow(day(5, 0), "cash_in_transit", 0, 1_000_000)
		flow(day(5, 0), "bank_cash", 1_000_000, 0)

		// Day 6: $2.00 paid out by hand fails before it lands and comes back.
		m.a.clock.Advance(day(6, 10).Sub(m.a.clock.Now()))
		po3 := core.Payout(200, "usd", day(7, 12), false)
		flow(m.a.clock.Now(), "cash_clearing", 0, 2_000_000)
		flow(m.a.clock.Now(), "cash_in_transit", 2_000_000, 0)
		m.a.clock.Advance(2 * time.Hour)
		m.notice(t, "evt_h3_failed", "payout.failed", core.SetPayoutStatus(po3["id"].(string), "failed"))
		require.False(t, runPass(t, fleet))
		flow(m.a.clock.Now(), "cash_in_transit", 0, 2_000_000)
		flow(m.a.clock.Now(), "cash_clearing", 2_000_000, 0)

		// Each day in New York, each payout account moved as the history
		// says, PSP accounts on the PSP and bank cash on its bank account.
		got := map[key][2]int64{}
		rows, err := w.pool.Query(t.Context(), "SELECT to_char((bucket_at AT TIME ZONE 'America/New_York')::date, 'YYYY-MM-DD'), account_type, sum(debits)::bigint, sum(credits)::bigint, bool_and(CASE WHEN account_type='bank_cash' THEN bank_account_id IS NOT NULL AND psp_id IS NULL"+
			" WHEN account_type='processor_fees' THEN psp_id IS NULL AND bank_account_id IS NULL ELSE psp_id IS NOT DISTINCT FROM $2 AND bank_account_id IS NULL END)"+
			" FROM "+booksTable(w, "ledger_daily")+" WHERE merchant_id=$1 AND book='general' AND currency='USD' AND bucket_at >= $3"+
			" AND account_type IN ('cash_clearing', 'cash_in_transit', 'processor_reserve', 'bank_cash', 'processor_fees') GROUP BY 1, 2", mid, psp, start)
		require.NoError(t, err)
		for rows.Next() {
			var k key
			var f [2]int64
			var scoped bool
			require.NoError(t, rows.Scan(&k.day, &k.account, &f[0], &f[1], &scoped))
			require.True(t, scoped, k)
			got[k] = f
		}
		require.NoError(t, rows.Err())
		require.Equal(t, want, got)

		// Each day's legs balance, and at the end the money is in the bank but
		// for the fee: nothing in transit, clearing or reserve.
		var unbalanced int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM (SELECT 1 FROM "+booksTable(w, "ledger_daily")+" WHERE merchant_id=$1 AND book='general'"+
			" GROUP BY currency, (bucket_at AT TIME ZONE 'America/New_York')::date HAVING sum(debits) <> sum(credits)) d", mid).Scan(&unbalanced))
		require.Zero(t, unbalanced)
		for account, balance := range map[string]int64{"cash_clearing": 0, "cash_in_transit": 0, "processor_reserve": 0, "bank_cash": 9_410_000} {
			f := feeTotal(t, w, mid, account, "USD")
			require.Equal(t, balance, f[1]-f[0], account)
		}
		audit := hostCLI(t, w, "ledger-audit", "--merchant", "id:"+mid.String())
		require.Contains(t, audit, "Ledger integrity OK")
		for _, account := range []string{"cash_clearing", "cash_in_transit", "processor_reserve", "bank_cash"} {
			require.NotContains(t, audit, "general/"+account, account)
		}

		// The merchant's archive carries its bank accounts and payout books to
		// an empty deployment, whose restore check and audit agree.
		target, client := archiveTarget(t, w)
		_, err = client.ImportBillingArchive(t.Context(), bytes.NewReader(archiveBytes(t, m.a.alpha)))
		require.NoError(t, err)
		require.Equal(t, [2]int64{0, 9_410_000}, feeTotal(t, target, mid, "bank_cash", "USD"))
		require.Equal(t, [2]int64{11_410_000, 11_410_000}, feeTotal(t, target, mid, "cash_in_transit", "USD"))
		var restored string
		require.NoError(t, target.pool.QueryRow(t.Context(), "SELECT last4 FROM "+booksTable(target, "bank_accounts")+" WHERE merchant_id=$1 AND ref=$2", mid, stripemock.BankAccount["id"]).Scan(&restored))
		require.Equal(t, "6789", restored)
		require.Contains(t, hostCLI(t, target, "ledger-audit", "--merchant", "id:"+mid.String()), "Ledger integrity OK")
	})
}

// payoutMembership is alice's 60-day membership, paid by a $10.00 Stripe
// charge that nothing renews while a scenario runs.
func payoutMembership(t *testing.T, configure ...func(*world)) *disputedMembership {
	t.Helper()
	a, p, sub := purchasedMembershipWith(t, 60*24, configure...)
	effects := p.effects(t)
	paid, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{SubscriptionID: sub.ID})
	require.NoError(t, err)
	require.Len(t, paid.Items, 1)
	return &disputedMembership{a: a, p: p, sub: sub, payment: paid.Items[0].ID, charge: effects[0].ID, intent: effects[0].Operation}
}

// alphaStripePSP is alpha's Stripe PSP.
func alphaStripePSP(t *testing.T, w *world, merchant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(w, "psps")+" WHERE merchant_id=$1 AND rail='stripe' AND account_id='acct_e2e'", merchant).Scan(&id))
	return id
}

// newYorkBooks keeps alpha's books in New York.
func newYorkBooks(w *world) {
	alpha := cfgMerchant(w.cfg, "alpha")
	alpha.Accounting = &billing.AccountingSettings{Timezone: "America/New_York"}
	setCfgMerchant(&w.cfg, "alpha", alpha)
}

// payoutReads counts the merchant's payout reads still queued.
func payoutReads(t *testing.T, w *world, merchant uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "stripe_payout_reads")+" WHERE merchant_id=$1", merchant).Scan(&n))
	return n
}

// payoutRead is the merchant's one queued payout read: its failures, and
// whether it waits for its finding.
func payoutRead(t *testing.T, w *world, merchant uuid.UUID) (int, bool) {
	t.Helper()
	var attempts int
	var parked bool
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT attempts, due_at = 'infinity' FROM "+booksTable(w, "stripe_payout_reads")+" WHERE merchant_id=$1", merchant).Scan(&attempts, &parked))
	return attempts, parked
}

// transferAt is when the general-ledger transfer at a coordinate took effect.
func transferAt(t *testing.T, w *world, merchant uuid.UUID, transferType, operation, sourceID string) time.Time {
	t.Helper()
	var at time.Time
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT effective_at FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND book='general' AND transfer_type=$2 AND operation=$3 AND source_id=$4",
		merchant, transferType, operation, sourceID).Scan(&at))
	return at
}
