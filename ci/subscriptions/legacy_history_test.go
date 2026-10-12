//go:build e2e && integration

package subscriptions_test

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/nmimock"
)

type chargeAttributionPass struct{}

func (chargeAttributionPass) Kind() string { return "openrails.charge_attributions" }

// resolveAttributions runs one charge-attribution pass and waits for it.
func (w *world) resolveAttributions() {
	w.t.Helper()
	res, err := w.jobs.Insert(w.t.Context(), chargeAttributionPass{}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(w.t, err)
	// A large book's pass reads thousands of transactions.
	require.Eventually(w.t, func() bool {
		job, err := w.jobs.JobGet(w.t.Context(), res.Job.ID)
		return err == nil && job.State == rivertype.JobStateCompleted
	}, 5*time.Minute, 50*time.Millisecond, "charge attribution pass")
	w.settle()
}

// attributions is the merchant's declared charge attributions by status.
func (w *world) attributions() map[string][]string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT status, transaction_id FROM billing.provider_charge_attributions ORDER BY transaction_id`))
	require.NoError(w.t, err)
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var status, id string
		require.NoError(w.t, rows.Scan(&status, &id))
		out[status] = append(out[status], id)
	}
	require.NoError(w.t, rows.Err())
	return out
}

// billRow is one order and its payment as stored.
type billRow struct {
	origin, owner, status, kind, source string
	total, paid, unit                   int64
	periodStart, periodEnd, purchasedAt *time.Time
	providerAmount                      bool
	price                               string
}

// bills is the customer's orders keyed by their payment's transaction id.
func (w *world) bills(customerID string) map[string]billRow {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT p.transaction_id, o.origin, o.collection_owner, o.status, COALESCE(o.subscription_bill_kind, ''),
			COALESCE((SELECT b.source_kind FROM billing.provider_bills b WHERE b.merchant_id = o.merchant_id AND b.order_id = o.id LIMIT 1), ''),
			o.total, p.amount, l.unit_amount, o.period_starts_at, o.period_ends_at, p.purchased_at, l.provider_amount, pr.public_id
		FROM billing.payments p
		JOIN billing.orders o ON o.merchant_id = p.merchant_id AND o.id = p.order_id
		JOIN billing.order_lines l ON l.merchant_id = o.merchant_id AND l.order_id = o.id AND l.position = 0
		JOIN billing.prices pr ON pr.merchant_id = l.merchant_id AND pr.id = l.price_id
		WHERE p.customer_id = $1 AND p.refunded_payment_id IS NULL`), uuid.MustParse(customerID))
	require.NoError(w.t, err)
	defer rows.Close()
	out := map[string]billRow{}
	for rows.Next() {
		var id string
		var b billRow
		require.NoError(w.t, rows.Scan(&id, &b.origin, &b.owner, &b.status, &b.kind, &b.source, &b.total, &b.paid, &b.unit, &b.periodStart, &b.periodEnd, &b.purchasedAt, &b.providerAmount, &b.price))
		require.NotContains(w.t, out, id, "one payment per provider transaction")
		out[id] = b
	}
	require.NoError(w.t, rows.Err())
	return out
}

func (w *world) countRows(sql string, args ...any) (n int) {
	w.t.Helper()
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(sql), args...).Scan(&n))
	return n
}

// solanaSignature is a fresh transaction signature as Solana prints it.
func solanaSignature() string {
	var b [64]byte
	_, _ = rand.Read(b[:])
	return solanago.SignatureFromBytes(b[:]).String()
}

// A legacy wallet top-up imports as a complete engine-owned order with its
// frozen line, its succeeded rail payment and the access it bought. A
// re-import changes nothing; a total that does not add up, a sale with no
// fiat value and a reused identity are refused with findings.
func TestImportedPurchases(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := prepareWorld(t, 12)
			withSolana(t, w)
			w.start()
			ctx := t.Context()
			client := w.client[embedded]
			ent := "content:pass-" + uuid.NewString()[:6]
			product, err := client.CreateProduct(ctx, billing.CreateProductParams{Key: "pass-" + uuid.NewString()[:8], DisplayName: "Premium pass", Entitlements: []string{ent}})
			require.NoError(t, err)
			month := 720
			price, err := client.CreatePrice(ctx, billing.CreatePriceParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 23_000_000, Currency: "USD", AccessDurationHours: &month})
			require.NoError(t, err)

			now := w.clock.Now()
			buyer, earlier := w.newCustomer(), w.newCustomer()
			paid, legacyPaid := now.Add(-10*day), now.Add(-2*day)
			ends := paid.Add(30 * day)
			sig, legacySig := solanaSignature(), solanaSignature()
			solana := billing.PSPRef{Key: "solana"}
			line := func(unit int64, ends *time.Time) []billing.DeclaredOrderLine {
				return []billing.DeclaredOrderLine{{Price: price.ID, UnitAmount: unit, AccessEndsAt: ends}}
			}
			book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}, Purchases: []billing.DeclaredPurchase{
				{SourceID: "wallet-1", Customer: buyer.customerID(), Rail: "solana", PSP: solana, TransactionID: sig, Currency: "USD", Amount: 23_000_000, PaidAt: paid,
					Lines:      line(23_000_000, &ends),
					Settlement: &billing.DeclaredSettlement{Asset: "DUSD", Quantity: "23000000", Payer: "payer-wallet", Recipient: "merchant-wallet"},
					Evidence:   json.RawMessage(`{"legacy_wallet_tx_id":1}`)},
				// Sold at a legacy rate: the line keeps what was paid; access is the price's.
				{SourceID: "wallet-2", Customer: earlier.customerID(), Rail: "solana", PSP: solana, TransactionID: legacySig, Currency: "USD", Amount: 20_000_000, PaidAt: legacyPaid,
					Lines: line(20_000_000, nil)},
				{SourceID: "wallet-mismatch", Customer: buyer.customerID(), Rail: "solana", PSP: solana, TransactionID: solanaSignature(), Currency: "USD", Amount: 30_000_000, PaidAt: paid,
					Lines: line(23_000_000, nil)},
				{SourceID: "wallet-no-usd", Customer: buyer.customerID(), Rail: "solana", PSP: solana, TransactionID: solanaSignature(), Currency: "SOL", PaidAt: paid,
					Lines: line(0, nil)},
			}}

			res, err := w.client[tp].ImportBilling(ctx, book)
			require.NoError(t, err)
			require.Equal(t, []string{"wallet-1", "wallet-2"}, res.Imported, "%+v", res)
			require.Equal(t, []string{"wallet-mismatch", "wallet-no-usd"}, res.Blocked)
			require.Contains(t, res.Reasons["wallet-mismatch"], "differs from the line total")
			require.Contains(t, res.Reasons["wallet-no-usd"], "no fiat amount")
			refused := w.openFindings("life.import.purchase_refused")
			sort.Strings(refused)
			require.Equal(t, []string{"purchase:wallet-mismatch", "purchase:wallet-no-usd"}, refused)

			bought := w.bills(buyer.id)
			require.Len(t, bought, 1)
			b := bought[sig]
			require.Equal(t, billRow{origin: "merchant", owner: "engine", status: "complete", source: "import_purchase", total: 23_000_000, paid: 23_000_000, unit: 23_000_000,
				purchasedAt: b.purchasedAt, providerAmount: true, price: price.ID.String()}, b)
			require.True(t, paid.Equal(*b.purchasedAt))
			legacy := w.bills(earlier.id)[legacySig]
			require.Equal(t, int64(20_000_000), legacy.unit, "the line keeps the price it was sold at")
			require.Equal(t, int64(20_000_000), legacy.paid)

			var settlement, evidence string
			var list int64
			require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT metadata->'settlement'->>'quantity', metadata->'evidence'->>'legacy_wallet_tx_id', list_amount FROM billing.payments WHERE transaction_id = $1`), sig).Scan(&settlement, &evidence, &list))
			require.Equal(t, "23000000", settlement)
			require.Equal(t, "1", evidence)
			require.Equal(t, int64(23_000_000), list)

			// Access is the declared window, and the price's when none is declared.
			window := func(c *customer) (start, end time.Time) {
				require.NoError(t, w.pool.QueryRow(ctx, w.q(`SELECT starts_at, ends_at FROM billing.product_access WHERE customer_id = $1 AND source_type = 'purchase'`), uuid.MustParse(c.id)).Scan(&start, &end))
				return start, end
			}
			start, end := window(buyer)
			require.True(t, start.Equal(paid) && end.Equal(ends), "%s..%s", start, end)
			start, end = window(earlier)
			require.True(t, start.Equal(legacyPaid) && end.Equal(legacyPaid.Add(30*day)), "%s..%s", start, end)
			require.True(t, buyer.entitled(ent))
			require.False(t, buyer.entitledAt(ent, ends.Add(time.Hour)), "the pass ends when it ended")

			// A re-import is a no-op.
			again, err := w.client[tp].ImportBilling(ctx, book)
			require.NoError(t, err)
			require.Equal(t, []string{"wallet-1", "wallet-2"}, again.Skipped)
			require.Empty(t, again.Imported)
			require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.orders WHERE origin = 'merchant'`))
			require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.payments`))
			require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.grants WHERE source_type = 'purchase'`))

			// The same source with another transaction, or the same transaction
			// under another source, is refused.
			moved, reused := book.Purchases[0], book.Purchases[0]
			moved.TransactionID = solanaSignature()
			reused.SourceID = "wallet-copy"
			conflicts, err := w.client[tp].ImportBilling(ctx, billing.DeclaredBilling{AsOf: now, Purchases: []billing.DeclaredPurchase{moved, reused}})
			require.NoError(t, err)
			require.Equal(t, []string{"wallet-1", "wallet-copy"}, conflicts.Blocked, "%+v", conflicts)
			require.Equal(t, 2, w.countRows(`SELECT count(*) FROM billing.payments`))
		})
	}
}

// historyMember is a long-running legacy NMI member: a vault and a schedule
// NMI has billed for months before the import.
type historyMember struct {
	c        *customer
	vault    string
	schedule string
	paid     time.Time
	cycle    time.Time
	started  time.Time
	current  string
	sub      billing.SubscriptionID
}

func (w *world) historyMember(tier bookTier, paid time.Time, cycles int) *historyMember {
	m := &historyMember{c: w.newCustomer(), vault: w.nmi.AddVault(visa), paid: paid, cycle: paid.Add(-time.Duration(tier.days) * day)}
	m.started = m.cycle.Add(-time.Duration(cycles*tier.days) * day)
	m.schedule = w.nmi.AddSchedule(nmimock.Schedule{Vault: m.vault, Plan: tier.plan, Amount: tier.amount, Days: tier.days, NextBilling: paid})
	m.current = w.nmi.AddScheduleSale(m.schedule, m.cycle).TransactionID
	return m
}

// declare adds the member as doujins declares it: no accepted schedule (the
// book proves the current cycle), the current charge with NMI's next date,
// and charges.
func (m *historyMember) declare(book *billing.DeclaredBilling, tier bookTier, charges ...billing.DeclaredTransaction) {
	id := m.c.customerID()
	method := &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: m.vault, RailMethodRef: m.c.w.nmi.Vault(m.vault).BillingID}
	paid := m.paid
	book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: id})
	book.PaymentMethods = append(book.PaymentMethods, billing.DeclaredPaymentMethod{Customer: id, Rail: "nmi", RailCustomerRef: m.vault, RailMethodRef: method.RailMethodRef, Card: declaredCard(visa)})
	book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: "member-" + m.schedule, Customer: id, Price: tier.price.ID, Rail: "nmi",
		RailSubscriptionID: m.schedule, StartedAt: m.started, PaidThrough: &paid, PaymentMethod: method})
	book.Transactions = append(book.Transactions, billing.DeclaredTransaction{RailSubscriptionID: m.schedule, TransactionID: m.current, Success: true, Amount: tier.cents * 10_000,
		Currency: "USD", OccurredAt: m.cycle, NextBillingAt: &paid})
	for _, c := range charges {
		c.RailSubscriptionID = m.schedule
		if c.Currency == "" {
			c.Currency = "USD"
		}
		book.Transactions = append(book.Transactions, c)
	}
}

func (w *world) subscriptionOf(c *customer) billing.Subscription {
	w.t.Helper()
	subs, err := w.client[embedded].ListSubscriptions(w.t.Context(), billing.SubscriptionListParams{CustomerID: c.customerID()})
	require.NoError(w.t, err)
	require.Len(w.t, subs.Items, 1)
	return subs.Items[0]
}

// grossRevenue is the metrics' settled sales in [from, to).
func (w *world) grossRevenue(from, to time.Time, measure string) float64 {
	w.t.Helper()
	status, res := w.staffJSON(http.MethodPost, "/v1/metrics/query", map[string]any{"measures": []string{measure},
		"range": map[string]string{"from": from.UTC().Format(time.RFC3339), "to": to.UTC().Format(time.RFC3339)}})
	require.Equal(w.t, http.StatusOK, status, "%v", res)
	rows, _ := res["rows"].([]any)
	require.Len(w.t, rows, 1, "%v", res)
	columns, _ := res["columns"].([]any)
	for i, c := range columns {
		if c.(map[string]any)["name"] == measure {
			return decimal(w.t, rows[0].([]any)[i])
		}
	}
	w.t.Fatalf("no %s column: %v", measure, res)
	return 0
}

// Charges NMI made before the accepted schedule's first cycle, and every
// charge of a canceled member with no schedule, import as settled historical
// orders at NMI's exact amounts: no new access, no renewal, no schedule. A
// declared charge without an amount is read from NMI by its id, once; a pull
// binds an attributed id to its subscription and never records a charge
// twice. A synthetic id or an id NMI does not have records no payment.
func TestLegacyNMIHistorySettles(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := t.Context()
	monthly := w.bookTier("history", 999, 30)
	now := w.clock.Now()

	// A: a member since 110 days ago, on a $4.99 first month.
	a := w.historyMember(monthly, now.Add(10*day), 3)
	initial := w.nmi.AddSale(nmimock.Sale{Vault: a.vault, Amount: "4.99", OrderID: "legacy-signup", At: a.started}).TransactionID
	declined := w.nmi.AddSale(nmimock.Sale{Vault: a.vault, Amount: "9.99", ScheduleID: a.schedule, Declined: "202", At: now.Add(-80 * day)}).TransactionID
	retry := w.nmi.AddSale(nmimock.Sale{Vault: a.vault, Amount: "9.99", OrderID: "legacy-retry", At: now.Add(-78 * day)}).TransactionID
	declaredRebill := w.nmi.AddScheduleSale(a.schedule, now.Add(-63*day)).TransactionID
	pulledRebill := w.nmi.AddScheduleSale(a.schedule, now.Add(-50*day)).TransactionID
	declinedAsPaid := w.nmi.AddSale(nmimock.Sale{Vault: a.vault, Amount: "9.99", OrderID: "legacy-retry", Declined: "200", At: now.Add(-61 * day)}).TransactionID
	unknown := "8999999999"

	// C: canceled 10 days ago; NMI deleted the schedule. Its book has no
	// accepted schedule, so every charge is settled history.
	c := w.historyMember(monthly, now.Add(-5*day), 2)
	w.nmi.DeleteSchedule(c.schedule)
	cFirst := w.nmi.AddScheduleSale(c.schedule, c.started).TransactionID
	cSecond := w.nmi.AddScheduleSale(c.schedule, c.started.Add(30*day)).TransactionID

	book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
	a.declare(&book, monthly,
		billing.DeclaredTransaction{TransactionID: initial, Success: true, Amount: 4_990_000, OccurredAt: a.started},
		billing.DeclaredTransaction{TransactionID: declined, Type: "decline", OccurredAt: now.Add(-80 * day)},
		billing.DeclaredTransaction{TransactionID: retry, Success: true, OccurredAt: now.Add(-78 * day)},
		billing.DeclaredTransaction{TransactionID: declaredRebill, Success: true, Amount: 9_990_000, OccurredAt: now.Add(-63 * day)},
		billing.DeclaredTransaction{TransactionID: declinedAsPaid, Success: true, OccurredAt: now.Add(-61 * day)},
		billing.DeclaredTransaction{TransactionID: unknown, Success: true, OccurredAt: now.Add(-60 * day)},
		billing.DeclaredTransaction{TransactionID: "legacy-attempt:users_logs:7", Success: true, Amount: 9_990_000, OccurredAt: now.Add(-49 * day)},
		billing.DeclaredTransaction{TransactionID: "legacy-attempt:users_logs:8", Type: "decline", OccurredAt: now.Add(-48 * day)},
	)
	c.declare(&book, monthly,
		billing.DeclaredTransaction{TransactionID: cFirst, Success: true, Amount: 9_990_000, OccurredAt: c.started},
		billing.DeclaredTransaction{TransactionID: cSecond, Success: true, Amount: 9_990_000, OccurredAt: c.started.Add(30 * day)},
	)
	book.Subscriptions[1].Cancel = billing.CancelEvidence{Kind: "user_canceled", At: now.Add(-10 * day)}

	res, err := w.client[embedded].ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Len(t, res.Imported, 2, "%+v", res)
	w.settle()
	subA, subC := w.subscriptionOf(a.c), w.subscriptionOf(c.c)
	require.Equal(t, billing.SubscriptionActive, subA.Status)
	require.True(t, a.paid.Equal(*subA.CurrentPeriodEndsAt) && a.cycle.Equal(*subA.CurrentPeriodStartsAt), "history moves no period")
	require.Equal(t, billing.SubscriptionCanceled, subC.Status)

	price := monthly.price.ID.String()
	history := func(at time.Time, kind string, amount int64) billRow {
		end := at.Add(30 * day)
		return billRow{origin: "subscription", owner: "nmi_schedule", status: "complete", kind: kind, source: "provider_pre_schedule", total: amount, paid: amount, unit: amount,
			periodStart: &at, periodEnd: &end, purchasedAt: &at, providerAmount: true, price: price}
	}
	sameBill := func(want, got billRow) {
		t.Helper()
		require.True(t, want.periodStart.Equal(*got.periodStart) && want.periodEnd.Equal(*got.periodEnd) && want.purchasedAt.Equal(*got.purchasedAt), "%+v", got)
		want.periodStart, want.periodEnd, want.purchasedAt = got.periodStart, got.periodEnd, got.purchasedAt
		require.Equal(t, want, got)
	}
	billsA := w.bills(a.c.id)
	require.Len(t, billsA, 3, "the initial charge, the declared rebill and the current cycle: %v", billsA)
	sameBill(history(a.started, "initial", 4_990_000), billsA[initial])
	sameBill(history(now.Add(-63*day), "renewal", 9_990_000), billsA[declaredRebill])
	require.Equal(t, "provider_cycle", billsA[a.current].source, "the current charge binds the accepted cycle")
	billsC := w.bills(c.c.id)
	require.Len(t, billsC, 3, "a canceled member's charges are all history: %v", billsC)
	sameBill(history(c.started, "initial", 9_990_000), billsC[cFirst])
	sameBill(history(c.started.Add(30*day), "renewal", 9_990_000), billsC[cSecond])
	require.Equal(t, "provider_pre_schedule", billsC[c.current].source)

	require.Equal(t, []string{"transaction:legacy-attempt:users_logs:7"}, w.openFindings("life.import.transaction_unverified"))
	require.Empty(t, w.openFindings("pull.bill.unresolved"))
	require.Empty(t, w.openFindings("pull.charge.after_cancellation"))
	require.Equal(t, map[string][]string{"pending": sortedIDs(retry, declinedAsPaid, unknown)}, w.attributions())

	// History grants nothing and renews nothing.
	accessA := w.countRows(`SELECT count(*) FROM billing.grants WHERE customer_id = $1`, uuid.MustParse(a.c.id))
	accessC := w.countRows(`SELECT count(*) FROM billing.grants WHERE customer_id = $1`, uuid.MustParse(c.c.id))
	require.Zero(t, w.countRows(`SELECT count(*) FROM billing.grants WHERE source_type = 'purchase'`))
	require.Zero(t, w.countRows(`SELECT count(*) FROM billing.rebill_cycles`), "history is no rebill cycle")
	require.Zero(t, w.countRows(`SELECT count(*) FROM billing.provider_cycle_receipts WHERE transaction_id = ANY($1)`, []string{initial, declaredRebill, cFirst, cSecond, c.current}))
	require.Equal(t, 1, w.countRows(`SELECT count(*) FROM billing.provider_schedules`), "no schedule is inferred from history")
	// It is historical revenue at the time it was charged.
	require.Equal(t, 4_990_000.0, w.grossRevenue(a.started.Add(-time.Hour), a.started.Add(time.Hour), "gross_revenue"))
	require.Equal(t, 1.0, w.grossRevenue(now.Add(-64*day), now.Add(-62*day), "unique_rebilled_customers"))

	// A re-import is a no-op.
	again, err := w.client[embedded].ImportBilling(ctx, book)
	require.NoError(t, err)
	require.Len(t, again.Skipped, 2)
	require.Equal(t, billsA, w.bills(a.c.id))
	require.Equal(t, billsC, w.bills(c.c.id))
	require.Len(t, w.attributions()["pending"], 3)

	// The pass reads each pending id from NMI once.
	w.resolveAttributions()
	billsA = w.bills(a.c.id)
	require.Len(t, billsA, 4)
	sameBill(history(now.Add(-78*day), "renewal", 9_990_000), billsA[retry])
	require.Equal(t, map[string][]string{"resolved": {retry}, "unresolvable": sortedIDs(declinedAsPaid, unknown)}, w.attributions())
	unverified := w.openFindings("life.import.transaction_unverified")
	sort.Strings(unverified)
	require.Equal(t, sortedIDs("transaction:legacy-attempt:users_logs:7", "transaction:"+declinedAsPaid, "transaction:"+unknown), unverified)

	// B: a member imported later whose host retried a rebill by API: only its
	// declared attribution ties the sale to the schedule.
	b := w.historyMember(monthly, now.Add(12*day), 2)
	bRetry := w.nmi.AddSale(nmimock.Sale{Vault: b.vault, Amount: "9.99", OrderID: "legacy-retry", At: now.Add(-40 * day)}).TransactionID
	later := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}}
	b.declare(&later, monthly, billing.DeclaredTransaction{TransactionID: bRetry, Success: true, OccurredAt: now.Add(-40 * day)})
	_, err = w.client[embedded].ImportBilling(ctx, later)
	require.NoError(t, err)
	require.Contains(t, w.attributions()["pending"], bRetry)

	// The pull binds the attributed id and records A's undeclared rebill;
	// nothing it already holds is recorded again.
	require.NoError(t, w.cliPullAs(config.ProviderWriteModeFull, "nmi", true, false))
	w.settle()
	billsB := w.bills(b.c.id)
	sameBill(history(now.Add(-40*day), "renewal", 9_990_000), billsB[bRetry])
	require.NotContains(t, w.openFindings("pull.charge.missing"), bRetry)
	billsA = w.bills(a.c.id)
	require.Len(t, billsA, 5)
	sameBill(history(now.Add(-50*day), "renewal", 9_990_000), billsA[pulledRebill])
	require.Equal(t, sortedIDs(bRetry, retry), w.attributions()["resolved"])
	require.Len(t, w.bills(c.c.id), 3)

	w.resolveAttributions()
	require.Empty(t, w.attributions()["pending"])
	require.Equal(t, accessA, w.countRows(`SELECT count(*) FROM billing.grants WHERE customer_id = $1`, uuid.MustParse(a.c.id)), "history grants no access")
	require.Equal(t, accessC, w.countRows(`SELECT count(*) FROM billing.grants WHERE customer_id = $1`, uuid.MustParse(c.c.id)))
	require.Zero(t, len(w.nmi.Attempts()), "OpenRails charged nothing")
}

func sortedIDs(ids ...string) []string {
	sort.Strings(ids)
	return ids
}

// A large legacy book: every member's first charge predates its current
// cycle and one rebill is declared without an amount. Import records the
// history after each batch commits, and the attribution pass reads every
// rebill from NMI by id.
func TestLegacyHistoryAtScale(t *testing.T) {
	t.Parallel()
	const members, batch = 1000, 250
	w := newWorld(t)
	monthly := w.bookTier("scale", 999, 30)
	now := w.clock.Now()
	var books []billing.DeclaredBilling
	var rebills []string
	for i := 0; i < members; i++ {
		if i%batch == 0 {
			books = append(books, billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "nmi"}})
		}
		m := w.historyMember(monthly, now.Add(time.Duration(5+i%20)*day), 2)
		first := w.nmi.AddScheduleSale(m.schedule, m.started).TransactionID
		rebill := w.nmi.AddScheduleSale(m.schedule, m.started.Add(30*day)).TransactionID
		rebills = append(rebills, rebill)
		m.declare(&books[len(books)-1], monthly,
			billing.DeclaredTransaction{TransactionID: first, Success: true, Amount: 9_990_000, OccurredAt: m.started},
			billing.DeclaredTransaction{TransactionID: rebill, Success: true, OccurredAt: m.started.Add(30 * day)})
	}
	started := time.Now()
	for i, book := range books {
		res, err := w.client[embedded].ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Len(t, res.Imported, batch, "batch %d: %+v", i, res.Blocked)
	}
	imported := time.Since(started)
	require.Equal(t, members, w.countRows(`SELECT count(*) FROM billing.provider_bills WHERE source_kind = 'provider_pre_schedule'`))
	require.Equal(t, members, len(w.attributions()["pending"]))

	started = time.Now()
	w.resolveAttributions()
	resolved := time.Since(started)
	require.Equal(t, members, len(w.attributions()["resolved"]), "every rebill read once")
	require.Equal(t, 2*members, w.countRows(`SELECT count(*) FROM billing.provider_bills WHERE source_kind = 'provider_pre_schedule'`))
	require.Equal(t, 3*members, w.countRows(`SELECT count(*) FROM billing.payments`))
	require.Empty(t, w.openFindings("pull.bill.unresolved"))
	t.Logf("scale: %d members (%d historical charges) imported in %d batches in %s; %d attributions resolved in %s",
		members, members, len(books), imported.Round(time.Millisecond), members, resolved.Round(time.Millisecond))
}
