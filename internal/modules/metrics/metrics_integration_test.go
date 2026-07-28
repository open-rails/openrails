//go:build integration

package metrics_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/metrics"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Fixture truth (all hand-computed; June 2026, usd, micros):
//
// payments (merchant A): sale0 5/10 7M one-time; sale1 6/02 10M sub initial;
// sale2 6/10 10M sub renewal; sale3 6/11 5M one-time initial (PROMO);
// fail1 6/10 renewal code 202; fail2 6/12 initial code 223 (both c2, 10M);
// refund1 6/20 -5M (refund of sale3); cb1 6/25 -10M (chargeback of sale2).
//   June: gross 25M, refunds 5M, chargebacks 10M, net 10M; counts 3/2/1/1;
//   approval 3/5=0.6; cb_rate 1/3; unique_failed 1; unique_rebilled 1;
//   realized 10M/2 payers = 5M. May: net 7M (compare target).
//
// subscriptions (product P1): sub1 c1 monthly 5/15- active; sub2 c2 monthly
// 6/05-6/20 cancelled(user); sub3 c3 annual 4/01- active; sub4 c4 monthly
// 3/01- past_due; sub5 c5 monthly 3/15- past_due->active (recovery);
// sub6 c6 monthly 3/20- past_due->cancelled(failed_payment) 6/22;
// sub7 c8 weekly 6/10- active; sub_old c8 2025 ended (makes c8 returning).
//   subscriptions@Apr1=4, May1=4, Jun1=5; mrr@Jun1=48M (10+8+10+10+10);
//   monthly 720h->10M, annual 8760h->96M/12=8M, weekly 168h->round(2.3M*730/168).
//   June: new=2 (first_time sub2, returning sub7); cancellations=2;
//   churn=2/5=0.4; avg duration (15d+94d)/2=54.5; recovery_rate=1/2.
//
// credits/usage: lots c1 20M 6/03 + 10M 6/15, c2 30M 5/20; usage c1 5M 6/05 +
// 3M 6/18, c2 2M 6/10 + 14M 6/28; owed accrual 4M 6/12, payment 1M 6/20.
//   June: credits_sold 30M; repeat_topup 1/2; usage_revenue 24M; units 4;
//   payers 2; utilization 0.8; liability@7/1 = 60-24 = 36M; owed@7/1 = 3M;
//   depletion@7/1: c2 balance 14M vs 7d burn 14M -> 1 payer at risk.
//
// merchant B: one 999M payment 6/05 — must never appear under A.

var (
	mA = uuid.New()
	mB = uuid.New()

	c = func() map[int]uuid.UUID {
		out := map[int]uuid.UUID{}
		for i := 1; i <= 8; i++ {
			out[i] = uuid.New()
		}
		return out
	}()
	cB = uuid.New()

	productA = uuid.New()
	productB = uuid.New()
	pricePM  = uuid.New() // monthly 10M / 720h
	pricePA  = uuid.New() // annual 96M / 8760h
	pricePO  = uuid.New() // one-time 5M
	pricePW  = uuid.New() // weekly 2.3M / 168h
	priceB   = uuid.New()
	acctRA1  = uuid.New()

	subs = map[int]uuid.UUID{1: uuid.New(), 2: uuid.New(), 3: uuid.New(), 4: uuid.New(), 5: uuid.New(), 6: uuid.New(), 7: uuid.New(), 8: uuid.New()}
)

func d(t *testing.T, y int, m time.Month, day int) time.Time {
	t.Helper()
	return time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
}

func exec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	_, err := pool.Exec(ctx, sql, args...)
	require.NoError(t, err, sql)
}

func seed(t *testing.T) (*pgxpool.Pool, *metrics.Service, context.Context, context.Context) {
	t.Helper()
	ctx := context.Background()
	dbi := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	pool := dbi.Pool()
	svc := metrics.NewService(dbi)
	ctxA := merchant.WithID(ctx, merchant.ID(mA))
	ctxB := merchant.WithID(ctx, merchant.ID(mB))

	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	exec(ctx, t, pool, `INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active'), ($3, $4, 'active') ON CONFLICT (id) DO NOTHING`,
		mA, "metrics-a-"+suffix, mB, "metrics-b-"+suffix)
	for _, cid := range c {
		exec(ctx, t, pool, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, cid, mA)
	}
	exec(ctx, t, pool, `INSERT INTO openrails.customers (id, merchant_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, cB, mB)
	exec(ctx, t, pool, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Metrics P1', $3) ON CONFLICT DO NOTHING`,
		productA, "metrics-p1-"+suffix, mA)
	exec(ctx, t, pool, `INSERT INTO openrails.products (id, key, display_name, merchant_id) VALUES ($1, $2, 'Metrics PB', $3) ON CONFLICT DO NOTHING`,
		productB, "metrics-pb-"+suffix, mB)
	for _, p := range []struct {
		id       uuid.UUID
		amount   int64
		hours    *int
		renew    bool
		product  uuid.UUID
		merchant uuid.UUID
	}{
		{pricePM, 10_000_000, intp(720), true, productA, mA},
		{pricePA, 96_000_000, intp(8760), true, productA, mA},
		{pricePO, 5_000_000, nil, false, productA, mA},
		{pricePW, 2_300_000, intp(168), true, productA, mA},
		{priceB, 999_000_000, nil, false, productB, mB},
	} {
		exec(ctx, t, pool, `INSERT INTO openrails.prices (id, product_id, amount, currency, merchant_id, access_duration_hours, auto_renew)
			VALUES ($1, $2, $3, 'USD', $4, $5, $6) ON CONFLICT DO NOTHING`,
			p.id, p.product, p.amount, p.merchant, p.hours, p.renew)
	}
	exec(ctx, t, pool, `INSERT INTO openrails.psps (id, merchant_id, rail, account_id) VALUES ($1, $2, 'nmi', 'acct-1') ON CONFLICT DO NOTHING`,
		acctRA1, mA)

	// --- subscriptions (insert order matters only for readability) -------------
	type subRow struct {
		id               uuid.UUID
		customer         uuid.UUID
		price            uuid.UUID
		status           string
		started          time.Time
		ended, cancelled *time.Time
		cancelType       *string
		periodEnd        *time.Time
	}
	tp := func(tt time.Time) *time.Time { return &tt }
	sp := func(s string) *string { return &s }
	for _, s := range []subRow{
		{id: subs[1], customer: c[1], price: pricePM, status: "active", started: d(t, 2026, 5, 15)},
		{id: subs[2], customer: c[2], price: pricePM, status: "cancelled", started: d(t, 2026, 6, 5), ended: tp(d(t, 2026, 6, 20)), cancelled: tp(d(t, 2026, 6, 20)), cancelType: sp("user")},
		{id: subs[3], customer: c[3], price: pricePA, status: "active", started: d(t, 2026, 4, 1)},
		{id: subs[4], customer: c[4], price: pricePM, status: "past_due", started: d(t, 2026, 3, 1), periodEnd: tp(d(t, 2026, 6, 25))},
		{id: subs[5], customer: c[5], price: pricePM, status: "past_due", started: d(t, 2026, 3, 15), periodEnd: tp(d(t, 2026, 6, 15))},
		{id: subs[6], customer: c[6], price: pricePM, status: "past_due", started: d(t, 2026, 3, 20), periodEnd: tp(d(t, 2026, 6, 18))},
		{id: subs[7], customer: c[8], price: pricePW, status: "active", started: d(t, 2026, 6, 10)},
		{id: subs[8], customer: c[8], price: pricePM, status: "cancelled", started: d(t, 2025, 1, 1), ended: tp(d(t, 2025, 6, 1)), cancelled: tp(d(t, 2025, 5, 15)), cancelType: sp("user")},
	} {
		exec(ctx, t, pool, `INSERT INTO openrails.subscriptions
			(id, merchant_id, customer_id, product_id, price_id, rail, status, started_at, ended_at, cancelled_at, cancel_type, current_period_ends_at)
			VALUES ($1, $2, $3, $4, $5, 'nmi', $6::openrails.subscription_status, $7, $8, $9, $10, $11)`,
			s.id, mA, s.customer, productA, s.price, s.status, s.started, s.ended, s.cancelled, s.cancelType, s.periodEnd)
	}
	// Recovery transitions (the trigger records them in the same tx):
	// sub5 past_due->active; sub6 past_due->cancelled.
	exec(ctx, t, pool, `UPDATE openrails.subscriptions SET status = 'active', current_period_ends_at = $2 WHERE id = $1`,
		subs[5], d(t, 2026, 7, 15))
	exec(ctx, t, pool, `UPDATE openrails.subscriptions SET status = 'cancelled', cancelled_at = $2, ended_at = $2, cancel_type = 'failed_payment', next_retry_at = NULL, grace_ends_at = NULL WHERE id = $1`,
		subs[6], d(t, 2026, 6, 22))

	// --- payments ---------------------------------------------------------------
	sale0, sale1, sale2, sale3 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fail1, fail2, refund1, cb1, saleB := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	payCols := `INSERT INTO openrails.payments
		(id, merchant_id, customer_id, price_id, subscription_id, refunded_payment_id, rail, psp_id,
		 transaction_id, amount, list_amount, currency, status, attempt_kind, failure_code, failure_reason, reversal_kind,
		 card_brand, discount_code, purchased_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'USD',$12::openrails.payment_status,$13,$14,$15,$16,$17,$18,$19,$19)`
	txp := "mtx-" + suffix + "-"
	exec(ctx, t, pool, payCols, sale0, mA, c[1], pricePO, nil, nil, "nmi", nil, txp+"s0", 7_000_000, 7_000_000, "completed", "initial", nil, nil, nil, "visa", nil, d(t, 2026, 5, 10))
	exec(ctx, t, pool, payCols, sale1, mA, c[1], pricePM, subs[1], nil, "nmi", acctRA1, txp+"s1", 10_000_000, 10_000_000, "completed", "initial", nil, nil, nil, "visa", nil, d(t, 2026, 6, 2))
	exec(ctx, t, pool, payCols, sale2, mA, c[1], pricePM, subs[1], nil, "nmi", acctRA1, txp+"s2", 10_000_000, 10_000_000, "completed", "renewal", nil, nil, nil, "visa", nil, d(t, 2026, 6, 10))
	exec(ctx, t, pool, payCols, sale3, mA, c[2], pricePO, nil, nil, "ccbill", nil, txp+"s3", 5_000_000, 5_000_000, "completed", "initial", nil, nil, nil, nil, "PROMO", d(t, 2026, 6, 11))
	exec(ctx, t, pool, payCols, fail1, mA, c[2], pricePM, nil, nil, "nmi", nil, txp+"f1", 10_000_000, 10_000_000, "failed", "renewal", "202", "insufficient_funds", nil, nil, nil, d(t, 2026, 6, 10))
	exec(ctx, t, pool, payCols, fail2, mA, c[2], pricePM, nil, nil, "nmi", nil, txp+"f2", 10_000_000, 10_000_000, "failed", "initial", "223", "expired_card", nil, nil, nil, d(t, 2026, 6, 12))
	exec(ctx, t, pool, payCols, refund1, mA, c[2], pricePO, nil, sale3, "ccbill", nil, txp+"r1", -5_000_000, 5_000_000, "completed", nil, nil, nil, "refund", nil, nil, d(t, 2026, 6, 20))
	exec(ctx, t, pool, payCols, cb1, mA, c[1], pricePM, subs[1], sale2, "nmi", acctRA1, txp+"c1", -10_000_000, 10_000_000, "completed", nil, nil, nil, "chargeback", nil, nil, d(t, 2026, 6, 25))
	exec(ctx, t, pool, payCols, saleB, mB, cB, priceB, nil, nil, "nmi", nil, txp+"sB", 999_000_000, 999_000_000, "completed", "initial", nil, nil, nil, nil, nil, d(t, 2026, 6, 5))

	// --- credit lots (grants) + usage + ledger ------------------------------------
	lot := `INSERT INTO openrails.grants (id, merchant_id, customer_id, kind, source_type, source_id, event, amount, currency, created_at, starts_at)
		VALUES ($1, $2, $3, 'credit', 'purchase', $4, 'grant', $5, 'USD', $6, $6)`
	exec(ctx, t, pool, lot, uuid.New(), mA, c[1], txp+"lot1", 20_000_000, d(t, 2026, 6, 3))
	exec(ctx, t, pool, lot, uuid.New(), mA, c[1], txp+"lot2", 10_000_000, d(t, 2026, 6, 15))
	exec(ctx, t, pool, lot, uuid.New(), mA, c[2], txp+"lot3", 30_000_000, d(t, 2026, 5, 20))

	ue := `INSERT INTO openrails.usage_events (id, merchant_id, customer_id, invoker_id, currency, resource, event_type, amount, source, source_id, occurred_at)
		VALUES ($1, $2, $3, 'user:test', 'USD', $4, $5, $6, 'metrics-test', $7, $8)`
	exec(ctx, t, pool, ue, uuid.New(), mA, c[1], "api", "gpt", 5_000_000, txp+"u1", d(t, 2026, 6, 5))
	exec(ctx, t, pool, ue, uuid.New(), mA, c[1], "api", "gpt", 3_000_000, txp+"u2", d(t, 2026, 6, 18))
	exec(ctx, t, pool, ue, uuid.New(), mA, c[2], "img", "flux", 2_000_000, txp+"u3", d(t, 2026, 6, 10))
	exec(ctx, t, pool, ue, uuid.New(), mA, c[2], "img", "flux", 14_000_000, txp+"u4", d(t, 2026, 6, 28))

	clearing, cb1acct, cb2acct, rev, arrears := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	acct := `INSERT INTO openrails.ledger_accounts (id, merchant_id, customer_id, account_type, currency, debits_must_not_exceed_credits)
		VALUES ($1, $2, $3, $4, 'USD', $5)`
	exec(ctx, t, pool, acct, clearing, mA, nil, "processor_clearing", false)
	exec(ctx, t, pool, acct, cb1acct, mA, c[1], "customer_balance", true)
	exec(ctx, t, pool, acct, cb2acct, mA, c[2], "customer_balance", true)
	exec(ctx, t, pool, acct, rev, mA, nil, "platform_revenue", false)
	exec(ctx, t, pool, acct, arrears, mA, nil, "arrears_liability", false)
	tr := `INSERT INTO openrails.ledger_transfers (id, merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, customer_id, created_at)
		VALUES ($1, $2, $3, $4, $5, 'USD', $6, $7, $8)`
	// deposits first so the customer-balance floor never trips.
	exec(ctx, t, pool, tr, uuid.New(), mA, clearing, cb1acct, 20_000_000, "deposit", c[1], d(t, 2026, 6, 3))
	exec(ctx, t, pool, tr, uuid.New(), mA, clearing, cb1acct, 10_000_000, "deposit", c[1], d(t, 2026, 6, 15))
	exec(ctx, t, pool, tr, uuid.New(), mA, clearing, cb2acct, 30_000_000, "deposit", c[2], d(t, 2026, 5, 20))
	exec(ctx, t, pool, tr, uuid.New(), mA, cb1acct, rev, 5_000_000, "credit_spend", c[1], d(t, 2026, 6, 5))
	exec(ctx, t, pool, tr, uuid.New(), mA, cb1acct, rev, 3_000_000, "credit_spend", c[1], d(t, 2026, 6, 18))
	exec(ctx, t, pool, tr, uuid.New(), mA, cb2acct, rev, 2_000_000, "credit_spend", c[2], d(t, 2026, 6, 10))
	exec(ctx, t, pool, tr, uuid.New(), mA, cb2acct, rev, 14_000_000, "credit_spend", c[2], d(t, 2026, 6, 28))
	exec(ctx, t, pool, tr, uuid.New(), mA, arrears, rev, 4_000_000, "owed_accrual", c[3], d(t, 2026, 6, 12))
	exec(ctx, t, pool, tr, uuid.New(), mA, clearing, arrears, 1_000_000, "owed_payment", c[3], d(t, 2026, 6, 20))

	// --- entitlements ----------------------------------------------------------------
	ent := `INSERT INTO openrails.entitlements (id, merchant_id, customer_id, entitlement, start_at, end_at, source_id, source_type)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'admin')`
	exec(ctx, t, pool, ent, uuid.New(), mA, c[1], "premium", d(t, 2026, 5, 1), nil, uuid.New())
	e2end := d(t, 2026, 6, 15)
	exec(ctx, t, pool, ent, uuid.New(), mA, c[2], "premium", d(t, 2026, 6, 1), e2end, uuid.New())
	exec(ctx, t, pool, ent, uuid.New(), mA, c[3], "gold", d(t, 2026, 6, 1), nil, uuid.New())

	// --- admission denial aggregates -----------------------------------------------
	adh := `INSERT INTO openrails.admission_denials_hourly (merchant_id, customer_id, denial_reason, hour_at, denials)
		VALUES ($1, $2, $3, $4, $5)`
	exec(ctx, t, pool, adh, mA, c[1], "insufficient_credit", time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC), 5)
	exec(ctx, t, pool, adh, mA, c[1], "budget_exceeded", time.Date(2026, 6, 10, 10, 0, 0, 0, time.UTC), 2)
	exec(ctx, t, pool, adh, mA, c[2], "insufficient_credit", time.Date(2026, 6, 11, 8, 0, 0, 0, time.UTC), 3)

	t.Cleanup(func() {
		for _, sql := range []string{
			`DELETE FROM openrails.admission_denials_hourly WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.entitlements WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.ledger_transfers WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.ledger_accounts WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.usage_events WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.grants WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.payments WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.subscription_status_transitions WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.subscriptions WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.psps WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.prices WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.products WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.customers WHERE merchant_id = ANY($1)`,
			`DELETE FROM openrails.merchants WHERE id = ANY($1)`,
		} {
			_, _ = pool.Exec(context.Background(), sql, []uuid.UUID{mA, mB})
		}
	})
	return pool, svc, ctxA, ctxB
}

func intp(v int) *int { return &v }

func run(t *testing.T, svc *metrics.Service, ctx context.Context, q *metrics.Query) *metrics.Result {
	t.Helper()
	plan, ve := metrics.Validate(q)
	require.Nil(t, ve, "validate: %v", ve)
	res, err := svc.Execute(ctx, plan)
	require.NoError(t, err)
	return res
}

func colIdx(t *testing.T, res *metrics.Result, name string) int {
	t.Helper()
	for i, col := range res.Columns {
		if col.Name == name {
			return i
		}
	}
	t.Fatalf("column %q not in %v", name, res.Columns)
	return -1
}

// cell finds the single row matching the given dim/time string values and
// returns the named measure cell.
func cell(t *testing.T, res *metrics.Result, match map[string]string, measure string) any {
	t.Helper()
	rows := matchRows(t, res, res.Rows, match)
	require.Len(t, rows, 1, "want exactly one row matching %v, got %d", match, len(rows))
	return rows[0][colIdx(t, res, measure)]
}

func matchRows(t *testing.T, res *metrics.Result, rows [][]any, match map[string]string) [][]any {
	t.Helper()
	var out [][]any
	for _, row := range rows {
		ok := true
		for k, v := range match {
			if fmt.Sprint(row[colIdx(t, res, k)]) != v {
				ok = false
			}
		}
		if ok {
			out = append(out, row)
		}
	}
	return out
}

var juneQ = &metrics.QueryRange{From: "2026-06-01", To: "2026-06-30"}

func usd(q *metrics.Query) *metrics.Query {
	if q.Filters == nil {
		q.Filters = map[string][]string{}
	}
	q.Filters["currency"] = []string{"usd"}
	return q
}

// --- payments family --------------------------------------------------------------

func TestMetrics_PaymentsFlow(t *testing.T) {
	_, svc, ctxA, _ := seed(t)

	res := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"gross_revenue", "net_revenue", "refunds", "chargebacks", "payment_count",
			"payment_failures", "refund_count", "chargeback_count", "unique_failed_customers",
			"unique_rebilled_customers", "approval_rate", "chargeback_rate", "realized_revenue_per_customer"},
		Range: juneQ,
	}))
	require.Len(t, res.Rows, 1)
	m := map[string]string{}
	require.Equal(t, int64(25_000_000), cell(t, res, m, "gross_revenue"))
	require.Equal(t, int64(10_000_000), cell(t, res, m, "net_revenue"))
	require.Equal(t, int64(5_000_000), cell(t, res, m, "refunds"))
	require.Equal(t, int64(10_000_000), cell(t, res, m, "chargebacks"))
	require.Equal(t, int64(3), cell(t, res, m, "payment_count"))
	require.Equal(t, int64(2), cell(t, res, m, "payment_failures"))
	require.Equal(t, int64(1), cell(t, res, m, "refund_count"))
	require.Equal(t, int64(1), cell(t, res, m, "chargeback_count"))
	require.Equal(t, int64(1), cell(t, res, m, "unique_failed_customers"))
	require.Equal(t, int64(1), cell(t, res, m, "unique_rebilled_customers"))
	// Ratios divide AFTER aggregation: 3/5, not the mean of daily ratios (0.625).
	require.InDelta(t, 0.6, cell(t, res, m, "approval_rate").(float64), 1e-9)
	require.InDelta(t, 1.0/3.0, cell(t, res, m, "chargeback_rate").(float64), 1e-9)
	require.InDelta(t, 5_000_000, cell(t, res, m, "realized_revenue_per_customer").(float64), 1e-6)
}

func TestMetrics_PaymentsDimensions(t *testing.T) {
	_, svc, ctxA, _ := seed(t)

	byRail := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"gross_revenue", "net_revenue"}, By: []string{"rail"}, Range: juneQ}))
	require.Equal(t, int64(20_000_000), cell(t, byRail, map[string]string{"rail": "nmi"}, "gross_revenue"))
	require.Equal(t, int64(10_000_000), cell(t, byRail, map[string]string{"rail": "nmi"}, "net_revenue"))
	require.Equal(t, int64(5_000_000), cell(t, byRail, map[string]string{"rail": "ccbill"}, "gross_revenue"))
	require.Equal(t, int64(0), cell(t, byRail, map[string]string{"rail": "ccbill"}, "net_revenue"))

	byStream := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"gross_revenue"}, By: []string{"stream"}, Range: juneQ}))
	require.Equal(t, int64(20_000_000), cell(t, byStream, map[string]string{"stream": "subscription"}, "gross_revenue"))
	require.Equal(t, int64(5_000_000), cell(t, byStream, map[string]string{"stream": "one_time"}, "gross_revenue"))

	byKind := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"payment_count"}, By: []string{"attempt_kind"}, Range: juneQ}))
	require.Equal(t, int64(2), cell(t, byKind, map[string]string{"attempt_kind": "initial"}, "payment_count"))
	require.Equal(t, int64(1), cell(t, byKind, map[string]string{"attempt_kind": "renewal"}, "payment_count"))

	byReason := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"payment_failures"}, By: []string{"failure_reason"}, Range: juneQ}))
	require.Equal(t, int64(1), cell(t, byReason, map[string]string{"failure_reason": "insufficient_funds"}, "payment_failures"))
	require.Equal(t, int64(1), cell(t, byReason, map[string]string{"failure_reason": "expired_card"}, "payment_failures"))

	// The doujins golden tile: unique failed INITIAL customers by reason.
	golden := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"unique_failed_customers"},
		By:       []string{"failure_reason"},
		Range:    juneQ,
		Filters:  map[string][]string{"attempt_kind": {"initial"}},
	}))
	require.Equal(t, int64(1), cell(t, golden, map[string]string{"failure_reason": "expired_card"}, "unique_failed_customers"))
	require.Len(t, golden.Rows, 1)

	byAcct := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"payment_count", "chargeback_rate"}, By: []string{"rail_account"}, Range: juneQ}))
	require.Equal(t, int64(2), cell(t, byAcct, map[string]string{"rail_account": "acct-1"}, "payment_count"))
	require.InDelta(t, 0.5, cell(t, byAcct, map[string]string{"rail_account": "acct-1"}, "chargeback_rate").(float64), 1e-9)
	require.Equal(t, int64(1), cell(t, byAcct, map[string]string{"rail_account": "unknown"}, "payment_count"))

	byDiscount := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"gross_revenue"}, By: []string{"discount_code"}, Range: juneQ}))
	require.Equal(t, int64(5_000_000), cell(t, byDiscount, map[string]string{"discount_code": "PROMO"}, "gross_revenue"))

	byBrand := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"payment_count"}, By: []string{"card_brand"}, Range: juneQ}))
	require.Equal(t, int64(2), cell(t, byBrand, map[string]string{"card_brand": "visa"}, "payment_count"))
}

func TestMetrics_ZeroFillAndCompare(t *testing.T) {
	_, svc, ctxA, _ := seed(t)

	// Zero-fill: every day in range appears; empty days are explicit zeros.
	daily := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"net_revenue"}, By: []string{"time"}, Grain: "day",
		Range:   &metrics.QueryRange{From: "2026-06-01", To: "2026-06-07"},
		Filters: map[string][]string{"rail": {"nmi"}},
	}))
	require.Len(t, daily.Rows, 7)
	require.Equal(t, int64(10_000_000), cell(t, daily, map[string]string{"time": "2026-06-02T00:00:00Z"}, "net_revenue"))
	require.Equal(t, int64(0), cell(t, daily, map[string]string{"time": "2026-06-01T00:00:00Z"}, "net_revenue"))

	// compare=previous_period returns the shifted window (May: sale0 = 7M).
	cmp := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"net_revenue"}, Range: juneQ, Compare: "previous_period"}))
	require.Equal(t, int64(10_000_000), cell(t, cmp, map[string]string{}, "net_revenue"))
	require.NotNil(t, cmp.CompareRange)
	require.Len(t, cmp.CompareRows, 1)
	require.Equal(t, int64(7_000_000), cmp.CompareRows[0][colIdx(t, cmp, "net_revenue")])
}

// --- subscriptions ---------------------------------------------------------------

func TestMetrics_SubscriptionFlowAndChurn(t *testing.T) {
	_, svc, ctxA, _ := seed(t)

	res := run(t, svc, ctxA, &metrics.Query{
		Measures: []string{"new_subscriptions", "cancellations", "churn_rate", "avg_membership_duration_days"},
		Range:    juneQ,
	})
	m := map[string]string{}
	require.Equal(t, int64(2), cell(t, res, m, "new_subscriptions"))
	require.Equal(t, int64(2), cell(t, res, m, "cancellations"))
	require.InDelta(t, 0.4, cell(t, res, m, "churn_rate").(float64), 1e-9) // 2 cancels / 5 at period start
	require.InDelta(t, 54.5, cell(t, res, m, "avg_membership_duration_days").(float64), 1e-6)

	bySubType := run(t, svc, ctxA, &metrics.Query{Measures: []string{"new_subscriptions"}, By: []string{"subscriber_type"}, Range: juneQ})
	require.Equal(t, int64(1), cell(t, bySubType, map[string]string{"subscriber_type": "first_time"}, "new_subscriptions"))
	require.Equal(t, int64(1), cell(t, bySubType, map[string]string{"subscriber_type": "returning"}, "new_subscriptions"))

	byCancelType := run(t, svc, ctxA, &metrics.Query{Measures: []string{"cancellations"}, By: []string{"cancel_type"}, Range: juneQ})
	require.Equal(t, int64(1), cell(t, byCancelType, map[string]string{"cancel_type": "user"}, "cancellations"))
	require.Equal(t, int64(1), cell(t, byCancelType, map[string]string{"cancel_type": "failed_payment"}, "cancellations"))
}

func TestMetrics_SnapshotReconstruction(t *testing.T) {
	_, svc, ctxA, _ := seed(t)

	// Pinned interval reconstruction: exact counts/mrr at each month START edge,
	// including billing-cycle monthly normalization (annual 96M -> 8M).
	series := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"subscriptions", "mrr"}, By: []string{"time"}, Grain: "month",
		Range: &metrics.QueryRange{From: "2026-04-01", To: "2026-06-30"},
	}))
	require.Len(t, series.Rows, 3)
	require.Equal(t, int64(4), cell(t, series, map[string]string{"time": "2026-04-01T00:00:00Z"}, "subscriptions"))
	require.Equal(t, int64(38_000_000), cell(t, series, map[string]string{"time": "2026-04-01T00:00:00Z"}, "mrr"))
	require.Equal(t, int64(4), cell(t, series, map[string]string{"time": "2026-05-01T00:00:00Z"}, "subscriptions"))
	require.Equal(t, int64(5), cell(t, series, map[string]string{"time": "2026-06-01T00:00:00Z"}, "subscriptions"))
	require.Equal(t, int64(48_000_000), cell(t, series, map[string]string{"time": "2026-06-01T00:00:00Z"}, "mrr"))

	// A snapshot is NEVER summed across buckets: the whole-June single value
	// equals the point-in-time count, not a 30-day sum.
	single := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"subscriptions"}, Range: juneQ}))
	require.Equal(t, int64(5), cell(t, single, map[string]string{}, "subscriptions")) // at range end 7/1: sub1,3,4,5,7

	// status splits use CURRENT status (documented caveat): sub6 covers 6/1 but
	// reads 'cancelled' today.
	byStatus := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"subscriptions", "mrr"}, By: []string{"time", "status"}, Grain: "month",
		Range: &metrics.QueryRange{From: "2026-06-01", To: "2026-06-30"},
	}))
	jun := "2026-06-01T00:00:00Z"
	require.Equal(t, int64(3), cell(t, byStatus, map[string]string{"time": jun, "status": "active"}, "subscriptions"))
	require.Equal(t, int64(1), cell(t, byStatus, map[string]string{"time": jun, "status": "past_due"}, "subscriptions"))
	require.Equal(t, int64(1), cell(t, byStatus, map[string]string{"time": jun, "status": "cancelled"}, "subscriptions"))
	require.Equal(t, int64(10_000_000), cell(t, byStatus, map[string]string{"time": jun, "status": "past_due"}, "mrr"))

	// billable = auto-renew + non-terminal + not scheduled to end.
	billable := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"billable_subscriptions"}, By: []string{"time"}, Grain: "month",
		Range: &metrics.QueryRange{From: "2026-06-01", To: "2026-06-30"},
	}))
	require.Equal(t, int64(4), cell(t, billable, map[string]string{"time": jun}, "billable_subscriptions"))

	// Weekly-cycle normalization pinned at the range end (sub7: 2.3M/168h).
	mrrNow := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"mrr"}, Range: juneQ}))
	require.Equal(t, int64(10_000_000+8_000_000+10_000_000+10_000_000+9_994_048), cell(t, mrrNow, map[string]string{}, "mrr"))
}

func TestMetrics_RecoveryRate(t *testing.T) {
	_, svc, ctxA, _ := seed(t)
	res := run(t, svc, ctxA, &metrics.Query{
		Measures: []string{"recovery_rate"},
		Range:    &metrics.QueryRange{From: "2026-06-01", To: "2026-12-31"},
	})
	// Trigger-recorded transitions: sub5 past_due->active, sub6 past_due->cancelled.
	require.InDelta(t, 0.5, cell(t, res, map[string]string{}, "recovery_rate").(float64), 1e-9)
}

// --- credits / usage / balances -----------------------------------------------------

func TestMetrics_UsageCreditsFlow(t *testing.T) {
	_, svc, ctxA, _ := seed(t)

	res := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"credits_sold", "usage_revenue", "usage_units", "active_payers", "credit_utilization", "repeat_topup_rate"},
		Range:    juneQ,
	}))
	m := map[string]string{}
	require.Equal(t, int64(30_000_000), cell(t, res, m, "credits_sold"))
	require.Equal(t, int64(24_000_000), cell(t, res, m, "usage_revenue"))
	require.Equal(t, int64(4), cell(t, res, m, "usage_units"))
	require.Equal(t, int64(2), cell(t, res, m, "active_payers"))
	require.InDelta(t, 0.8, cell(t, res, m, "credit_utilization").(float64), 1e-9)
	require.InDelta(t, 0.5, cell(t, res, m, "repeat_topup_rate").(float64), 1e-9)

	// top payers tile: order desc + limit.
	top := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"usage_revenue"}, By: []string{"payer"},
		Range: juneQ,
		Order: []metrics.OrderTerm{{Measure: "usage_revenue", Dir: "desc"}},
		Limit: intp(1),
	}))
	require.Len(t, top.Rows, 1)
	require.Equal(t, c[2].String(), top.Rows[0][colIdx(t, top, "payer")])
	require.Equal(t, int64(16_000_000), top.Rows[0][colIdx(t, top, "usage_revenue")])

	bySku := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"usage_revenue"}, By: []string{"sku", "rate_card"}, Range: juneQ}))
	require.Equal(t, int64(8_000_000), cell(t, bySku, map[string]string{"sku": "api", "rate_card": "gpt"}, "usage_revenue"))
	require.Equal(t, int64(16_000_000), cell(t, bySku, map[string]string{"sku": "img", "rate_card": "flux"}, "usage_revenue"))
}

func TestMetrics_BalancesAndDepletion(t *testing.T) {
	_, svc, ctxA, _ := seed(t)

	// Point-in-time balances at the range end (strictly-before semantics).
	bal := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"outstanding_credit_liability", "outstanding_owed"},
		Range:    juneQ,
	}))
	m := map[string]string{}
	require.Equal(t, int64(36_000_000), cell(t, bal, m, "outstanding_credit_liability"))
	require.Equal(t, int64(3_000_000), cell(t, bal, m, "outstanding_owed"))

	// Series: balance at each month START (May 1: nothing yet; Jun 1: deposits 30M).
	series := run(t, svc, ctxA, usd(&metrics.Query{
		Measures: []string{"outstanding_credit_liability"}, By: []string{"time"}, Grain: "month",
		Range: &metrics.QueryRange{From: "2026-05-01", To: "2026-06-30"},
	}))
	require.Equal(t, int64(0), cell(t, series, map[string]string{"time": "2026-05-01T00:00:00Z"}, "outstanding_credit_liability"))
	require.Equal(t, int64(30_000_000), cell(t, series, map[string]string{"time": "2026-06-01T00:00:00Z"}, "outstanding_credit_liability"))

	// Depletion risk at 7/1: c2's balance (14M) covers exactly 7 days of its
	// trailing-7d burn (14M) -> at risk; c1 has no trailing burn -> not counted.
	risk := run(t, svc, ctxA, &metrics.Query{Measures: []string{"payers_at_depletion_risk"}, Range: juneQ})
	require.Equal(t, int64(1), cell(t, risk, map[string]string{}, "payers_at_depletion_risk"))
}

func TestMetrics_EntitledCustomers(t *testing.T) {
	_, svc, ctxA, _ := seed(t)
	res := run(t, svc, ctxA, &metrics.Query{Measures: []string{"entitled_customers"}, By: []string{"entitlement"}, Range: juneQ})
	// At 7/1: c2's premium window ended 6/15; c1 premium + c3 gold remain.
	require.Equal(t, int64(1), cell(t, res, map[string]string{"entitlement": "premium"}, "entitled_customers"))
	require.Equal(t, int64(1), cell(t, res, map[string]string{"entitlement": "gold"}, "entitled_customers"))
}

func TestMetrics_AdmissionDenials(t *testing.T) {
	_, svc, ctxA, _ := seed(t)
	res := run(t, svc, ctxA, &metrics.Query{Measures: []string{"admission_denials"}, By: []string{"denial_reason"}, Range: juneQ})
	require.Equal(t, int64(8), cell(t, res, map[string]string{"denial_reason": "insufficient_credit"}, "admission_denials"))
	require.Equal(t, int64(2), cell(t, res, map[string]string{"denial_reason": "budget_exceeded"}, "admission_denials"))
}

// --- isolation -----------------------------------------------------------------------

func TestMetrics_MerchantIsolation(t *testing.T) {
	_, svc, ctxA, ctxB := seed(t)

	a := run(t, svc, ctxA, usd(&metrics.Query{Measures: []string{"gross_revenue"}, Range: juneQ}))
	require.Equal(t, int64(25_000_000), cell(t, a, map[string]string{}, "gross_revenue"), "merchant A must never see B's 999M payment")

	b := run(t, svc, ctxB, usd(&metrics.Query{Measures: []string{"gross_revenue", "payment_count"}, Range: juneQ}))
	require.Equal(t, int64(999_000_000), cell(t, b, map[string]string{}, "gross_revenue"))
	require.Equal(t, int64(1), cell(t, b, map[string]string{}, "payment_count"))

	bSubs := run(t, svc, ctxB, &metrics.Query{Measures: []string{"subscriptions", "new_subscriptions", "cancellations"}, Range: juneQ})
	require.Equal(t, int64(0), cell(t, bSubs, map[string]string{}, "subscriptions"))
	require.Equal(t, int64(0), cell(t, bSubs, map[string]string{}, "new_subscriptions"))
}
