//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/stripemock"
)

func TestRevenueRecognition(t *testing.T) {
	t.Run("OR-J41-S01/a-monthly-plan-bought-mid-month-books-cash-per-psp-and-recognises-at-the-boundary-and-the-end", func(t *testing.T) {
		start := time.Date(2026, time.June, 15, 12, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		price := calendarPrice(t, a, "monthly", 10_000_000, 1)
		order := buy(t, a, provider, "monthly-plan", []billing.OrderLineParams{{PriceID: price}}, 10_000_000)
		line, payment := order.Lines[0].ID.UUID(), order.Payment.PaymentID.UUID()

		var psp uuid.UUID
		var paidAt time.Time
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT psp_id, purchased_at FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), payment).Scan(&psp, &paidAt))
		paidAt = paidAt.UTC()
		sched := scheduleOfLine(t, a.world, line)
		require.Equal(t, revenueSchedule{Method: "ratable", Account: "subscription_revenue", Funded: 10_000_000, Starts: start, Ends: start.AddDate(0, 1, 0),
			Through: start, Next: ptr(time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC))}, sched.terms())
		require.Equal(t, []glRow{
			{Type: "cash_received", Operation: "cash", Source: "payment", SourceID: payment.String(), Debit: "cash_clearing", DebitPSP: &psp, Credit: "unapplied_cash", Amount: 10_000_000, At: paidAt},
			{Type: "cash_applied", Operation: "application", Source: "payment", SourceID: payment.String(), Debit: "unapplied_cash", Credit: "deferred_revenue", Amount: 10_000_000, At: minTime(start, paidAt), OrderLine: &line, Schedule: &sched.ID},
		}, glFor(t, a.world, "payment", payment.String()), "the money lands in its PSP's clearing account and waits in deferred revenue")

		// Midnight on 1 July ends June: 15.5 of the period's 30 days, dated
		// the last instant of June.
		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, time.Date(2026, time.July, 1, 1, 0, 0, 0, time.UTC))
		july := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
		require.Equal(t, []glRow{recognized(sched.ID, line, "recognition", july, july.Add(-time.Microsecond), 5_166_666)}, recognitions(t, a.world, sched.ID))
		var june int64
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(credits), 0) FROM "+booksTable(a.world, "ledger_daily")+" WHERE merchant_id=$1 AND book='general' AND account_type='subscription_revenue' AND bucket_at < $2", a.alpha.MerchantID().UUID(), july).Scan(&june))
		require.EqualValues(t, 5_166_666, june, "June's revenue is June's")

		// The period's end earns the rest.
		end := start.AddDate(0, 1, 0)
		passAt(t, a, fleet, end.Add(time.Hour))
		require.Equal(t, []glRow{
			recognized(sched.ID, line, "recognition", july, july.Add(-time.Microsecond), 5_166_666),
			recognized(sched.ID, line, "recognition", end, end.Add(-time.Microsecond), 4_833_334),
		}, recognitions(t, a.world, sched.ID))
		done := scheduleOfLine(t, a.world, line)
		require.EqualValues(t, 10_000_000, done.Recognized)
		require.Nil(t, done.Next)
	})

	t.Run("OR-J41-S02/an-annual-calendar-plan-recognises-a-twelfth-a-month-and-a-zone-boundary-follows-dst", func(t *testing.T) {
		// Bought at midnight on 1 January 2024 in UTC: each month a twelfth,
		// leap February included.
		start := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		price := calendarPrice(t, a, "annual", 120_000_000, 12)
		order := buy(t, a, provider, "annual-plan", []billing.OrderLineParams{{PriceID: price}}, 120_000_000)
		line := order.Lines[0].ID.UUID()
		sched := scheduleOfLine(t, a.world, line)
		require.NotNil(t, sched.Months)
		require.EqualValues(t, 12, *sched.Months)
		require.Equal(t, start.AddDate(1, 0, 0), sched.Ends)
		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, time.Date(2024, time.March, 1, 1, 0, 0, 0, time.UTC))
		feb, mar := time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, time.March, 1, 0, 0, 0, 0, time.UTC)
		require.Equal(t, []glRow{
			recognized(sched.ID, line, "recognition", feb, time.Date(2024, time.January, 31, 23, 59, 59, 999_999_000, time.UTC), 10_000_000),
			recognized(sched.ID, line, "recognition", mar, time.Date(2024, time.February, 29, 23, 59, 59, 999_999_000, time.UTC), 10_000_000),
		}, recognitions(t, a.world, sched.ID))
		after := scheduleOfLine(t, a.world, line)
		require.EqualValues(t, 20_000_000, after.Recognized)
		require.Equal(t, ptr(time.Date(2024, time.April, 1, 0, 0, 0, 0, time.UTC)), after.Next)

		// In New York the April boundary is midnight EDT, an hour before the
		// period bought at midnight EST ends.
		ny, err := time.LoadLocation("America/New_York")
		require.NoError(t, err)
		bought := time.Date(2026, time.March, 1, 0, 0, 0, 0, ny)
		n, nprovider := newPaymentWorld(t, atClock(bought), func(w *world) {
			alpha := cfgMerchant(w.cfg, "alpha")
			alpha.Accounting = &billing.AccountingSettings{Timezone: "America/New_York"}
			setCfgMerchant(&w.cfg, "alpha", alpha)
		})
		monthly := calendarPrice(t, n, "monthly", 10_000_000, 1)
		norder := buy(t, n, nprovider, "new-york-plan", []billing.OrderLineParams{{PriceID: monthly}}, 10_000_000)
		nline := norder.Lines[0].ID.UUID()
		nsched := scheduleOfLine(t, n.world, nline)
		april := time.Date(2026, time.April, 1, 0, 0, 0, 0, ny).UTC()
		require.Equal(t, "2026-04-01T04:00:00Z", april.Format(time.RFC3339))
		require.Equal(t, bought.UTC().AddDate(0, 1, 0), nsched.Ends, "a calendar period runs in UTC")
		require.Equal(t, &april, nsched.Next)
		nfleet := startFleet(t, n.world)
		passAt(t, n, nfleet, april.Add(30*time.Minute))
		passAt(t, n, nfleet, nsched.Ends.Add(30*time.Minute))
		require.Equal(t, []glRow{
			recognized(nsched.ID, nline, "recognition", april, april.Add(-time.Microsecond), 9_986_559),
			recognized(nsched.ID, nline, "recognition", nsched.Ends, nsched.Ends.Add(-time.Microsecond), 13_441),
		}, recognitions(t, n.world, nsched.ID), "743 of 744 hours fall in March")
		rows, err := n.pool.Query(t.Context(), "SELECT (bucket_at AT TIME ZONE 'America/New_York')::date::text, credits FROM "+booksTable(n.world, "ledger_daily")+
			" WHERE merchant_id=$1 AND book='general' AND account_type='subscription_revenue' AND bucket_at < $2 ORDER BY bucket_at", n.alpha.MerchantID().UUID(), nsched.Ends)
		require.NoError(t, err)
		type day struct {
			Date    string
			Credits int64
		}
		days, err := pgx.CollectRows(rows, pgx.RowToStructByPos[day])
		require.NoError(t, err)
		require.Equal(t, []day{{"2026-03-31", 9_986_559}, {"2026-04-01", 13_441}}, days)
	})

	t.Run("OR-J41-S03/lifetime-access-at-once-a-pass-ratable-and-seats-funded-per-seat", func(t *testing.T) {
		start := time.Date(2026, time.May, 10, 10, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		lifetime := oneOffPrice(t, a, "lifetime", 25_000_000, nil)
		pass := oneOffPrice(t, a, "pass", 9_000_000, ptr(720))
		team, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "team", DisplayName: "Team", Entitlements: []string{"team:member"}})
		require.NoError(t, err)
		seat, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: team.ID, Key: "team-month", UnitAmount: 4_000_000, Currency: "USD",
			BillingIntervalHours: ptr(720), AccessDurationHours: ptr(720), Quantity: &catalog.Quantity{Min: 1, Max: 5}})
		require.NoError(t, err)

		owned := buy(t, a, provider, "lifetime", []billing.OrderLineParams{{PriceID: lifetime}}, 25_000_000)
		bought := buy(t, a, provider, "pass", []billing.OrderLineParams{{PriceID: pass}}, 9_000_000)
		seats := buy(t, a, provider, "seats", []billing.OrderLineParams{{PriceID: seat.ID, Quantity: ptr(3)}}, 12_000_000)

		// Lifetime access is delivered at once.
		lline := owned.Lines[0].ID.UUID()
		ls := scheduleOfLine(t, a.world, lline)
		require.Equal(t, revenueSchedule{Method: "point_in_time", Account: "one_time_revenue", Funded: 25_000_000, Recognized: 25_000_000, Starts: ls.Starts, Ends: ls.Starts, Through: ls.Starts}, ls.terms())
		require.Equal(t, []glRow{recognized(ls.ID, lline, "recognition", ls.Starts, ls.Starts, 25_000_000)}, recognitions(t, a.world, ls.ID))
		require.Equal(t, "one_time_revenue", glAccount(t, a.world, ls.ID))

		// A pass is earned over its access window.
		pline := bought.Lines[0].ID.UUID()
		ps := scheduleOfLine(t, a.world, pline)
		require.NotNil(t, bought.Lines[0].ProductAccessID)
		require.Equal(t, bought.Lines[0].ProductAccessID.UUID(), *ps.ProductAccess)
		require.Equal(t, revenueSchedule{Method: "ratable", Account: "one_time_revenue", Funded: 9_000_000, Starts: start, Ends: start.Add(720 * time.Hour), Through: start,
			Next: ptr(time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC))}, ps.terms())

		// Seats are funded per seat.
		sline := seats.Lines[0].ID.UUID()
		ss := scheduleOfLine(t, a.world, sline)
		require.Equal(t, revenueSchedule{Method: "ratable", Account: "subscription_revenue", Funded: 12_000_000, Starts: start, Ends: start.Add(720 * time.Hour), Through: start,
			Next: ptr(time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC))}, ss.terms())

		// 518 of 720 hours fall in May.
		june := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, june.Add(time.Hour))
		require.Equal(t, []glRow{recognized(ps.ID, pline, "recognition", june, june.Add(-time.Microsecond), 6_475_000)}, recognitions(t, a.world, ps.ID))
		require.Equal(t, []glRow{recognized(ss.ID, sline, "recognition", june, june.Add(-time.Microsecond), 8_633_333)}, recognitions(t, a.world, ss.ID))
		require.Equal(t, []glRow{recognized(ls.ID, lline, "recognition", ls.Starts, ls.Starts, 25_000_000)}, recognitions(t, a.world, ls.ID), "lifetime access earns nothing more")
	})

	t.Run("OR-J41-S04/provider-bills-book-cash-to-their-psp-and-non-cash-to-revenue-adjustments", func(t *testing.T) {
		w := newWorld(t)
		mid := w.alpha.MerchantID().UUID()
		start := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
		end := start.Add(720 * time.Hour)
		now := start.Add(time.Hour)
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "plan", DisplayName: "Plan", Entitlements: []string{"plan:member"}})
		require.NoError(t, err)
		_, err = w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "plan-month", UnitAmount: 20_000_000, Currency: "USD", BillingIntervalHours: ptr(720), AccessDurationHours: ptr(720)})
		require.NoError(t, err)
		priceID := priceUUID(t, w, product.ID.UUID())
		plan := observed.Line{PriceID: &priceID, ProductID: ptr(product.ID.UUID()), Description: "Plan", Quantity: ptr(1), BillingIntervalHours: ptr(720)}
		type rebill struct {
			rail, policy, owner, currency string
			total, cash, applied, nonCash int64
			lines                         []observed.Line
			// recognised at the application, an hour into the period
			caughtUp int64
		}
		withAmount := func(l observed.Line, amount int64) observed.Line {
			l.UnitAmount, l.Amount = amount, amount
			return l
		}
		var stripeOrder uuid.UUID
		for _, r := range []rebill{
			// A Stripe invoice: 20.00 less a 2.00 provider discount plus 1.00
			// of tax, 3.00 of it paid from the customer's Stripe balance. The
			// tax is no revenue: its cash waits unapplied.
			{rail: "stripe", policy: "provider", owner: observed.OwnerProvider, currency: "USD", total: 19_000_000, cash: 16_000_000, applied: 15_000_000, nonCash: 3_000_000,
				lines: []observed.Line{withAmount(plan, 20_000_000), {Source: "provider_adjustment", Description: "Stripe invoice discount", Amount: -2_000_000},
					{Source: "provider_adjustment", Description: "Stripe invoice tax", UnitAmount: 1_000_000, Amount: 1_000_000}}, caughtUp: 25_000},
			{rail: "nmi", policy: "nmi_schedule", owner: observed.OwnerNMISchedule, currency: "USD", total: 10_000_000, cash: 10_000_000, applied: 10_000_000,
				lines: []observed.Line{withAmount(plan, 10_000_000)}, caughtUp: 13_888},
			{rail: "ccbill", policy: "provider", owner: observed.OwnerProvider, currency: "USD", total: 10_000_000, cash: 10_000_000, applied: 10_000_000,
				lines: []observed.Line{withAmount(plan, 10_000_000)}, caughtUp: 13_888},
			// A Solana pull in USDC, its scale six decimals.
			{rail: "solana", policy: "engine", owner: observed.OwnerEngine, currency: "USDC", total: 9_990_000, cash: 9_990_000, applied: 9_990_000,
				lines: []observed.Line{withAmount(plan, 9_990_000)}, caughtUp: 13_875},
		} {
			psp := railPSP(t, w, r.rail)
			// A customer holds one subscription to a product: one each.
			customer := uuid.New()
			sub := fixtureSubscription(t, w, customer, product.ID.UUID(), priceID, psp, r.rail, r.policy, start, end, nil)
			settlement := "cash"
			if r.nonCash > 0 {
				settlement = "mixed"
			}
			transaction := r.rail + "-rebill-" + start.Format("20060102")
			result := recordBill(t, w, observed.Bill{
				PSPID: psp, CustomerID: customer, SubscriptionID: &sub, Owner: r.owner, Kind: "renewal",
				SourceKind: r.rail + "_rebill", SourceKey: transaction, Currency: r.currency, Total: r.total,
				PeriodStart: start, PeriodEnd: end, DueAt: start, Lines: r.lines,
				Payment: &models.Payment{CustomerID: customer, SubscriptionID: &sub, Rail: models.Rail(r.rail), TransactionID: transaction,
					Amount: r.cash, ListAmount: r.cash, Currency: r.currency, Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
				Settled: true, SettlementSource: settlement, NonCashAmount: r.nonCash,
			}, now)
			require.True(t, result.Completed, r.rail)
			payment := result.Payment.ID
			if r.rail == "stripe" {
				stripeOrder = result.Order.ID
			}

			// One schedule: the adjustment line has none of its own.
			lines := orderSchedules(t, w, result.Order.ID)
			require.Len(t, lines, 1, r.rail)
			sched := lines[0]
			require.Equal(t, revenueSchedule{Method: "ratable", Account: "subscription_revenue", Funded: r.applied + r.nonCash, Recognized: r.caughtUp, Starts: start, Ends: end, Through: now,
				Next: ptr(time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC))}, sched.terms(), r.rail)
			require.Equal(t, r.currency, sched.Currency)
			want := []glRow{
				{Type: "cash_received", Operation: "cash", Source: "payment", SourceID: payment.String(), Debit: "cash_clearing", DebitPSP: &psp, Credit: "unapplied_cash", Amount: r.cash, At: now},
				{Type: "cash_applied", Operation: "application", Source: "payment", SourceID: payment.String(), Debit: "unapplied_cash", Credit: "deferred_revenue", Amount: r.applied, At: now, OrderLine: &sched.OrderLine, Schedule: &sched.ID},
			}
			require.Equal(t, want, glFor(t, w, "payment", payment.String()), r.rail)
			var nonCash []glRow
			if r.nonCash > 0 {
				nonCash = []glRow{{Type: "revenue_adjusted", Operation: "application", Source: "order", SourceID: result.Order.ID.String(), Debit: "revenue_adjustments", Credit: "deferred_revenue", Amount: r.nonCash, At: now, OrderLine: &sched.OrderLine, Schedule: &sched.ID}}
			}
			require.Equal(t, nonCash, glFor(t, w, "order", result.Order.ID.String()), r.rail)
			require.Equal(t, []glRow{recognized(sched.ID, sched.OrderLine, "recognition", now, now, r.caughtUp)}, recognitions(t, w, sched.ID), "an hour of %s's period is earned at payment", r.rail)
			var currency string
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT currency FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND book='general' AND operation='cash' AND source='payment' AND source_id=$2", mid, payment.String()).Scan(&currency))
			require.Equal(t, r.currency, currency, "each PSP clears in its own currency")
		}
		require.EqualValues(t, 1_000_000, unappliedCash(t, w, "USD"), "the tax waits")
		findings, err := w.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.adjustment.unapplied"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Equal(t, "1000000", findings.Items[0].Evidence["amount"])
		require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")

		// Settled by a person, the finding stays settled when the order is
		// replayed.
		_, err = w.pool.Exec(t.Context(), "UPDATE "+booksTable(w, "reconciliation_findings")+" SET status='fixed', resolution='auto_vanished', resolved_at=now() WHERE merchant_id=$1 AND finding_type='books.adjustment.unapplied'", mid)
		require.NoError(t, err)
		database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.Replay(ctx, database.NewWithPgxTx(tx), stripeOrder)
		}))
		findings, err = w.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.adjustment.unapplied", Status: "fixed"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1, "a settled finding stays settled")
		open, err := w.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.adjustment.unapplied"})
		require.NoError(t, err)
		require.Empty(t, open.Items)
	})

	t.Run("OR-J41-S05/a-seat-increase-defers-its-charge-over-the-rest-of-the-period", func(t *testing.T) {
		start := time.Date(2026, time.August, 12, 6, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		team, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "team", DisplayName: "Team", Entitlements: []string{"team:member"}})
		require.NoError(t, err)
		seat, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: team.ID, Key: "team-day", UnitAmount: 10_000_000, Currency: "USD",
			BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24), Quantity: &catalog.Quantity{Min: 1, Max: 5}})
		require.NoError(t, err)
		order := buy(t, a, provider, "team", []billing.OrderLineParams{{PriceID: seat.ID, Quantity: ptr(1)}}, 10_000_000)
		sub := *order.Lines[0].SubscriptionID
		first := scheduleOfLine(t, a.world, order.Lines[0].ID.UUID())

		// Two more seats half way through the day: 2 × 10.00 × 12/24.
		accepted := start.Add(12 * time.Hour)
		a.clock.Advance(accepted.Sub(a.clock.Now()))
		status, raw := a.request(t, http.MethodPost, "/v1/me/subscriptions/"+sub.String()+"/change", a.aliceToken, map[string]any{"quantity": 3}, map[string]string{"Idempotency-Key": "more-seats"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var added billing.SubscriptionChange
		require.NoError(t, json.Unmarshal(raw, &added))
		require.Equal(t, "succeeded", added.Status, string(raw))
		require.EqualValues(t, 10_000_000, added.AmountDueNow)

		var adjustment revenueScheduleRow
		for _, s := range subscriptionSchedules(t, a.world, sub.UUID()) {
			if s.ID != first.ID {
				adjustment = s
			}
		}
		require.Equal(t, revenueSchedule{Method: "ratable", Account: "subscription_revenue", Funded: 10_000_000, Starts: accepted, Ends: start.Add(24 * time.Hour), Through: accepted,
			Next: ptr(start.Add(24 * time.Hour))}, adjustment.terms(), "the added seats' charge is earned over what is left of the period")
		require.Equal(t, int64(10_000_000), first.Funded, "the first schedule keeps running unchanged")

		fleet := startFleet(t, a.world)
		end := start.Add(24 * time.Hour)
		passAt(t, a, fleet, end.Add(30*time.Minute))
		require.Equal(t, []glRow{recognized(adjustment.ID, adjustment.OrderLine, "recognition", end, end.Add(-time.Microsecond), 10_000_000)}, recognitions(t, a.world, adjustment.ID))
		require.Equal(t, []glRow{recognized(first.ID, first.OrderLine, "recognition", end, end.Add(-time.Microsecond), 10_000_000)}, recognitions(t, a.world, first.ID))
	})

	t.Run("OR-J41-S06/an-idle-pass-locks-no-schedule-and-a-replayed-boundary-posts-once", func(t *testing.T) {
		start := time.Date(2026, time.June, 15, 12, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		price := calendarPrice(t, a, "monthly", 10_000_000, 1)
		order := buy(t, a, provider, "monthly-plan", []billing.OrderLineParams{{PriceID: price}}, 10_000_000)
		line := order.Lines[0].ID.UUID()
		sched := scheduleOfLine(t, a.world, line)
		fleet := startFleet(t, a.world)

		// Nothing is due before 1 July: the pass reads the due index, finds
		// nothing and takes no schedule's row. Other jobs may post (the
		// sale's processor fee): only the schedule's legs are counted.
		before := scheduleVersion(t, a.world, sched.ID)
		count := scheduleLegCount(t, a.world)
		passAt(t, a, fleet, time.Date(2026, time.June, 30, 23, 0, 0, 0, time.UTC))
		require.Equal(t, before, scheduleVersion(t, a.world, sched.ID), "an idle pass locks no schedule")
		require.Equal(t, count, scheduleLegCount(t, a.world))

		// The boundary posts once, whichever pass reaches it and however often.
		july := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
		a.clock.Advance(july.Add(time.Hour).Sub(a.clock.Now()))
		runRegistered(t, fleet, "openrails.billing_pass", 2)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		want := []glRow{recognized(sched.ID, line, "recognition", july, july.Add(-time.Microsecond), 5_166_666)}
		require.Equal(t, want, recognitions(t, a.world, sched.ID))

		// The rebuild's replay of the order posts nothing again.
		count = scheduleLegCount(t, a.world)
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.Replay(ctx, database.NewWithPgxTx(tx), order.ID.UUID())
		}))
		require.Equal(t, count, scheduleLegCount(t, a.world))
		require.Equal(t, want, recognitions(t, a.world, sched.ID))
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S07/an-external-payment-books-to-external-receipts-and-a-payment-beyond-the-bill-stays-unapplied", func(t *testing.T) {
		a, p := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		order := unpaidSale(t, a, p, customer, "external-order")
		payment, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "bank-receipt-0001"})
		require.NoError(t, err)
		line := order.Lines[0].ID.UUID()
		sched := scheduleOfLine(t, a.world, line)
		require.Equal(t, "point_in_time", sched.Method)
		var paidAt time.Time
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT purchased_at FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), payment.ID.UUID()).Scan(&paidAt))
		paidAt = paidAt.UTC()
		got := glFor(t, a.world, "payment", payment.ID.UUID().String())
		require.Len(t, got, 2)
		require.Equal(t, glRow{Type: "cash_received", Operation: "cash", Source: "payment", SourceID: payment.ID.UUID().String(), Debit: "external_receipts", Credit: "unapplied_cash", Amount: 30_000_000, At: paidAt}, got[0],
			"money received outside OpenRails is no PSP's")
		require.Equal(t, "cash_applied", got[1].Type)
		require.EqualValues(t, 30_000_000, got[1].Amount)
		require.Zero(t, unappliedCash(t, a.world, "USD"))

		// A provider bill paid twice: the second capture is beyond it.
		var psp uuid.UUID
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(a.world, "psps")+" WHERE merchant_id=$1 AND rail='stripe'", a.alpha.MerchantID().UUID()).Scan(&psp))
		sale := order.Lines[0]
		salePrice := priceUUID(t, a.world, sale.ProductID.UUID())
		now := a.clock.Now().UTC().Truncate(time.Microsecond)
		bill := func(transaction string) observed.Bill {
			return observed.Bill{PSPID: psp, CustomerID: customer.UUID(), Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "provider_purchase", SourceKey: "twice-paid",
				Currency: "USD", Total: 10_000_000, Lines: []observed.Line{{PriceID: &salePrice, ProductID: ptr(sale.ProductID.UUID()), Description: "Sale", Quantity: ptr(1), UnitAmount: 10_000_000, Amount: 10_000_000}},
				Payment: &models.Payment{CustomerID: customer.UUID(), Rail: models.RailStripe, TransactionID: transaction, Amount: 10_000_000, ListAmount: 10_000_000, Currency: "USD",
					Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
				Settled: true, SettlementSource: "cash"}
		}
		paid := recordBill(t, a.world, bill("ch_twice_first"), now)
		require.True(t, paid.Completed)
		again := recordBill(t, a.world, bill("ch_twice_second"), now)
		require.True(t, again.PaymentCreated)
		require.False(t, again.Completed)
		require.Len(t, glFor(t, a.world, "payment", paid.Payment.ID.String()), 2, "the first capture is applied")
		require.Equal(t, []glRow{{Type: "cash_received", Operation: "cash", Source: "payment", SourceID: again.Payment.ID.String(), Debit: "cash_clearing", DebitPSP: &psp, Credit: "unapplied_cash", Amount: 10_000_000, At: now}},
			glFor(t, a.world, "payment", again.Payment.ID.String()), "the second stays unapplied until it is refunded")
		require.EqualValues(t, 10_000_000, unappliedCash(t, a.world, "USD"))
		audit := "id:" + a.alpha.MerchantID().String()
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", audit), "Ledger integrity OK")

		// A payment's applications never exceed what it brought.
		first := customer.UUID()
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := books.New(database.NewWithPgxTx(tx).Gen(ctx), a.alpha.MerchantID().UUID()).Post(ctx, books.Posting{
				Debit: books.Ref{Type: books.UnappliedCash}, Credit: books.Ref{Type: books.DeferredRevenue}, Amount: 5_000_000, Currency: "USD", Type: books.CashApplied,
				Coord: books.Coord{Operation: books.OpApplication, Source: "payment", SourceID: paid.Payment.ID.String()}, EffectiveAt: now, Customer: &first,
			})
			return err
		}))
		output, err := hostCLIResult(t, a.world, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "overapplied payment: "+paid.Payment.ID.String())
	})

	t.Run("OR-J41-S08/an-immediate-cancellation-without-a-refund-forfeits-the-remainder", func(t *testing.T) {
		start := time.Date(2026, time.September, 10, 8, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		sub := buyMembership(t, a, a.alpha, provider, 24)
		line := subscriptionSchedules(t, a.world, sub.ID.UUID())
		require.Len(t, line, 1)
		sched := line[0]

		canceled := start.Add(6 * time.Hour)
		a.clock.Advance(canceled.Sub(a.clock.Now()))
		_, err := a.alpha.CancelSubscription(t.Context(), sub.ID, billing.CancelSubscriptionParams{Reason: "Ended by the merchant", RevokeAccess: true})
		require.NoError(t, err)
		ended := scheduleOfLine(t, a.world, sched.OrderLine)
		require.Equal(t, &canceled, ended.EndedAt)
		require.Equal(t, &canceled, ended.Next, "an early end makes the schedule due")
		require.Empty(t, recognitions(t, a.world, sched.ID), "nothing is recognised inline")

		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, canceled.Add(time.Hour))
		require.Equal(t, []glRow{
			recognized(sched.ID, sched.OrderLine, "recognition", canceled, canceled.Add(-time.Microsecond), 2_500_000),
			recognized(sched.ID, sched.OrderLine, "termination", canceled, canceled.Add(-time.Microsecond), 7_500_000),
		}, recognitions(t, a.world, sched.ID), "six hours earned, the rest forfeited")
		done := scheduleOfLine(t, a.world, sched.OrderLine)
		require.EqualValues(t, 10_000_000, done.Recognized)
		require.Nil(t, done.Next)
	})

	t.Run("OR-J41-S09/a-usage-orders-payment-books-its-cash-and-leaves-it-for-the-receivable", func(t *testing.T) {
		for _, collection := range []string{"card", "remittance"} {
			t.Run(collection, func(t *testing.T) {
				a, _, customer := usageBillingWorld(t, true, collection == "remittance")
				oneUsage(t, a.alpha, usageInput(customer, "billed-work", 3))
				// The pass posts usage taken in before its instant.
				a.clock.Advance(time.Second)
				fleet := startFleet(t, a.world)
				if collection == "remittance" {
					postUsageMonth(t, a.world, fleet)
				}
				runUsageDue(t, fleet)
				order := oneUsageOrder(t, a, customer)
				if collection == "remittance" {
					require.Equal(t, billing.OrderOpen, order.Status)
					_, err := a.alpha.RecordExternalPayment(t.Context(), billing.RecordExternalPaymentParams{OrderID: order.ID, Amount: 30_000_000, TransactionID: "bank-usage-receipt"})
					require.NoError(t, err)
				} else {
					require.Eventually(t, func() bool { return oneUsageOrder(t, a, customer).Status == billing.OrderComplete }, 15*time.Second, 20*time.Millisecond)
				}
				var payment uuid.UUID
				var psp *uuid.UUID
				var paidAt time.Time
				require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id, psp_id, purchased_at FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND order_id=$2",
					a.alpha.MerchantID().UUID(), order.ID.UUID()).Scan(&payment, &psp, &paidAt))
				debit := "cash_clearing"
				if collection == "remittance" {
					debit = "external_receipts"
				}
				require.Equal(t, []glRow{{Type: "cash_received", Operation: "cash", Source: "payment", SourceID: payment.String(), Debit: debit, DebitPSP: psp, Credit: "unapplied_cash", Amount: 30_000_000, At: paidAt.UTC()}},
					glFor(t, a.world, "payment", payment.String()), "the money is booked once and applied to no line")
				require.Empty(t, orderSchedules(t, a.world, order.ID.UUID()))
				require.EqualValues(t, 30_000_000, unappliedCash(t, a.world, "USD"), "it waits for the usage receivable")
				require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
			})
		}
	})

	t.Run("OR-J41-S10/revoked-access-ends-its-schedule-and-an-undone-revocation-takes-the-forfeit-back", func(t *testing.T) {
		start := time.Date(2026, time.May, 10, 10, 0, 0, 0, time.UTC)
		a, provider := newPaymentWorld(t, atClock(start))
		pass := oneOffPrice(t, a, "pass", 9_000_000, ptr(720))
		order := buy(t, a, provider, "pass", []billing.OrderLineParams{{PriceID: pass}}, 9_000_000)
		line, access := order.Lines[0].ID.UUID(), *order.Lines[0].ProductAccessID
		sched := scheduleOfLine(t, a.world, line)
		june := time.Date(2026, time.June, 1, 0, 0, 0, 0, time.UTC)
		require.Equal(t, &june, sched.Next)
		accessRow := func(set string, args ...any) {
			t.Helper()
			_, err := a.pool.Exec(t.Context(), "UPDATE "+booksTable(a.world, "product_access")+" SET "+set+" WHERE merchant_id=$1 AND id=$2",
				append([]any{a.alpha.MerchantID().UUID(), access.UUID()}, args...)...)
			require.NoError(t, err)
		}

		// Access cut short to 5 June and restored, as a destructive run and its
		// undo would: the schedule ends there, then runs again, still due on
		// 1 June.
		accessRow("ends_at=$3", time.Date(2026, time.June, 5, 10, 0, 0, 0, time.UTC))
		require.Equal(t, &june, scheduleOfLine(t, a.world, line).Next)
		accessRow("ends_at=$3", start.Add(720*time.Hour))
		restored := scheduleOfLine(t, a.world, line)
		require.Nil(t, restored.EndedAt)
		require.Equal(t, &june, restored.Next, "an end undone never postpones a boundary due before it")

		// Revoked five days in: 120 of 720 hours earned, the rest forfeited.
		revoked := start.Add(120 * time.Hour)
		a.clock.Advance(revoked.Sub(a.clock.Now()))
		require.NoError(t, a.alpha.RevokeProductAccess(t.Context(), access, billing.RevokeProductAccessParams{Reason: "Ended by the merchant"}))
		require.Equal(t, &revoked, scheduleOfLine(t, a.world, line).EndedAt)
		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, revoked.Add(time.Hour))
		earned := recognized(sched.ID, line, "recognition", revoked, revoked.Add(-time.Microsecond), 1_500_000)
		forfeited := recognized(sched.ID, line, "termination", revoked, revoked.Add(-time.Microsecond), 7_500_000)
		require.Equal(t, []glRow{earned, forfeited}, recognitions(t, a.world, sched.ID))

		// The revocation undone: the forfeit is taken back. Then a flapping
		// provider ends it again at the same instant and undoes that too: each
		// end and reinstatement posts in its own generation.
		undone := revoked.Add(24 * time.Hour)
		a.clock.Advance(undone.Sub(a.clock.Now()))
		accessRow("revoked_at=NULL, revoke_reason=NULL")
		back := scheduleOfLine(t, a.world, line)
		require.Nil(t, back.EndedAt)
		require.Equal(t, &revoked, back.Next)
		passAt(t, a, fleet, undone.Add(time.Hour))
		accessRow("revoked_at=$3, revoke_reason='admin'", revoked)
		passAt(t, a, fleet, undone.Add(2*time.Hour))
		accessRow("revoked_at=NULL, revoke_reason=NULL")
		passAt(t, a, fleet, undone.Add(3*time.Hour))
		passAt(t, a, fleet, june.Add(time.Hour))
		reinstated := func(gen int, at time.Time) glRow {
			return glRow{Type: "revenue_recognized", Operation: "reinstatement", Source: "revenue_schedule", SourceID: sched.ID.String() + "@" + revoked.Format(time.RFC3339Nano) + "#" + strconv.Itoa(gen),
				Debit: "one_time_revenue", Amount: 7_500_000, At: at, OrderLine: &line, Schedule: &sched.ID}
		}
		require.Equal(t, []glRow{earned, forfeited, reinstated(1, undone.Add(time.Hour)), inGeneration(forfeited, 1), reinstated(2, undone.Add(3*time.Hour)),
			recognized(sched.ID, line, "recognition", june, june.Add(-time.Microsecond), 4_975_000)}, recognitions(t, a.world, sched.ID))
		done := scheduleOfLine(t, a.world, line)
		require.EqualValues(t, 6_475_000, done.Recognized)
		require.EqualValues(t, 2, done.Reinstatements)
	})

	t.Run("OR-J41-S11/a-late-payment-on-an-order-another-holds-stays-unapplied", func(t *testing.T) {
		a, provider := newPaymentWorld(t, func(w *world) {
			w.clock.Advance(-30 * 24 * time.Hour)
			w.cfg.ProviderSandbox = &openrails.ProviderSandboxConfig{CCBillWebhookSources: []string{"127.0.0.1/32"}}
			m := cfgMerchant(w.cfg, "alpha")
			m.PSPs["ccbill"] = openrails.CCBillPSP{ClientAccnum: "999999", ClientSubacc: "0000", Environment: billing.EnvironmentSandbox, Salt: "critical-ccbill-salt"}.PSPConfig()
			setCfgMerchant(&w.cfg, "alpha", m)
		})
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "member", DisplayName: "Member", Entitlements: []string{"member:paid"}})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "member-monthly", UnitAmount: 10_000_000, Currency: "USD",
			BillingIntervalHours: ptr(720), AccessDurationHours: ptr(720)})
		require.NoError(t, err)
		_, err = a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "member-ccbill", PSP: "ccbill", FlexID: "critical-flex",
			Currency: "USD", UnitAmount: ptr(int64(10_000_000)), BillingIntervalHours: ptr(720)})
		require.NoError(t, err)
		lines := []billing.OrderLineParams{{PriceID: price.ID}}
		status, raw := a.request(t, http.MethodPost, "/v1/me/orders/preview", a.aliceToken, billing.PreviewMyOrderParams{Lines: lines})
		require.Equal(t, http.StatusOK, status, string(raw))
		var preview billing.OrderPreview
		require.NoError(t, json.Unmarshal(raw, &preview))
		var page billing.PSPID
		for _, option := range preview.PaymentOptions {
			if option.Rail == "ccbill" {
				page = option.PSPID
			}
		}
		require.False(t, page.IsZero(), string(raw))

		// Alice opens the CCBill page and leaves; the order expires and she
		// buys the membership by card in another.
		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, billing.CreateMyOrderParams{Lines: lines, ExpectedTotal: ptr(int64(10_000_000)),
			Payment: &billing.OrderPaymentParams{PSPID: &page, ReturnURL: "https://critical.invalid/paid"}}, map[string]string{"Idempotency-Key": "ccbill-page"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var abandoned billing.Order
		require.NoError(t, json.Unmarshal(raw, &abandoned))
		link, err := url.Parse(*abandoned.Payment.NextAction.URL)
		require.NoError(t, err)
		a.clock.Advance(25 * time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.billing_pass", 1)
		expired, err := a.alpha.GetOrder(t.Context(), abandoned.ID)
		require.NoError(t, err)
		require.Equal(t, billing.OrderExpired, expired.Status)
		card := buy(t, a, provider, "card-instead", lines, 10_000_000)

		// The page is completed after all: the money lands on the expired
		// order, whose membership the card's order holds. It is kept for
		// refund and applied to nothing.
		sale := map[string]any{"subscriptionId": "0778899001122334466", "transactionId": "0812233445566778831", "reservationId": link.Query().Get("reservationId"),
			"flexId": "critical-flex", "username": a.alice.Username, "billedInitialPrice": "10.00", "billedRecurringPrice": "10.00", "billedCurrencyCode": "840",
			"nextRenewalDate": a.clock.Now().UTC().Add(720 * time.Hour).Format(time.DateOnly), "cardType": "VISA", "last4": "1111"}
		status, body := ccbillNotice(t, a, "NewSaleSuccess", sale)
		require.Equal(t, http.StatusOK, status, body)
		var late uuid.UUID
		var ccbill uuid.UUID
		var paidAt time.Time
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id, psp_id, purchased_at FROM "+booksTable(a.world, "payments")+" WHERE merchant_id=$1 AND transaction_id='0812233445566778831'",
			a.alpha.MerchantID().UUID()).Scan(&late, &ccbill, &paidAt))
		require.Equal(t, []glRow{{Type: "cash_received", Operation: "cash", Source: "payment", SourceID: late.String(), Debit: "cash_clearing", DebitPSP: &ccbill, Credit: "unapplied_cash", Amount: 10_000_000, At: paidAt.UTC()}},
			glFor(t, a.world, "payment", late.String()))
		require.Empty(t, orderSchedules(t, a.world, abandoned.ID.UUID()))
		require.Len(t, orderSchedules(t, a.world, card.ID.UUID()), 1)
		require.EqualValues(t, 10_000_000, unappliedCash(t, a.world, "USD"), "the late payment waits for its refund")
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "life.order.late_payment"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1)
		require.Equal(t, late.String(), findings.Items[0].SubjectKey)
	})

	t.Run("OR-J41-S12/a-purchase-is-applied-over-the-access-its-grant-gives-and-a-replay-applies-an-order-with-no-schedule", func(t *testing.T) {
		start := time.Date(2026, time.July, 6, 9, 0, 0, 0, time.UTC)
		a, _ := newPaymentWorld(t, atClock(start))
		oneOffPrice(t, a, "pass", 9_000_000, ptr(720))
		products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{"pass"}})
		require.NoError(t, err)
		require.Len(t, products.Items, 1)
		product := products.Items[0].ID.UUID()
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		var psp uuid.UUID
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(a.world, "psps")+" WHERE merchant_id=$1 AND rail='stripe'", a.alpha.MerchantID().UUID()).Scan(&psp))
		now := a.clock.Now().UTC().Truncate(time.Microsecond)
		priceID := priceUUID(t, a.world, product)

		// The provider's sale, and its tax, is recorded before its access is
		// granted: its timed line waits, its money unapplied. The tax is no
		// revenue: the application that leaves it unapplied says so.
		paid := recordBill(t, a.world, observed.Bill{PSPID: psp, CustomerID: customer.UUID(), Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "provider_purchase", SourceKey: "pass-sale",
			Currency: "USD", Total: 10_000_000, Lines: []observed.Line{{PriceID: &priceID, ProductID: &product, Description: "Pass", Quantity: ptr(1), UnitAmount: 9_000_000, Amount: 9_000_000, AccessDurationHours: ptr(720)},
				{Source: "provider_adjustment", Description: "Stripe invoice tax", UnitAmount: 1_000_000, Amount: 1_000_000}},
			Payment: &models.Payment{CustomerID: customer.UUID(), Rail: models.RailStripe, TransactionID: "ch_pass_sale", Amount: 10_000_000, ListAmount: 10_000_000, Currency: "USD",
				Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
			Settled: true, SettlementSource: "cash"}, now)
		require.True(t, paid.Completed)
		require.Empty(t, orderSchedules(t, a.world, paid.Order.ID), "no access yet, no service to recognise over")
		require.EqualValues(t, 10_000_000, unappliedCash(t, a.world, "USD"))
		tax, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.adjustment.unapplied"})
		require.NoError(t, err)
		require.Len(t, tax.Items, 1)
		require.Equal(t, "order:"+paid.Order.ID.String(), tax.Items[0].SubjectKey)
		unapplied := func(status billing.FindingStatus) []billing.Finding {
			t.Helper()
			found, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.line.unapplied", Status: status})
			require.NoError(t, err)
			return found.Items
		}
		require.Empty(t, unapplied(""), "the grant is its caller's next step")

		// Applied for good with no access granted: a finding, and the audit
		// names the line.
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.ApplyOrder(ctx, database.NewWithPgxTx(tx), paid.Order.ID, nil)
		}))
		pending := unapplied("")
		require.Len(t, pending, 1)
		lines, err := a.alpha.GetOrder(t.Context(), billing.OrderID(paid.Order.ID))
		require.NoError(t, err)
		require.Equal(t, "order_line:"+lines.Lines[0].ID.UUID().String(), pending[0].SubjectKey)
		audit := "id:" + a.alpha.MerchantID().String()
		output, err := hostCLIResult(t, a.world, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "unscheduled line: order="+paid.Order.ID.String())

		// Its grant stacks after ten days the customer already holds.
		from, until := now.Add(240*time.Hour), now.Add(960*time.Hour)
		paymentID := paid.Payment.ID
		granted, err := entitlements.NewEntitlementService(database, a.clock).PushAccess(ctx, entitlements.PushAccessParams{UserID: customer.UUID().String(), CustomerID: customer.UUID(), ProductID: product,
			NotBefore: &from, EndsAt: &until, SourceType: models.AccessSourcePurchase, SourceID: paymentID.String(), PaymentID: &paymentID})
		require.NoError(t, err)
		require.NotNil(t, granted)

		// The rebuild's replay applies the order now: over the window granted.
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.Replay(ctx, database.NewWithPgxTx(tx), paid.Order.ID)
		}))
		schedules := orderSchedules(t, a.world, paid.Order.ID)
		require.Len(t, schedules, 1)
		sched := schedules[0]
		require.Equal(t, granted.ID, *sched.ProductAccess)
		require.Equal(t, revenueSchedule{Method: "ratable", Account: "one_time_revenue", Funded: 9_000_000, Starts: from, Ends: until, Through: from, Next: ptr(time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC))}, sched.terms())
		require.EqualValues(t, 1_000_000, unappliedCash(t, a.world, "USD"), "the tax waits")
		require.Empty(t, unapplied(""), "the line is scheduled at last")
		require.Len(t, unapplied("fixed"), 1)
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", audit), "Ledger integrity OK")

		// Revoked before it began: forfeited whole at its start.
		require.NoError(t, a.alpha.RevokeProductAccess(t.Context(), billing.ProductAccessID(granted.ID), billing.RevokeProductAccessParams{Reason: "Ended by the merchant"}))
		require.Equal(t, &from, scheduleOfLine(t, a.world, sched.OrderLine).EndedAt)
		fleet := startFleet(t, a.world)
		passAt(t, a, fleet, from.Add(time.Hour))
		require.Equal(t, []glRow{recognized(sched.ID, sched.OrderLine, "termination", from, from.Add(-time.Microsecond), 9_000_000)}, recognitions(t, a.world, sched.ID))
	})

	t.Run("OR-J41-S13/a-prune-that-hides-booked-money-or-a-recognising-subscription-raises-a-finding-its-undo-resolves", func(t *testing.T) {
		w := newWorld(t)
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sale", DisplayName: "Sale", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		_, err = w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "sale", UnitAmount: 10_000_000, Currency: "USD"})
		require.NoError(t, err)
		plan, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "plan", DisplayName: "Plan", Entitlements: []string{"plan:member"}})
		require.NoError(t, err)
		_, err = w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: plan.ID, Key: "plan-month", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(720), AccessDurationHours: ptr(720)})
		require.NoError(t, err)
		psp := railPSP(t, w, "stripe")
		customer := uuid.New()
		now := w.clock.Now().UTC().Truncate(time.Microsecond)
		priceID, productID := priceUUID(t, w, product.ID.UUID()), product.ID.UUID()
		sold := recordBill(t, w, observed.Bill{PSPID: psp, CustomerID: customer, Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "provider_purchase", SourceKey: "pruned-sale",
			Currency: "USD", Total: 10_000_000, Lines: []observed.Line{{PriceID: &priceID, ProductID: &productID, Description: "Sale", Quantity: ptr(1), UnitAmount: 10_000_000, Amount: 10_000_000}},
			Payment: &models.Payment{CustomerID: customer, Rail: models.RailStripe, TransactionID: "ch_pruned", Amount: 10_000_000, ListAmount: 10_000_000, Currency: "USD",
				Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
			Settled: true, SettlementSource: "cash"}, now)
		require.Len(t, glFor(t, w, "payment", sold.Payment.ID.String()), 2)
		planPrice, planID := priceUUID(t, w, plan.ID.UUID()), plan.ID.UUID()
		start := now.Add(-time.Hour)
		sub := fixtureSubscription(t, w, customer, planID, planPrice, psp, "stripe", "provider", start, start.Add(720*time.Hour), nil)
		renewed := recordBill(t, w, observed.Bill{PSPID: psp, CustomerID: customer, SubscriptionID: &sub, Owner: observed.OwnerProvider, Kind: "renewal",
			SourceKind: "stripe_invoice", SourceKey: "in_pruned", Currency: "USD", Total: 10_000_000, PeriodStart: start, PeriodEnd: start.Add(720 * time.Hour), DueAt: start,
			Lines: []observed.Line{{PriceID: &planPrice, ProductID: &planID, Description: "Plan", Quantity: ptr(1), UnitAmount: 10_000_000, Amount: 10_000_000, BillingIntervalHours: ptr(720)}},
			Payment: &models.Payment{CustomerID: customer, SubscriptionID: &sub, Rail: models.RailStripe, TransactionID: "ch_pruned_renewal", Amount: 10_000_000, ListAmount: 10_000_000,
				Currency: "USD", Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
			Settled: true, SettlementSource: "cash"}, now)
		require.Len(t, orderSchedules(t, w, renewed.Order.ID), 1)

		// The provider's complete record lacks both charges and the
		// subscription.
		since, until := now.Add(-time.Hour), now.Add(time.Hour)
		database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
		var pruned reconcile.PruneResult
		require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			var err error
			pruned, err = reconcile.PrunePSPExcess(ctx, database, windowSnapshot{since: since, until: until}, reconcile.ProviderStripe,
				reconcile.PSPBinding{ID: psp, Rail: "stripe", AccountID: "books-stripe"}, reconcile.PruneParams{Since: since, Until: until, Apply: true, ExpectedRows: ptr(3), Actor: "critical"})
			return err
		}))
		require.ElementsMatch(t, []uuid.UUID{sold.Payment.ID, renewed.Payment.ID}, pruned.PaymentIDs)
		require.Equal(t, []uuid.UUID{sub}, pruned.SubscriptionIDs)
		findings := func(typ string, status billing.FindingStatus) []billing.Finding {
			t.Helper()
			found, err := w.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: typ, Status: status})
			require.NoError(t, err)
			return found.Items
		}
		payments := findings("books.payment.pruned", "")
		require.Len(t, payments, 2)
		subscriptions := findings("books.subscription.pruned", "")
		require.Len(t, subscriptions, 1)
		require.Equal(t, "subscription:"+sub.String(), subscriptions[0].SubjectKey)
		require.Len(t, glFor(t, w, "payment", sold.Payment.ID.String()), 2, "the books keep what they booked until a person decides")

		// The prune undone: its findings close.
		require.NoError(t, database.RunInMerchantConn(ctx, func(ctx context.Context) error {
			_, err := reconcile.RollbackDestructiveRun(ctx, database, pruned.RunID, "critical")
			return err
		}))
		require.Empty(t, append(findings("books.payment.pruned", ""), findings("books.subscription.pruned", "")...), "none stays open")
		require.Len(t, findings("books.payment.pruned", "fixed"), 2)
		require.Len(t, findings("books.subscription.pruned", "fixed"), 1)
	})

	t.Run("OR-J41-S14/an-end-before-the-schedule-exists-reaches-it", func(t *testing.T) {
		w := newWorld(t)
		start := time.Date(2026, time.August, 3, 0, 0, 0, 0, time.UTC)
		end, now := start.Add(720*time.Hour), start.Add(time.Hour)
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "plan", DisplayName: "Plan", Entitlements: []string{"plan:member"}})
		require.NoError(t, err)
		_, err = w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "plan-month", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(720), AccessDurationHours: ptr(720)})
		require.NoError(t, err)
		priceID, productID := priceUUID(t, w, product.ID.UUID()), product.ID.UUID()
		customer := uuid.New()
		psp := railPSP(t, w, "ccbill")

		// Imported canceled ten days before: the provider bills the next
		// period anyway. The charge serves nothing and is forfeited at payment.
		canceled := start.Add(-240 * time.Hour)
		sub := fixtureSubscription(t, w, customer, productID, priceID, psp, "ccbill", "provider", start.Add(-720*time.Hour), start, &canceled)
		bill := recordBill(t, w, observed.Bill{PSPID: psp, CustomerID: customer, SubscriptionID: &sub, Owner: observed.OwnerProvider, Kind: "renewal",
			SourceKind: "ccbill_rebill", SourceKey: "after-cancel", Currency: "USD", Total: 10_000_000, PeriodStart: start, PeriodEnd: end, DueAt: start,
			Lines: []observed.Line{{PriceID: &priceID, ProductID: &productID, Description: "Plan", Quantity: ptr(1), UnitAmount: 10_000_000, Amount: 10_000_000, BillingIntervalHours: ptr(720)}},
			Payment: &models.Payment{CustomerID: customer, SubscriptionID: &sub, Rail: models.RailCCBill, TransactionID: "ccbill-after-cancel", Amount: 10_000_000, ListAmount: 10_000_000,
				Currency: "USD", Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
			Settled: true, SettlementSource: "cash"}, now)
		require.True(t, bill.Completed)
		schedules := orderSchedules(t, w, bill.Order.ID)
		require.Len(t, schedules, 1)
		sched := schedules[0]
		require.Equal(t, &start, sched.EndedAt, "the end before the schedule reaches it")
		require.Nil(t, sched.Next)
		require.Equal(t, []glRow{{Type: "revenue_recognized", Operation: "termination", Source: "revenue_schedule", SourceID: sched.ID.String() + "@" + now.Format(time.RFC3339Nano) + "#0",
			Debit: "deferred_revenue", Amount: 10_000_000, At: now, OrderLine: &sched.OrderLine, Schedule: &sched.ID}}, recognitions(t, w, sched.ID))
		after, err := w.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "pull.charge.after_cancellation"})
		require.NoError(t, err)
		require.Len(t, after.Items, 1, "and the charge is a person's to refund")
	})

	t.Run("OR-J41-S15/ledger-audit-warns-of-lines-from-before-the-books-and-a-tax-line-and-fails-a-later-unscheduled-line", func(t *testing.T) {
		w := newWorld(t)
		audit := "id:" + w.alpha.MerchantID().String()
		customer := uuid.New()
		// An order completed two days before the merchant's books started,
		// from before OpenRails kept them.
		before := time.Now().Add(-48 * time.Hour)
		legacy, legacyLine := rawOrder(t, w, customer, "USD", &before, rawLine{amount: 10_000_000})

		// The books start: a provider's sale, its tax unapplied.
		product, err := w.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "sale", DisplayName: "Sale", Ownership: catalog.OwnershipConsumable, Entitlements: []string{"sale:owned"}})
		require.NoError(t, err)
		_, err = w.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "sale", UnitAmount: 10_000_000, Currency: "USD"})
		require.NoError(t, err)
		priceID, productID := priceUUID(t, w, product.ID.UUID()), product.ID.UUID()
		psp := railPSP(t, w, "stripe")
		now := w.clock.Now().UTC().Truncate(time.Microsecond)
		sold := recordBill(t, w, observed.Bill{PSPID: psp, CustomerID: customer, Owner: observed.OwnerProvider, Kind: "purchase", SourceKind: "provider_purchase", SourceKey: "taxed-sale",
			Currency: "USD", Total: 11_000_000, Lines: []observed.Line{{PriceID: &priceID, ProductID: &productID, Description: "Sale", Quantity: ptr(1), UnitAmount: 10_000_000, Amount: 10_000_000},
				{Source: "provider_adjustment", Description: "Stripe invoice tax", UnitAmount: 1_000_000, Amount: 1_000_000}},
			Payment: &models.Payment{CustomerID: customer, Rail: models.RailStripe, TransactionID: "ch_taxed_sale", Amount: 11_000_000, ListAmount: 11_000_000, Currency: "USD",
				Status: "succeeded", MoneyMovement: models.MoneyMovementRail, PurchasedAt: now},
			Settled: true, SettlementSource: "cash"}, now)
		require.True(t, sold.Completed)
		require.Len(t, orderSchedules(t, w, sold.Order.ID), 1)
		// An order priced in the merchant's own unit: the books keep no unit.
		gems, _ := rawOrder(t, w, customer, "GEMS", nil, rawLine{amount: 500})

		output := hostCLI(t, w, "ledger-audit", "--merchant", audit)
		require.Contains(t, output, "Ledger integrity OK")
		require.Contains(t, output, "unscheduled line: order="+legacy.String()+" line="+legacyLine.String()+" amount=10.00 USD completed before the books started, awaiting their rebuild (warning)")
		require.Contains(t, output, "unapplied adjustment: order="+sold.Order.ID.String())
		require.NotContains(t, output, gems.String())

		// A line completed since the books started has no excuse.
		late, lateLine := rawOrder(t, w, customer, "USD", nil, rawLine{amount: 7_000_000})
		output, err = hostCLIResult(t, w, "ledger-audit", "--merchant", audit)
		require.Error(t, err)
		require.Contains(t, output, "unscheduled line: order="+late.String()+" line="+lateLine.String()+" amount=7.00 USD; ", "a failure, before the warnings")
		require.Contains(t, output, "completed before the books started")
	})

	t.Run("OR-J41-S16/no-payment-is-applied-beyond-what-it-brought-when-its-order-is-applied-again", func(t *testing.T) {
		start := time.Date(2026, time.July, 6, 9, 0, 0, 0, time.UTC)
		a, _ := newPaymentWorld(t, atClock(start))
		pass, passProduct := catalogLine(t, a, "pass", 22_000_000, ptr(720))
		sale, saleProduct := catalogLine(t, a, "sale", 14_000_000, nil)
		customer := uuid.MustParse(a.alice.ID)
		now := a.clock.Now().UTC().Truncate(time.Microsecond)

		// A bill of 36.00 that 20.00 in two payments paid: its sale is applied
		// at once, its pass waits for its access.
		order, _ := rawOrder(t, a.world, customer, "USD", &now, rawLine{price: &pass, product: &passProduct, amount: 22_000_000, accessHours: ptr(720)},
			rawLine{price: &sale, product: &saleProduct, amount: 14_000_000})
		own := rawReceipt(t, a.world, customer, order, 5_000_000, now.Add(-2*time.Hour))
		_, err := a.pool.Exec(t.Context(), "UPDATE "+booksTable(a.world, "orders")+" SET payment_id=$3 WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), order, own)
		require.NoError(t, err)
		other := rawReceipt(t, a.world, customer, order, 15_000_000, now.Add(-time.Hour))
		database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.ApplyRecorded(ctx, database.NewWithPgxTx(tx), order)
		}))
		require.Len(t, orderSchedules(t, a.world, order), 1)

		// A payment reported late sorts between the two; the access is
		// granted, and the order is applied again.
		late := rawReceipt(t, a.world, customer, order, 12_500_000, now.Add(-90*time.Minute))
		until := now.Add(720 * time.Hour)
		_, err = entitlements.NewEntitlementService(database, a.clock).PushAccess(ctx, entitlements.PushAccessParams{UserID: customer.String(), CustomerID: customer, ProductID: passProduct,
			NotBefore: &now, EndsAt: &until, SourceType: models.AccessSourcePurchase, SourceID: own.String(), PaymentID: &own})
		require.NoError(t, err)
		require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return books.ApplyOrder(ctx, database.NewWithPgxTx(tx), order, nil)
		}))
		schedules := orderSchedules(t, a.world, order)
		require.Len(t, schedules, 2)
		require.EqualValues(t, 7_777_779, schedules[0].Funded, "the sale had what the two payments brought")
		require.EqualValues(t, 22_000_000, schedules[1].Funded)

		// Each payment within what it brought: the first application's share
		// counts against the second.
		rows, err := a.pool.Query(t.Context(), "SELECT source_id, sum(amount)::bigint FROM "+booksTable(a.world, "ledger_transfers")+
			" WHERE merchant_id=$1 AND book='general' AND transfer_type='cash_applied' AND source='payment' GROUP BY source_id", a.alpha.MerchantID().UUID())
		require.NoError(t, err)
		applied := map[string]int64{}
		for rows.Next() {
			var id string
			var amount int64
			require.NoError(t, rows.Scan(&id, &amount))
			applied[id] = amount
		}
		require.NoError(t, rows.Err())
		require.Equal(t, map[string]int64{own.String(): 5_000_000, late.String(): 12_500_000, other.String(): 12_277_779}, applied)
		require.EqualValues(t, 2_722_221, unappliedCash(t, a.world, "USD"))
		require.Contains(t, hostCLI(t, a.world, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")
	})

	t.Run("OR-J41-S17/an-imported-purchase-is-applied-over-the-access-its-import-grants", func(t *testing.T) {
		start := time.Date(2026, time.July, 6, 9, 0, 0, 0, time.UTC)
		a, _ := newPaymentWorld(t, atClock(start))
		pass := oneOffPrice(t, a, "pass", 9_000_000, ptr(720))
		customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
		paid := start.Add(-240 * time.Hour)
		imported, err := a.alpha.ImportBilling(t.Context(), billing.DeclaredBilling{AsOf: a.clock.Now(), DefaultPSP: billing.PSPRef{Key: "stripe"}, Customers: []billing.DeclaredCustomer{{Customer: customer}},
			Purchases: []billing.DeclaredPurchase{{SourceID: "imported-pass", Customer: customer, Rail: "stripe", TransactionID: "ch_imported_pass", Currency: "USD", Amount: 9_000_000, PaidAt: paid,
				Lines: []billing.DeclaredOrderLine{{Price: pass, Quantity: ptr(1), UnitAmount: 9_000_000}}}}})
		require.NoError(t, err)
		require.Empty(t, imported.Blocked)

		// Recorded with its pass waiting, then applied again once the import
		// granted the access: over the window it granted, from when it was paid.
		schedules := schedulesWhere(t, a.world, "customer_id=$2", customer.UUID())
		require.Len(t, schedules, 1)
		require.NotNil(t, schedules[0].ProductAccess)
		require.Equal(t, revenueSchedule{Method: "ratable", Account: "one_time_revenue", Funded: 9_000_000, Starts: paid, Ends: paid.Add(720 * time.Hour), Through: paid,
			Next: ptr(time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC))}, schedules[0].terms())
		require.Zero(t, unappliedCash(t, a.world, "USD"))
		found, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "books.line.unapplied"})
		require.NoError(t, err)
		require.Empty(t, found.Items)
	})
}

// catalogLine is a one-off price of a new product, and that product.
func catalogLine(t *testing.T, a *authorityWorld, key string, amount int64, hours *int) (price, product uuid.UUID) {
	t.Helper()
	oneOffPrice(t, a, key, amount, hours)
	products, err := a.alpha.ListProducts(t.Context(), billing.ProductListParams{Keys: []string{key}})
	require.NoError(t, err)
	require.Len(t, products.Items, 1)
	product = products.Items[0].ID.UUID()
	return priceUUID(t, a.world, product), product
}

// rawLine is a line of a rawOrder: a catalog line when it names a price, else
// a provider's line.
type rawLine struct {
	price, product *uuid.UUID
	amount         int64
	accessHours    *int
}

// rawOrder is a completed order as stored data, written by no writer: made
// and completed at made (the database's now when nil). It returns the order
// and its first line.
func rawOrder(t *testing.T, w *world, customer uuid.UUID, currency string, made *time.Time, lines ...rawLine) (order, first uuid.UUID) {
	t.Helper()
	mid := w.alpha.MerchantID().UUID()
	order = uuid.New()
	var total int64
	for _, l := range lines {
		total += l.amount
	}
	_, err := w.pool.Exec(t.Context(), "INSERT INTO "+booksTable(w, "customers")+" (merchant_id, id) VALUES ($1, $2) ON CONFLICT DO NOTHING", mid, customer)
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+booksTable(w, "orders")+" (merchant_id, id, customer_id, origin, status, currency, total, number, payment_status, collection_owner, completed_at, created_at, updated_at)"+
		" VALUES ($1, $2, $3, 'customer', 'complete', $4, $5, (SELECT COALESCE(max(number), 0) + 1000 FROM "+booksTable(w, "orders")+" WHERE merchant_id = $1), 'succeeded', 'provider',"+
		" COALESCE($6::timestamptz, now()), COALESCE($6::timestamptz, now()), COALESCE($6::timestamptz, now()))", mid, order, customer, currency, total, made)
	require.NoError(t, err)
	for i, l := range lines {
		id := uuid.New()
		if i == 0 {
			first = id
		}
		var source, ownership *string
		if l.price == nil {
			source = ptr("provider_line")
		} else {
			ownership = ptr("consumable")
		}
		_, err := w.pool.Exec(t.Context(), "INSERT INTO "+booksTable(w, "order_lines")+" (merchant_id, id, order_id, customer_id, position, price_id, product_id, description, quantity, unit_amount, amount, ownership, access_duration_hours, source)"+
			" VALUES ($1, $2, $3, $4, $5, $6, $7, 'Fixture line', 1, $8, $8, $9, $10, $11)", mid, id, order, customer, i, l.price, l.product, l.amount, ownership, l.accessHours, source)
		require.NoError(t, err)
	}
	return order, first
}

// rawReceipt is a payment received outside OpenRails toward an order, as
// stored data.
func rawReceipt(t *testing.T, w *world, customer, order uuid.UUID, amount int64, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := w.pool.Exec(t.Context(), "INSERT INTO "+booksTable(w, "payments")+" (merchant_id, id, customer_id, order_id, channel, transaction_id, amount, list_amount, currency, status, purchased_at)"+
		" VALUES ($1, $2, $3, $4, 'manual', $5, $6, $6, 'USD', 'succeeded', $7)", w.alpha.MerchantID().UUID(), id, customer, order, "receipt-"+id.String(), amount, at)
	require.NoError(t, err)
	return id
}

// atClock sets a world's clock before its engine starts.
func atClock(at time.Time) func(*world) {
	return func(w *world) { w.clock.Advance(at.Sub(w.clock.Now())) }
}

// passAt moves the clock to at and runs one billing pass.
func passAt(t *testing.T, a *authorityWorld, fleet *river.Client[pgx.Tx], at time.Time) {
	t.Helper()
	a.clock.Advance(at.Sub(a.clock.Now()))
	runRegistered(t, fleet, "openrails.billing_pass", 1)
}

// calendarPrice is a recurring price of a new product, billed every months.
func calendarPrice(t *testing.T, a *authorityWorld, key string, amount int64, months int) billing.PriceID {
	t.Helper()
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{key + ":member"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: amount, Currency: "USD", BillingIntervalMonths: ptr(months)})
	require.NoError(t, err)
	return price.ID
}

// oneOffPrice is a one-time price of a new product, with access for hours or
// for good.
func oneOffPrice(t *testing.T, a *authorityWorld, key string, amount int64, hours *int) billing.PriceID {
	t.Helper()
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{key + ":owned"}})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: amount, Currency: "USD", AccessDurationHours: hours})
	require.NoError(t, err)
	return price.ID
}

// buy is alice paying for lines with a new card she keeps.
func buy(t *testing.T, a *authorityWorld, provider *providerLedger, key string, lines []billing.OrderLineParams, total int64) billing.Order {
	t.Helper()
	input := billing.CreateMyOrderParams{Lines: lines, ExpectedTotal: ptr(total), Reusable: ptr(true),
		Payment: &billing.OrderPaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, input, map[string]string{"Idempotency-Key": key})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var order billing.Order
	require.NoError(t, json.Unmarshal(raw, &order))
	require.Equal(t, billing.OrderComplete, order.Status, string(raw))
	require.NotNil(t, order.Payment.PaymentID)
	return order
}

// glRow is one general-ledger transfer as the books keep it.
type glRow struct {
	Type, Operation, Source, SourceID string
	Debit                             string
	DebitPSP                          *uuid.UUID
	Credit                            string
	Amount                            int64
	At                                time.Time
	OrderLine, Schedule               *uuid.UUID
}

// recognized is one of a schedule's recognitions; a termination is in its
// first generation.
func recognized(schedule, line uuid.UUID, op string, at, dated time.Time, amount int64) glRow {
	r := glRow{Type: "revenue_recognized", Operation: op, Source: "revenue_schedule", SourceID: schedule.String() + "@" + at.UTC().Format(time.RFC3339Nano),
		Debit: "deferred_revenue", Credit: "", Amount: amount, At: dated.UTC(), OrderLine: &line, Schedule: &schedule}
	if op == "termination" {
		r.SourceID += "#0"
	}
	return r
}

// inGeneration is a termination posted again after n undone ends.
func inGeneration(r glRow, n int) glRow {
	r.SourceID = strings.TrimSuffix(r.SourceID, "#0") + "#" + strconv.Itoa(n)
	return r
}

func glQuery(t *testing.T, w *world, where string, args ...any) []glRow {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT t.transfer_type, t.operation, t.source, t.source_id, d.account_type, d.psp_id, c.account_type, t.amount, t.effective_at, t.order_line_id, t.revenue_schedule_id FROM "+
		booksTable(w, "ledger_transfers")+" t JOIN "+booksTable(w, "ledger_accounts")+" d ON d.merchant_id=t.merchant_id AND d.id=t.debit_account_id JOIN "+
		booksTable(w, "ledger_accounts")+" c ON c.merchant_id=t.merchant_id AND c.id=t.credit_account_id WHERE t.merchant_id=$1 AND t.book='general' AND "+where+" ORDER BY t.id",
		append([]any{w.alpha.MerchantID().UUID()}, args...)...)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (glRow, error) {
		var r glRow
		err := row.Scan(&r.Type, &r.Operation, &r.Source, &r.SourceID, &r.Debit, &r.DebitPSP, &r.Credit, &r.Amount, &r.At, &r.OrderLine, &r.Schedule)
		r.At = r.At.UTC()
		return r, err
	})
	require.NoError(t, err)
	return out
}

// glFor is the general-ledger transfers at one source.
func glFor(t *testing.T, w *world, source, id string) []glRow {
	t.Helper()
	return glQuery(t, w, "t.source=$2 AND t.source_id=$3", source, id)
}

// recognitions is a schedule's recognitions, its revenue account left blank.
func recognitions(t *testing.T, w *world, schedule uuid.UUID) []glRow {
	t.Helper()
	out := glQuery(t, w, "t.revenue_schedule_id=$2 AND t.transfer_type='revenue_recognized'", schedule)
	for i := range out {
		out[i].Credit = ""
	}
	return out
}

// glAccount is the revenue account a schedule's recognitions credit.
func glAccount(t *testing.T, w *world, schedule uuid.UUID) string {
	t.Helper()
	out := glQuery(t, w, "t.revenue_schedule_id=$2 AND t.transfer_type='revenue_recognized'", schedule)
	require.NotEmpty(t, out)
	return out[0].Credit
}

// scheduleLegCount is how many general-ledger legs name a revenue schedule.
func scheduleLegCount(t *testing.T, w *world) int {
	t.Helper()
	var n int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+booksTable(w, "ledger_transfers")+" WHERE merchant_id=$1 AND book='general' AND revenue_schedule_id IS NOT NULL", w.alpha.MerchantID().UUID()).Scan(&n))
	return n
}

// unappliedCash is unapplied_cash's balance in a currency: a merchant-wide
// account's balance is its type total.
func unappliedCash(t *testing.T, w *world, currency string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT COALESCE(sum(credits_posted - debits_posted), 0) FROM "+booksTable(w, "ledger_type_totals")+" WHERE merchant_id=$1 AND book='general' AND account_type='unapplied_cash' AND currency=$2",
		w.alpha.MerchantID().UUID(), currency).Scan(&n))
	return n
}

// revenueSchedule is a schedule's terms and progress.
type revenueSchedule struct {
	Method, Account       string
	Funded, Recognized    int64
	Starts, Ends, Through time.Time
	Next                  *time.Time
}

type revenueScheduleRow struct {
	ID, OrderLine  uuid.UUID
	Currency       string
	ProductAccess  *uuid.UUID
	Method         string
	Account        string
	Funded         int64
	Recognized     int64
	Starts, Ends   time.Time
	Months         *int32
	EndedAt        *time.Time
	Through        time.Time
	Next           *time.Time
	Reinstatements int32
}

func (r revenueScheduleRow) terms() revenueSchedule {
	return revenueSchedule{Method: r.Method, Account: r.Account, Funded: r.Funded, Recognized: r.Recognized, Starts: r.Starts, Ends: r.Ends, Through: r.Through, Next: r.Next}
}

func schedulesWhere(t *testing.T, w *world, where string, arg any) []revenueScheduleRow {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT id, order_line_id, currency, product_access_id, method, revenue_account, funded, recognized_net, service_starts_at, service_ends_at, service_months, ended_at, recognized_through_at, next_recognition_at, reinstatements FROM "+
		booksTable(w, "revenue_schedules")+" WHERE merchant_id=$1 AND "+where+" ORDER BY created_at, id", w.alpha.MerchantID().UUID(), arg)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (revenueScheduleRow, error) {
		var r revenueScheduleRow
		err := row.Scan(&r.ID, &r.OrderLine, &r.Currency, &r.ProductAccess, &r.Method, &r.Account, &r.Funded, &r.Recognized, &r.Starts, &r.Ends, &r.Months, &r.EndedAt, &r.Through, &r.Next, &r.Reinstatements)
		r.Starts, r.Ends, r.Through = r.Starts.UTC(), r.Ends.UTC(), r.Through.UTC()
		r.EndedAt, r.Next = utcPtr(r.EndedAt), utcPtr(r.Next)
		return r, err
	})
	require.NoError(t, err)
	return out
}

func scheduleOfLine(t *testing.T, w *world, line uuid.UUID) revenueScheduleRow {
	t.Helper()
	out := schedulesWhere(t, w, "order_line_id=$2", line)
	require.Len(t, out, 1, "one schedule per paid line")
	return out[0]
}

func orderSchedules(t *testing.T, w *world, order uuid.UUID) []revenueScheduleRow {
	t.Helper()
	return schedulesWhere(t, w, "order_id=$2", order)
}

func subscriptionSchedules(t *testing.T, w *world, sub uuid.UUID) []revenueScheduleRow {
	t.Helper()
	return schedulesWhere(t, w, "subscription_id=$2", sub)
}

// scheduleVersion is a schedule row's version and lock: a pass that took it
// changes one or the other.
func scheduleVersion(t *testing.T, w *world, id uuid.UUID) [2]string {
	t.Helper()
	var v [2]string
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT xmin::text, xmax::text FROM "+booksTable(w, "revenue_schedules")+" WHERE merchant_id=$1 AND id=$2", w.alpha.MerchantID().UUID(), id).Scan(&v[0], &v[1]))
	return v
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a.UTC()
	}
	return b.UTC()
}

// priceUUID is the row id of a product's one price.
func priceUUID(t *testing.T, w *world, product uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT id FROM "+booksTable(w, "prices")+" WHERE merchant_id=$1 AND product_id=$2", w.alpha.MerchantID().UUID(), product).Scan(&id))
	return id
}

// railPSP is a PSP of the rail for fixture bills; no charge reaches it.
func railPSP(t *testing.T, w *world, rail string) uuid.UUID {
	t.Helper()
	environment := "test"
	if rail == "solana" {
		environment = "devnet"
	}
	var id uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), "INSERT INTO "+booksTable(w, "psps")+" (merchant_id, key, rail, environment, account_id) VALUES ($1, $2, $3, $4, $5) RETURNING id",
		w.alpha.MerchantID().UUID(), "books-"+rail, rail, environment, uuid.NewString()).Scan(&id))
	return id
}

// fixtureSubscription is a subscription a provider bills, as its import would
// leave it: canceled and ended at ended when ended is set.
func fixtureSubscription(t *testing.T, w *world, customer, product, price, psp uuid.UUID, rail, policy string, start, end time.Time, ended *time.Time) uuid.UUID {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	var id uuid.UUID
	require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO billing.customers (merchant_id, id) VALUES ($1, $2) ON CONFLICT DO NOTHING", w.alpha.MerchantID().UUID(), customer); err != nil {
			return err
		}
		var ref *string
		if policy == "provider" {
			ref = ptr(rail + "-sub-" + psp.String())
		}
		status, cancel := "active", (*string)(nil)
		if ended != nil {
			status, cancel = "canceled", ptr("user")
		}
		return tx.QueryRow(ctx, "INSERT INTO billing.subscriptions (merchant_id, customer_id, product_id, price_id, status, rail, collection_policy, rail_subscription_id, psp_id, started_at, current_period_starts_at, current_period_ends_at, canceled_at, ended_at, cancel_type)"+
			" VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10, $11, $12, $12, $13) RETURNING id",
			w.alpha.MerchantID().UUID(), customer, product, price, status, rail, policy, ref, psp, start, end, ended, cancel).Scan(&id)
	}))
	return id
}

// recordBill records a verified provider bill as each rail's observer does.
func recordBill(t *testing.T, w *world, bill observed.Bill, now time.Time) observed.Result {
	t.Helper()
	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	var result observed.Result
	require.NoError(t, database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = observed.Record(ctx, database.NewWithPgxTx(tx), bill, now)
		return err
	}))
	return result
}

// windowSnapshot is a provider's complete record of one window, holding one
// charge and one subscription OpenRails never saw.
type windowSnapshot struct{ since, until time.Time }

func (windowSnapshot) Name() string { return "stripe" }

func (windowSnapshot) Capabilities() reconcile.Capabilities {
	return reconcile.Capabilities{Transactions: true}
}

func (s windowSnapshot) Fetch(context.Context, reconcile.FetchParams) (*reconcile.RemoteSnapshot, error) {
	return &reconcile.RemoteSnapshot{Provider: reconcile.ProviderStripe, Transactions: []reconcile.RemoteTransaction{{TransactionID: "ch_still_there"}},
		Subscriptions: []reconcile.RemoteSubscription{{RailSubscriptionID: "sub_still_there"}},
		Coverage: reconcile.SnapshotCoverage{SubscriptionsExhaustive: true, TransactionsExhaustive: true, TransactionsPaginatedComplete: true,
			TransactionWindowSince: &s.since, TransactionWindowUntil: &s.until}}, nil
}
