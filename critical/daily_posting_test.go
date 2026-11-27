//go:build integration

package critical_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// One posting a day takes a customer's usage and running charges onto one
// receipt, at the end of the merchant's accounting-zone day.
func TestDailyPosting(t *testing.T) {
	t.Run("OR-J25-S45/a-running-charge-posts-at-the-accounting-zones-midnight-on-the-days-receipt", chargeAtZoneMidnight)
	t.Run("OR-J25-S46/a-stop-posts-nothing-until-the-daily-posting", stopPostsNothing)
	t.Run("OR-J24-S88/a-receipt-holds-a-volume-fall-beside-new-usage", receiptHoldsAFall)
	t.Run("OR-J48-S20/a-posting-in-a-unit-forgives-with-the-event-store-down", unitPostingWithoutStore)
	t.Run("OR-J24-S89/a-posting-past-a-periods-end-dates-each-periods-owed-in-its-period", owedInItsPeriod)
	t.Run("OR-J24-S90", func(t *testing.T) {
		t.Run("a-fall-is-credited-before-a-later-rise-is-spent-in-usd", func(t *testing.T) { fallBeforeRise(t, "USD") })
		t.Run("a-fall-is-credited-before-a-later-rise-is-spent-in-a-unit", func(t *testing.T) { fallBeforeRise(t, "GOLD") })
	})
}

// calendarMonthsAtMonthEnd prices usage by calendar month and starts at noon
// on a month's last day.
func calendarMonthsAtMonthEnd(w *world) {
	m := cfgMerchant(w.cfg, "alpha")
	m.BillingPeriodBoundary = "calendar_month"
	setCfgMerchant(&w.cfg, "alpha", m)
	now := w.clock.Now().UTC()
	advanceTo(w, time.Date(now.Year(), now.Month()+2, 1, 0, 0, 0, 0, time.UTC).Add(-12*time.Hour))
}

// priceMeter prices an event type's property in a currency.
func priceMeter(t *testing.T, client *openrails.Client, key, eventType, property string, currency catalog.Currency, price catalog.RatePrice) {
	t.Helper()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key})
	require.NoError(t, err)
	price.Currency = currency
	_, err = client.SetMeter(t.Context(), key, billing.SetMeterParams{EventType: eventType, ValueProperty: property, Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: price})})
	require.NoError(t, err)
}

func owedInItsPeriod(t *testing.T) {
	w := newWorld(t, calendarMonthsAtMonthEnd)
	usageMeter(t, w.alpha, 100)
	// An unfunded customer: what posts is owed.
	customer := billing.CustomerID(uuid.New())
	settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "last-day", 2))...)
	last := w.clock.Now().UTC()
	boundary := time.Date(last.Year(), last.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	advanceTo(w, boundary.Add(30*time.Second))
	settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "first-minute", 3))...)
	fleet := startFleet(t, w)
	posted := boundary.Add(time.Minute)
	advanceTo(w, posted)
	postDays(t, fleet)
	require.EqualValues(t, 500, owed(t, w.alpha, customer))

	// The day's posting, a minute into the new month, dates the old month's
	// owed before its end and the new month's when it posted.
	type dated struct {
		at     time.Time
		amount int64
	}
	read := func(query string) []dated {
		t.Helper()
		rows, err := w.pool.Query(t.Context(), query, customer.UUID())
		require.NoError(t, err)
		defer rows.Close()
		var out []dated
		for rows.Next() {
			var d dated
			require.NoError(t, rows.Scan(&d.at, &d.amount))
			d.at = d.at.UTC()
			out = append(out, d)
		}
		require.NoError(t, rows.Err())
		return out
	}
	want := []dated{{boundary.Add(-time.Microsecond), 200}, {posted, 300}}
	require.Equal(t, want, read("SELECT accrued_at, amount FROM "+pgx.Identifier{w.schema, "owed_items"}.Sanitize()+
		" WHERE customer_id = $1 ORDER BY accrued_at"), "each period's owed item in its period")
	require.Equal(t, want, read("SELECT effective_at, amount FROM "+pgx.Identifier{w.schema, "ledger_transfers"}.Sanitize()+
		" WHERE customer_id = $1 AND book = 'general' AND transfer_type = 'usage_accrued' ORDER BY effective_at"), "and its revenue in the books")
}

func fallBeforeRise(t *testing.T, currency catalog.Currency) {
	var w *world
	var client *openrails.Client
	customer := billing.CustomerID(uuid.New())
	if currency == "GOLD" {
		a := unitWorld(t, calendarMonthsAtMonthEnd)
		w, client, customer = a.world, a.alpha, billing.CustomerID(uuid.MustParse(a.alice.ID))
	} else {
		w = newWorld(t, calendarMonthsAtMonthEnd)
		client = w.alpha
	}
	// 10 a unit, 5 each once there are more than 10; storage 3 a gigabyte.
	priceMeter(t, client, "compute", "compute", "units", currency, tiers(catalog.TierModeVolume, 10, 5, 10))
	priceMeter(t, client, "storage", "storage", "gb", currency, catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 3, DivideBy: 1}})
	_, err := client.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: currency, Amount: 80, Source: "critical", SourceID: "eighty"}})
	require.NoError(t, err)
	use := func(id, eventType, property string, n int64, at time.Time) billing.LeaseUsage {
		return billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: currency, EventType: eventType,
			Dimensions: map[string]int64{property: n}, Source: "critical", SourceID: id, OccurredAt: at}
	}
	last := w.clock.Now().UTC()
	lease := newLease()
	settleUsage(t, client, lease, numbered(1, use("eight", "compute", "units", 8, time.Time{}))...)
	fleet := startFleet(t, w)
	nextDay(w)
	postDays(t, fleet)
	unitBalance(t, client, customer, currency, 0, 0, 0)

	// Four more of the last month's day bring its price to 60, 20 less than
	// posted; the new month's storage costs 15. The posting credits the 20
	// first, and the 15 draws it: 5 left, nothing owed or forgiven.
	w.clock.Advance(10 * time.Hour)
	settleUsage(t, client, lease, numbered(2, use("late", "compute", "units", 4, last.Add(time.Hour)), use("storage", "storage", "gb", 5, time.Time{}))...)
	nextDay(w)
	postDays(t, fleet)
	b := unitBalance(t, client, customer, currency, 5, 0, 5)
	require.Zero(t, b.OwedAmount, "the rise draws the fall's lot, nothing is owed")
	receipts := usageReceiptRows(t, w, customer)
	require.Len(t, receipts, 2)
	require.Equal(t, [2]int64{-5, 0}, [2]int64{receipts[1].amount, receipts[1].forgiven}, "nothing is forgiven")
}

func chargeAtZoneMidnight(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	w := newWorld(t, func(w *world) {
		m := cfgMerchant(w.cfg, "alpha")
		m.Accounting = &billing.AccountingSettings{Timezone: "America/New_York"}
		setCfgMerchant(&w.cfg, "alpha", m)
	})
	local := w.clock.Now().In(ny)
	start := time.Date(local.Year(), local.Month(), local.Day()+1, 18, 0, 0, 0, ny)
	midnight := time.Date(local.Year(), local.Month(), local.Day()+2, 0, 0, 0, 0, ny)
	advanceTo(w, start)
	usageMeter(t, w.alpha, 100)
	customer, _ := fundedCustomer(t, w)
	_, err = w.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "evening", CustomerID: customer, Currency: "USD", RatePerHour: 120})
	require.NoError(t, err)
	w.clock.Advance(10 * time.Minute)
	settleUsage(t, w.alpha, newLease(), numbered(1, usageInput(customer, "evening-work", 2))...)
	fleet := startFleet(t, w)

	// Past UTC's midnight New York's day has not ended: nothing posts.
	advanceTo(w, start.UTC().Truncate(24*time.Hour).Add(24*time.Hour+30*time.Minute))
	postDays(t, fleet)
	require.Equal(t, []int64{1000}, ledgerAmounts(t, w.alpha, customer), "nothing posts at 00:00 UTC")
	charge, err := w.alpha.GetRunningCharge(t.Context(), "evening")
	require.NoError(t, err)
	require.Zero(t, charge.PostedAmount)
	require.Zero(t, receiptCount(t, w, customer))

	// Past New York's midnight its day posts: the usage and the charge
	// through that midnight, one receipt, one spend.
	advanceTo(w, midnight.Add(time.Minute))
	postDays(t, fleet)
	require.EqualValues(t, 1, receiptCount(t, w, customer))
	charged := int64(midnight.Sub(start)/time.Hour) * 120
	receipt := usageReceiptRows(t, w, customer)[0]
	require.Equal(t, "daily", receipt.reason)
	require.EqualValues(t, 200+charged, receipt.amount)
	require.Len(t, receipt.lines, 1)
	require.Equal(t, [3]any{"compute", int64(2), int64(200)}, [3]any{receipt.lines[0].MeterKey, receipt.lines[0].Quantity, receipt.lines[0].Amount})
	require.Len(t, receipt.charges, 1)
	require.Equal(t, charged, receipt.charges[0].Amount)
	require.True(t, receipt.charges[0].StartsAt.Equal(start), "from its start: %s", receipt.charges[0].StartsAt)
	require.True(t, receipt.charges[0].EndsAt.Equal(midnight), "through New York's midnight: %s", receipt.charges[0].EndsAt)
	require.Equal(t, []int64{-(200 + charged), 1000}, ledgerAmounts(t, w.alpha, customer))
	charge, err = w.alpha.GetRunningCharge(t.Context(), "evening")
	require.NoError(t, err)
	require.Equal(t, charged, charge.PostedAmount)
	b, err := w.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
	require.NoError(t, err)
	require.Equal(t, [3]int64{1000 - 200 - charged, 2, 1000 - 200 - charged - 2}, [3]int64{b.BalanceAmount, b.AccruedAmount, b.AvailableAmount},
		"the minute since midnight accrues")
	// It runs on: due again at New York's next midnight.
	next := time.Date(local.Year(), local.Month(), local.Day()+3, 0, 0, 0, 0, ny)
	require.True(t, usagePendingDue(t, w, customer).Equal(next), "due at %s", next)
}

func stopPostsNothing(t *testing.T) {
	w := runningChargeWorld(t)
	customer, _ := fundedCustomer(t, w)
	started := w.clock.Now()
	_, err := w.alpha.StartRunningCharge(t.Context(), billing.StartRunningChargeParams{Reference: "quarter-hour", CustomerID: customer, Currency: "USD", RatePerHour: 3600})
	require.NoError(t, err)
	w.clock.Advance(900 * time.Second)
	stoppedAt := w.clock.Now()
	stopped, err := w.alpha.StopRunningCharge(t.Context(), "quarter-hour", billing.StopRunningChargeParams{StoppedAt: ptr(stoppedAt)})
	require.NoError(t, err)
	require.Equal(t, billing.RunningChargeStopped, stopped.State)
	require.Equal(t, [2]int64{900, 0}, [2]int64{stopped.ChargedAmount, stopped.PostedAmount}, "the stop posts nothing")
	accrued := func(balance, accrued, available int64) {
		t.Helper()
		b, err := w.alpha.GetBalance(t.Context(), customer, billing.GetBalanceParams{Currency: "USD"})
		require.NoError(t, err)
		require.Equal(t, [3]int64{balance, accrued, available}, [3]int64{b.BalanceAmount, b.AccruedAmount, b.AvailableAmount})
	}
	accrued(1000, 900, 100)
	require.Equal(t, []int64{1000}, ledgerAmounts(t, w.alpha, customer))
	require.Zero(t, receiptCount(t, w, customer))

	// The accrual stopped with the charge, and the day has not ended.
	w.clock.Advance(2 * time.Hour)
	fleet := startFleet(t, w)
	postDays(t, fleet)
	accrued(1000, 900, 100)
	require.Equal(t, []int64{1000}, ledgerAmounts(t, w.alpha, customer))

	// The daily posting takes it, through its stop, once.
	nextDay(w)
	postDays(t, fleet)
	postDays(t, fleet)
	accrued(100, 0, 100)
	require.Equal(t, []int64{-900, 1000}, ledgerAmounts(t, w.alpha, customer))
	final, err := w.alpha.GetRunningCharge(t.Context(), "quarter-hour")
	require.NoError(t, err)
	require.Equal(t, [2]int64{900, 900}, [2]int64{final.ChargedAmount, final.PostedAmount})
	receipts := usageReceiptRows(t, w, customer)
	require.Len(t, receipts, 1)
	require.Equal(t, "daily", receipts[0].reason)
	require.Empty(t, receipts[0].lines)
	require.Len(t, receipts[0].charges, 1)
	require.EqualValues(t, 900, receipts[0].charges[0].Amount)
	require.True(t, receipts[0].charges[0].StartsAt.Equal(started) && receipts[0].charges[0].EndsAt.Equal(stoppedAt), "%+v", receipts[0].charges[0])
	require.Zero(t, queuedCount(t, w, customer), "nothing is left to post")
}

func receiptHoldsAFall(t *testing.T) {
	w := newWorld(t, calendarMonthsAtMonthEnd)
	// 10 a unit, 5 each once there are more than 10.
	priceMeter(t, w.alpha, "compute", "compute", "units", "USD", tiers(catalog.TierModeVolume, 10, 5, 10))
	priceMeter(t, w.alpha, "storage", "storage", "gb", "USD", catalog.RatePrice{Model: catalog.ModelPerUnit, PerUnit: &catalog.PerUnitPrice{UnitAmount: 3, DivideBy: 1}})
	customer, _ := fundedCustomer(t, w)
	last := w.clock.Now().UTC()
	october := time.Date(last.Year(), last.Month(), 1, 0, 0, 0, 0, time.UTC)
	november := october.AddDate(0, 1, 0)
	lease := newLease()
	settleUsage(t, w.alpha, lease, numbered(1, usageInput(customer, "eight", 8))...)
	fleet := startFleet(t, w)
	nextDay(w)
	postDays(t, fleet)
	balance(t, w.alpha, customer, 920, 0, 920)

	// Four more of the last month's day, reported late, bring its price to
	// 12 at 5: 20 less than posted; the new month's storage costs 15.
	w.clock.Advance(10 * time.Hour)
	late := usageInput(customer, "late", 4)
	late.OccurredAt = last.Add(time.Hour)
	storage := billing.LeaseUsage{CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EventType: "storage",
		Dimensions: map[string]int64{"gb": 5}, Source: "critical", SourceID: "storage"}
	settleUsage(t, w.alpha, lease, numbered(2, late, storage)...)
	balance(t, w.alpha, customer, 905, 15, 920)
	nextDay(w)
	postDays(t, fleet)
	require.EqualValues(t, 2, receiptCount(t, w, customer), "the fall and the new usage are one receipt")
	receipt := usageReceiptRows(t, w, customer)[1]
	require.Equal(t, "daily", receipt.reason)
	require.EqualValues(t, -5, receipt.amount)
	require.Len(t, receipt.lines, 2)
	require.True(t, receipt.lines[0].PeriodStartsAt.Equal(october) && receipt.lines[1].PeriodStartsAt.Equal(november), "%+v", receipt.lines)
	require.Equal(t, [2][3]any{{"compute", int64(4), int64(-20)}, {"storage", int64(5), int64(15)}},
		[2][3]any{{receipt.lines[0].MeterKey, receipt.lines[0].Quantity, receipt.lines[0].Amount}, {receipt.lines[1].MeterKey, receipt.lines[1].Quantity, receipt.lines[1].Amount}})
	// The rise is spent from the lot and the fall credited as a lot, both at
	// the receipt's coordinate.
	require.ElementsMatch(t, []int64{-15, 20}, receiptLegs(t, w.alpha, customer, receipt.id))
	balance(t, w.alpha, customer, 925, 0, 925)
	// Each period's receipts sum to the price of its quantity.
	require.Equal(t, map[string]int64{"compute": 60, "storage": 15}, linesByMeter(t, w, customer))
}

func unitPostingWithoutStore(t *testing.T) {
	var conn *downConn
	a := unitWorld(t, func(w *world) {
		conn = &downConn{Conn: w.deps.ClickHouse}
		w.deps.ClickHouse = conn
	})
	imageMeter(t, a)
	alice := billing.CustomerID(uuid.MustParse(a.alice.ID))
	_, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: alice, Currency: "GOLD", Amount: 10, Source: "critical", SourceID: "ten"}})
	require.NoError(t, err)
	used := a.clock.Now()
	useImages(t, a, alice, "five", 5)
	unitBalance(t, a.alpha, alice, "GOLD", -10, 20, 10)

	// The posting reads no store: it takes the 10 the lot holds and forgives
	// the rest by its own lines.
	conn.down.Store(true)
	fleet := quietFleet(t, a.world)
	nextDay(a.world)
	runPass(t, fleet)
	require.EqualValues(t, 1, receiptCount(t, a.world, alice), "the posting needs no event store")
	unitBalance(t, a.alpha, alice, "GOLD", 0, 0, 0)
	receipt := usageReceiptRows(t, a.world, alice)[0]
	require.Equal(t, [2]int64{20, 10}, [2]int64{receipt.amount, receipt.forgiven})
	require.Len(t, receipt.lines, 1)
	require.Equal(t, [3]any{"images", int64(20), int64(10)}, [3]any{receipt.lines[0].MeterKey, receipt.lines[0].Amount, receipt.lines[0].Forgiven})

	// Forgiven usage shows by rate card, with no invoker.
	conn.down.Store(false)
	require.Equal(t, map[string]int64{"charged": 10, "forgiven": 10}, usageChargedBetween(t, a, alice, used, a.clock.Now()))
	require.Equal(t, map[string]int64{"image/charged": 10, "image/forgiven": 10}, usageChargedBetween(t, a, alice, used, a.clock.Now(), "rate_card"))
	require.Equal(t, map[string]int64{alice.String() + "/charged": 20, "/charged": -10, "/forgiven": 10}, usageChargedBetween(t, a, alice, used, a.clock.Now(), "invoker"))
}

// ledgerAmounts are the customer's USD balance movements, newest first.
func ledgerAmounts(t *testing.T, client *openrails.Client, customer billing.CustomerID) []int64 {
	t.Helper()
	page, err := client.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
	require.NoError(t, err)
	out := make([]int64, 0, len(page.Items))
	for _, item := range page.Items {
		out = append(out, item.Amount)
	}
	return out
}

// receiptLegs are the customer's USD movements at a usage receipt's
// coordinate.
func receiptLegs(t *testing.T, client *openrails.Client, customer billing.CustomerID, receipt uuid.UUID) []int64 {
	t.Helper()
	page, err := client.ListBalanceTransactions(t.Context(), customer, billing.BalanceTransactionListParams{Currency: "USD"})
	require.NoError(t, err)
	var out []int64
	for _, item := range page.Items {
		if item.Source == "usage_receipt:"+receipt.String() {
			out = append(out, item.Amount)
		}
	}
	return out
}

func receiptCount(t *testing.T, w *world, customer billing.CustomerID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "usage_receipts"}.Sanitize()+" WHERE customer_id = $1",
		customer.UUID()).Scan(&n))
	return n
}

func queuedCount(t *testing.T, w *world, customer billing.CustomerID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+pgx.Identifier{w.schema, "usage_pending"}.Sanitize()+" WHERE customer_id = $1",
		customer.UUID()).Scan(&n))
	return n
}

func usagePendingDue(t *testing.T, w *world, customer billing.CustomerID) time.Time {
	t.Helper()
	var due time.Time
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT due_at FROM "+pgx.Identifier{w.schema, "usage_pending"}.Sanitize()+" WHERE customer_id = $1",
		customer.UUID()).Scan(&due))
	return due
}

// storedReceipt is one usage_receipts row as stored.
type storedReceipt struct {
	id               uuid.UUID
	reason           string
	amount, forgiven int64
	lines            []storedReceiptLine
	charges          []storedReceiptCharge
}

type storedReceiptLine struct {
	PeriodStartsAt time.Time `json:"period_starts_at"`
	MeterKey       string    `json:"meter_key"`
	Quantity       int64     `json:"quantity"`
	Amount         int64     `json:"amount"`
	Forgiven       int64     `json:"forgiven"`
}

type storedReceiptCharge struct {
	StartsAt time.Time `json:"starts_at"`
	EndsAt   time.Time `json:"ends_at"`
	Amount   int64     `json:"amount"`
}

// usageReceiptRows are the customer's usage receipts, oldest first.
func usageReceiptRows(t *testing.T, w *world, customer billing.CustomerID) []storedReceipt {
	t.Helper()
	rows, err := w.pool.Query(t.Context(), "SELECT id, reason, amount, forgiven, lines, charges FROM "+pgx.Identifier{w.schema, "usage_receipts"}.Sanitize()+
		" WHERE customer_id = $1 ORDER BY created_at, id", customer.UUID())
	require.NoError(t, err)
	defer rows.Close()
	var out []storedReceipt
	for rows.Next() {
		var r storedReceipt
		var lines, charges []byte
		require.NoError(t, rows.Scan(&r.id, &r.reason, &r.amount, &r.forgiven, &lines, &charges))
		require.NoError(t, json.Unmarshal(lines, &r.lines))
		require.NoError(t, json.Unmarshal(charges, &r.charges))
		out = append(out, r)
	}
	require.NoError(t, rows.Err())
	return out
}

// linesByMeter is what the customer's receipts took per meter.
func linesByMeter(t *testing.T, w *world, customer billing.CustomerID) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, r := range usageReceiptRows(t, w, customer) {
		for _, l := range r.lines {
			out[l.MeterKey] += l.Amount
		}
	}
	return out
}
