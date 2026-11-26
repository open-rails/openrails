//go:build integration

package critical_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

// A credit lot's remaining units are counted by the ledger: the legs on its
// balance account. Its view reads those counters and what its legs did.
func TestCreditLotCounters(t *testing.T) {
	t.Run("OR-J22-S10/lot-view-equals-ledger-counters", lotViewEqualsLedger)
	t.Run("OR-J22-S11/refunded-credit-pack-raises-no-refunded-grant-finding", refundedPackRaisesNoGrantFinding)
	t.Run("OR-J40-S04/unposted-deposit-is-not-spendable", unpostedDepositNotSpendable)
	t.Run("OR-J22-S13/lapsed-lots-expire-only-what-they-hold", lapsedLotsExpireWhatTheyHold)
	t.Run("OR-J26-S62/fractional-cent-invoice-paid-by-card-books-its-change-as-a-bought-lot", usageInvoiceChangeIsBought)
}

// lotMatchesLedger reads a lot and checks its counters against its legs on
// its balance account, and its view against the counters.
func lotMatchesLedger(t *testing.T, w *world, id billing.CreditGrantID) billing.CreditGrant {
	t.Helper()
	lot, err := w.alpha.GetCreditGrant(t.Context(), id)
	require.NoError(t, err)
	var units, credited, debited, loggedCredited, loggedDebited int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT g.units, g.units_credited, g.units_debited,"+
		" COALESCE(sum(t.amount) FILTER (WHERE t.credit_account_id = g.account_id), 0)::bigint,"+
		" COALESCE(sum(t.amount) FILTER (WHERE t.debit_account_id = g.account_id), 0)::bigint"+
		" FROM "+pgx.Identifier{w.schema, "credit_grants"}.Sanitize()+" g LEFT JOIN "+pgx.Identifier{w.schema, "ledger_transfers"}.Sanitize()+
		" t ON t.merchant_id = g.merchant_id AND t.credit_grant_id = g.id WHERE g.merchant_id = $1 AND g.id = $2"+
		" GROUP BY g.units, g.units_credited, g.units_debited", w.alpha.MerchantID().UUID(), id.UUID()).
		Scan(&units, &credited, &debited, &loggedCredited, &loggedDebited))
	require.Equal(t, loggedCredited, credited, "units_credited is the legs crediting the lot's balance")
	require.Equal(t, loggedDebited, debited, "units_debited is the legs debiting the lot's balance")
	require.Equal(t, units, lot.Amount)
	require.Equal(t, credited-debited, lot.RemainingAmount)
	require.LessOrEqual(t, debited, credited, "a lot never gives more than it holds")
	if credited == 0 {
		require.Equal(t, billing.CreditGrantPending, lot.State, "a lot none of whose units posted is pending")
		require.Zero(t, lot.SpentAmount+lot.ExpiredAmount+lot.RevokedAmount+lot.RefundedAmount+lot.RemainingAmount)
		return *lot
	}
	require.Equal(t, lot.Amount, lot.SpentAmount+lot.ExpiredAmount+lot.RevokedAmount+lot.RefundedAmount+lot.RemainingAmount,
		"what left the lot and what is left make up what it delivered")
	return *lot
}

func lotViewEqualsLedger(t *testing.T) {
	m := newMixedSale(t)
	a := m.a
	w := a.world
	payment := m.buy(t, "lot-counters")
	purchased, err := a.alpha.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: m.customer, Currency: "USD"})
	require.NoError(t, err)
	require.Len(t, purchased.Items, 1)
	pack := purchased.Items[0]
	require.Equal(t, billing.CreditFundingPurchase, pack.FundingSource)
	require.NotNil(t, pack.PaidAmount)
	require.EqualValues(t, 10_000_000, *pack.PaidAmount, "the pack's line paid $10")
	require.Equal(t, catalog.Currency("USD"), *pack.PaidCurrency)
	require.Equal(t, payment.String(), *pack.SourceID, "a bought lot names its payment")

	expiring, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: m.customer, Currency: "USD", Amount: 3_000_000,
		Source: "critical", SourceID: "expiring", ExpiresAt: ptr(a.clock.Now().Add(2 * time.Minute))}})
	require.NoError(t, err)
	revocable, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: m.customer, Currency: "USD", Amount: 4_000_000,
		Source: "critical", SourceID: "revocable", FundingSource: billing.CreditGrantFundingPromotion}})
	require.NoError(t, err)
	require.Equal(t, billing.CreditFundingPromotion, revocable[0].FundingSource)
	require.Nil(t, revocable[0].PaidAmount, "a gift records no payment")
	balance(t, a.alpha, m.customer, 32_000_000, 0, 32_000_000)
	for _, id := range []billing.CreditGrantID{pack.ID, expiring[0].ID, revocable[0].ID} {
		lot := lotMatchesLedger(t, w, id)
		require.Equal(t, billing.CreditGrantActive, lot.State)
		require.Equal(t, lot.Amount, lot.RemainingAmount)
	}

	// Spend: the soonest-expiring lot pays.
	compute, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "compute", DisplayName: "Compute"})
	require.NoError(t, err)
	_, err = a.alpha.SetMeter(t.Context(), "compute", billing.SetMeterParams{
		EventType: "compute", ValueProperty: "units", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: compute.ID, Price: catalog.RatePrice{Model: catalog.ModelDynamic, Currency: "USD"}}),
	})
	require.NoError(t, err)
	spend(t, a.alpha, m.customer, "spend", 2_000_000)
	// A deposit posts nothing: the usage stays pending.
	a.clock.Advance(time.Second)
	topUp, err := a.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: m.customer, Currency: "USD", Amount: 1_000_000, Source: "critical", SourceID: "top-up"}})
	require.NoError(t, err)
	balance(t, a.alpha, m.customer, 31_000_000, 2_000_000, 33_000_000)
	lot := lotMatchesLedger(t, w, expiring[0].ID)
	require.Zero(t, lot.SpentAmount)
	require.EqualValues(t, 3_000_000, lot.RemainingAmount)

	// Expiry: the usage before the lapse posts first and draws the lot; what
	// is left of it leaves it. The clock stays within the five minutes
	// payments may be dated ahead of the database's, for the chargeback below.
	a.clock.Advance(3 * time.Minute)
	require.False(t, runPass(t, quietFleet(t, w)), "the pass's look at the lapsed lot expires it")
	lot = lotMatchesLedger(t, w, expiring[0].ID)
	require.EqualValues(t, 2_000_000, lot.SpentAmount)
	require.EqualValues(t, 1_000_000, lot.ExpiredAmount)
	require.Zero(t, lot.RemainingAmount)
	require.Equal(t, billing.CreditGrantExpired, lot.State)
	balance(t, a.alpha, m.customer, 30_000_000, 0, 30_000_000)

	// Revocation.
	revoked, err := a.alpha.RevokeCreditGrant(t.Context(), "", revocable[0].ID, billing.RevokeCreditGrantParams{Reason: "Promotion withdrawn"})
	require.NoError(t, err)
	require.Equal(t, billing.CreditGrantRevoked, revoked.State)
	require.NotNil(t, revoked.RevokedAt)
	require.Equal(t, "Promotion withdrawn", *revoked.RevokeReason)
	lot = lotMatchesLedger(t, w, revocable[0].ID)
	require.EqualValues(t, 4_000_000, lot.RevokedAmount)
	require.Zero(t, lot.RefundedAmount)
	balance(t, a.alpha, m.customer, 26_000_000, 0, 26_000_000)

	// Refund: a chargeback of the invoice withdraws the pack, which reads
	// refunded, not spent and not revoked.
	charge, intent := m.charge(t, payment)
	dispute := stripemock.Object{"id": "dp_lot_counters", "object": "dispute", "amount": 10_000, "currency": "usd", "charge": charge, "payment_intent": intent, "status": "needs_response", "reason": "fraudulent"}
	m.notice(t, "evt_lot_counters_dispute", "charge.dispute.created", dispute)
	lot = lotMatchesLedger(t, w, pack.ID)
	require.EqualValues(t, 25_000_000, lot.RefundedAmount)
	require.Zero(t, lot.RevokedAmount)
	require.Zero(t, lot.SpentAmount)
	require.Equal(t, billing.CreditGrantRefunded, lot.State)
	balance(t, a.alpha, m.customer, 1_000_000, 0, 1_000_000)

	// Restore: the won dispute puts the pack back.
	dispute["status"] = "won"
	m.notice(t, "evt_lot_counters_dispute_won", "charge.dispute.closed", dispute)
	lot = lotMatchesLedger(t, w, pack.ID)
	require.Zero(t, lot.RefundedAmount)
	require.EqualValues(t, 25_000_000, lot.RemainingAmount)
	require.Equal(t, billing.CreditGrantActive, lot.State)
	balance(t, a.alpha, m.customer, 26_000_000, 0, 26_000_000)

	for _, id := range []billing.CreditGrantID{pack.ID, expiring[0].ID, revocable[0].ID, topUp[0].ID} {
		lotMatchesLedger(t, w, id)
	}
	require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String()), "Ledger integrity OK")

	// The audit names a lot whose counters were rewritten past their guard.
	tx, err := w.pool.Begin(t.Context())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(t.Context()) }()
	table := pgx.Identifier{w.schema, "credit_grants"}.Sanitize()
	_, err = tx.Exec(t.Context(), "ALTER TABLE "+table+" DISABLE TRIGGER guard_credit_grant_facts")
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "UPDATE "+table+" SET units_debited = units_debited + 1 WHERE merchant_id = $1 AND id = $2", a.alpha.MerchantID().UUID(), pack.ID.UUID())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), "ALTER TABLE "+table+" ENABLE TRIGGER guard_credit_grant_facts")
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
	output, err := hostCLIResult(t, w, "ledger-audit", "--merchant", "id:"+a.alpha.MerchantID().String())
	require.Error(t, err)
	require.Contains(t, output, "lot drift: credit_grant="+pack.ID.UUID().String())
}

// A refunded pack's lot is withdrawn, not a live grant whose payment was
// refunded: convergence raises no refunded-source finding for it.
func refundedPackRaisesNoGrantFinding(t *testing.T) {
	m := newMixedSale(t)
	a := m.a
	input := billing.CreateMyInvoiceParams{Lines: m.lines[:1], ExpectedTotal: ptr(int64(10_000_000)), Reusable: ptr(true),
		Payment: &billing.InvoicePaymentParams{Token: m.p.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242"})}}
	status, raw := a.request(t, http.MethodPost, "/v1/me/invoices", a.aliceToken, input, map[string]string{"Idempotency-Key": "pack-only"})
	require.Equal(t, http.StatusCreated, status, string(raw))
	var invoice billing.Invoice
	require.NoError(t, json.Unmarshal(raw, &invoice))
	require.Equal(t, billing.InvoiceComplete, invoice.Status)
	balance(t, a.alpha, m.customer, 25_000_000, 0, 25_000_000)
	refund, err := a.alpha.RefundPayment(t.Context(), "pack-return", *invoice.Payment.PaymentID, billing.RefundPaymentParams{Full: true, Reason: "Return the pack"})
	require.NoError(t, err)
	require.Equal(t, billing.PaymentSucceeded, refund.Status)
	balance(t, a.alpha, m.customer, 0, 0, 0)
	lots, err := a.alpha.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: m.customer, Currency: "USD"})
	require.NoError(t, err)
	require.Len(t, lots.Items, 1)
	require.Equal(t, billing.CreditGrantRefunded, lotMatchesLedger(t, a.world, lots.Items[0].ID).State)

	database, err := db.NewWithPGXPool(slog.Default(), a.pool, a.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
	customer := m.customer.UUID()
	_, err = converge.NewConvergeEngine(database, a.clock).Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID(), Customer: &customer})
	require.NoError(t, err)
	findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "derive.grant.excess"})
	require.NoError(t, err)
	require.Empty(t, findings.Items, "a refunded pack is no live grant of a refunded payment")
}

// A lot whose deposit never posted has no units: spending draws the lots
// that posted, and convergence posts the missing deposit once.
func unpostedDepositNotSpendable(t *testing.T) {
	w := newWorld(t)
	meterForCapture(t, w)
	customer := billing.CustomerID(uuid.New())
	posted, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "posted"}})
	require.NoError(t, err)
	// A lot recorded without its deposit, as an interrupted writer or an old
	// import left it; it lapses first, so spending would draw it first.
	unposted := uuid.New()
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{w.schema, "credit_grants"}.Sanitize()+
		" (merchant_id, id, customer_id, account_id, unit, units, funding_source, source, source_id, invoker, starts_at, expires_at)"+
		" SELECT merchant_id, $3, customer_id, account_id, unit, 5000, 'grant', 'critical', 'unposted', invoker, $4, $5"+
		" FROM "+pgx.Identifier{w.schema, "credit_grants"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2",
		w.alpha.MerchantID().UUID(), posted[0].ID.UUID(), unposted, w.clock.Now().Add(-time.Minute), w.clock.Now().AddDate(0, 2, 0))
	require.NoError(t, err)
	lot := lotMatchesLedger(t, w, billing.CreditGrantID(unposted))
	require.Zero(t, lot.RemainingAmount, "no unit of the lot posted")
	balance(t, w.alpha, customer, 1000, 0, 1000)

	// The whole posted balance is spent. Read from its face, the unposted lot
	// would pay it first (it lapses first) and leave the posted lot claiming
	// 1000 the balance no longer holds.
	spend(t, w.alpha, customer, "spend", 1000)
	fleet := startFleet(t, w)
	nextMonth(w)
	postDays(t, fleet)
	balance(t, w.alpha, customer, 0, 0, 0)
	lot = lotMatchesLedger(t, w, billing.CreditGrantID(unposted))
	require.Zero(t, lot.SpentAmount, "the spend never draws a lot with no units")
	paid := lotMatchesLedger(t, w, posted[0].ID)
	require.EqualValues(t, 1000, paid.SpentAmount, "the posted lot paid the spend")
	require.Zero(t, paid.RemainingAmount)
	require.Equal(t, billing.CreditGrantSpent, paid.State)

	database, err := db.NewWithPGXPool(slog.Default(), w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.alpha.MerchantID())
	scope := converge.Scope{Merchant: w.alpha.MerchantID(), Customer: ptr(customer.UUID())}
	for range 2 {
		_, err = converge.NewConvergeEngine(database, w.clock).Converge(ctx, scope)
		require.NoError(t, err)
	}
	lot = lotMatchesLedger(t, w, billing.CreditGrantID(unposted))
	require.EqualValues(t, 5000, lot.RemainingAmount, "convergence posts the missing deposit, once")
	require.Equal(t, billing.CreditGrantActive, lot.State)
	balance(t, w.alpha, customer, 5000, 0, 5000)
}

// Two lots lapse before their expiry runs. Usage settled at the first lapse
// is drawn from the lots live just before it: all of the first, then the
// second. The second then expires only what is left of it; read stale, it
// would expire more than it holds.
func lapsedLotsExpireWhatTheyHold(t *testing.T) {
	w := newWorld(t)
	meterForCapture(t, w)
	customer := billing.CustomerID(uuid.New())
	grant := func(id string, amount int64, expires *time.Time) billing.CreditGrantID {
		lots, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: amount, Source: "critical", SourceID: id, ExpiresAt: expires}})
		require.NoError(t, err)
		return lots[0].ID
	}
	first := grant("first", 1000, ptr(w.clock.Now().Add(time.Hour)))
	second := grant("second", 1000, ptr(w.clock.Now().Add(2*time.Hour)))
	lasting := grant("lasting", 5000, nil)
	spend(t, w.alpha, customer, "used", 1500)
	balance(t, w.alpha, customer, 5500, 1500, 7000)

	// Both lapse with the usage unposted; the pass's look expires them.
	w.clock.Advance(3 * time.Hour)
	require.False(t, runPass(t, quietFleet(t, w)))

	early := lotMatchesLedger(t, w, first)
	require.EqualValues(t, 1000, early.SpentAmount, "the usage drew the first lot, live just before its lapse")
	require.Zero(t, early.ExpiredAmount, "nothing was left of it to expire")
	late := lotMatchesLedger(t, w, second)
	require.EqualValues(t, 500, late.SpentAmount, "then the second")
	require.EqualValues(t, 500, late.ExpiredAmount, "which expires only what is left of it")
	rest := lotMatchesLedger(t, w, lasting)
	require.Zero(t, rest.SpentAmount)
	require.EqualValues(t, 5000, rest.RemainingAmount)
	balance(t, w.alpha, customer, 5000, 0, 5000)
	require.Contains(t, hostCLI(t, w, "ledger-audit", "--merchant", "id:"+w.alpha.MerchantID().String()), "Ledger integrity OK")
}

// A usage invoice whose total is not a whole cent is charged the next cent up.
// The invoice is paid, and the change is a lot bought at face.
func usageInvoiceChangeIsBought(t *testing.T) {
	a, p, customer := usageBillingWorld(t, false)
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "fractional-compute", DisplayName: "Fractional compute"})
	require.NoError(t, err)
	_, err = a.alpha.SetMeter(t.Context(), "fraction", billing.SetMeterParams{EventType: "fraction", ValueProperty: "units", Aggregation: catalog.AggregationSum,
		RateCard: catalog.Value(billing.MeterRateCardParams{ProductID: product.ID, Price: catalog.RatePrice{Model: catalog.ModelPerUnit, Currency: "USD",
			PerUnit: &catalog.PerUnitPrice{UnitAmount: 1_234_567, DivideBy: 1}}})})
	require.NoError(t, err)
	input := usageInput(customer, "fractional-work", 3)
	input.EventType = "fraction"
	oneUsage(t, a.alpha, input)
	fleet := startFleet(t, a.world)
	runUsageDue(t, fleet)
	a.clock.Advance(12 * time.Hour)
	runUsageDue(t, fleet)
	require.Eventually(t, func() bool {
		rows, err := a.alpha.ListInvoices(t.Context(), billing.InvoiceListParams{CustomerID: customer})
		return err == nil && len(rows.Items) == 1 && rows.Items[0].Status == billing.InvoiceComplete
	}, 15*time.Second, 20*time.Millisecond)
	invoice := oneUsageInvoice(t, a, customer)
	require.EqualValues(t, 3_703_701, invoice.Total)
	require.Zero(t, invoice.Usage.AmountDue)
	p.money(t, 1, 371)
	lots, err := a.alpha.ListCreditGrants(t.Context(), billing.CreditGrantListParams{CustomerID: customer, Currency: "USD"})
	require.NoError(t, err)
	require.Len(t, lots.Items, 1)
	change := lots.Items[0]
	require.Equal(t, billing.CreditFundingPurchase, change.FundingSource)
	require.EqualValues(t, 6_299, change.Amount, "$3.71 less the invoice's $3.703701")
	require.NotNil(t, change.PaidAmount)
	require.EqualValues(t, 6_299, *change.PaidAmount, "bought at face")
	lotMatchesLedger(t, a.world, change.ID)
	balance(t, a.alpha, customer, 6_299, 0, 6_299)
	// Recorded before its payment, the change's price is deferred from that
	// payment once it is: the commit's check passed.
	require.Equal(t, []int64{6_299}, lotLegs(t, a.world, change.ID, "cash_applied"))
	deferred, released := lotMoney(t, a.world, change.ID)
	require.EqualValues(t, 6_299, deferred)
	require.Zero(t, released)
	// The invoice's share of the $3.71 settles its usage receivable.
	require.Equal(t, []int64{3_703_701}, collected(t, a.world, "usage_collection"))
	require.Zero(t, glBalance(t, a.world, "usage_receivable"))
	require.Zero(t, glBalance(t, a.world, "unapplied_cash"))
	require.EqualValues(t, 3_703_701, glBalance(t, a.world, "usage_revenue"))
	// The change applies its payment's cash, as a bill's line would: the
	// per-payment audit counts it.
	var payment, issued string
	require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT p.id::text, t.source || ':' || t.source_id FROM "+booksTable(a.world, "payments")+" p, "+booksTable(a.world, "ledger_transfers")+" t"+
		" WHERE p.merchant_id = $1 AND p.invoice_id = $2 AND t.merchant_id = $1 AND t.credit_grant_id = $3 AND t.operation = 'credit_issue'",
		a.alpha.MerchantID().UUID(), invoice.ID.UUID(), change.ID.UUID()).Scan(&payment, &issued))
	require.Equal(t, "payment:"+payment, issued)
	booksAudited(t, a.world)
}
