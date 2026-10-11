package migrate

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"
)

// Migration 61 turns invoices into usage orders under their own ids: an
// invoice's own charges become its lines, one per accrual source; a prepaid
// statement, which billed none of them, is dropped.
func TestUsageOrdersMigrationConvertsInvoices(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required for the populated migration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "usage_orders_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	migrations, err := loadMigrations(schema)
	require.NoError(t, err)
	before := -1
	for i, m := range migrations {
		if strings.HasPrefix(m.Name, "0061_") {
			before = i
		}
	}
	require.Positive(t, before)
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	require.NoError(t, err)
	defer migrator.Close()
	migrator.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations[:before]))

	q := func(sql string) string { return strings.ReplaceAll(sql, "billing.", schema+".") }
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Exec(ctx, q(sql), args...)
		require.NoError(t, err)
	}
	merchantID, customerID := uuid.New(), uuid.New()
	open, paid, voided, statement := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'usage-orders','active')`, merchantID)
	exec(`INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, merchantID, customerID)
	start, end := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	due, paidAt := end.Add(30*24*time.Hour), end.Add(time.Hour)
	invoice := func(id uuid.UUID, status string, total, amountPaid, amountDue int64, number *string, from, to time.Time) {
		exec(`INSERT INTO billing.invoices(merchant_id,id,customer_id,currency,invoice_number,period_starts_at,period_ends_at,usage_total,deposits_total,
			owed_accrued,owed_paid,closing_balance,subtotal_amount,total_amount,amount_paid,amount_due,line_items,money_movements,status,collection_method,
			due_at,paid_at,voided_at,po_number,tax,billing_contacts,memo,finalized_at)
			VALUES($1,$2,$3,'USD',$4,$5,$6,$7,0,$7,0,0,$7,$7,$8,$9,'[]','{}',$10,'send_invoice',$11,
			CASE WHEN $10='paid' THEN $12::timestamptz END,CASE WHEN $10='voided' THEN $12::timestamptz END,'PO-1','{"vat":"GB1"}','[{"name":"AP","email":"ap@example.test"}]','net 30',$6)`,
			merchantID, id, customerID, number, from, to, total, amountPaid, amountDue, status, due, paidAt)
	}
	item := func(invoiceID *uuid.UUID, status, sourceType, source string, amount int64) {
		exec(`INSERT INTO billing.invoice_items(merchant_id,customer_id,currency,invoice_id,source_type,source_id,invoice_at,amount,status,metadata)
			VALUES($1,$2,'USD',$3,$4,$5,$6,$7,$8,CASE WHEN $9='' THEN '{}'::jsonb ELSE jsonb_build_object('source',$9::text) END)`,
			merchantID, customerID, invoiceID, sourceType, uuid.NewString(), start.Add(time.Hour), amount, status, source)
	}
	invoice(open, "open", 50, 20, 30, nil, start, end)
	item(&open, "invoiced", "owed_accrual", "metered:tokens", 30)
	item(&open, "invoiced", "owed_accrual", "", 20)
	invoice(paid, "paid", 10, 10, 0, new("INV-7"), start, start.Add(time.Hour))
	item(&paid, "invoiced", "owed_accrual", "metered:tokens", 10)
	invoice(voided, "voided", 5, 0, 5, nil, start, start.Add(2*time.Hour))
	item(&voided, "invoiced", "owed_accrual", "", 5)
	invoice(statement, "paid", 0, 0, 0, new("INV-8"), start, start.Add(3*time.Hour))
	item(nil, "pending", "owed_accrual", "", 7)
	exec(`INSERT INTO billing.invoice_collection_cadence(merchant_id,monthly_period_started_at,completed_at) VALUES($1,$2,$2)`, merchantID, start)
	exec(`INSERT INTO billing.customer_delinquency(merchant_id,customer_id,currency,state,overdue_started_at,entered_at,overdue_amount,overdue_invoices,transition_seq,evaluated_at)
		VALUES($1,$2,'USD','grace',$3,$3,30,1,1,$3)`, merchantID, customerID, due)

	require.NoError(t, migrator.ApplyMigrations(ctx, migrations))

	type order struct {
		origin, status, method string
		total, amountPaid      int64
		number, poNumber       *string
		periodStart, periodEnd time.Time
		dueAt                  time.Time
		completedAt, voidedAt  *time.Time
		tax, contacts          string
	}
	read := func(id uuid.UUID) (order, bool) {
		var o order
		err := pool.QueryRow(ctx, q(`SELECT origin,status,collection_method,total,amount_paid,number,po_number,period_starts_at,period_ends_at,due_at,
			completed_at,voided_at,tax::text,billing_contacts::text FROM billing.orders WHERE merchant_id=$1 AND id=$2`), merchantID, id).
			Scan(&o.origin, &o.status, &o.method, &o.total, &o.amountPaid, &o.number, &o.poNumber, &o.periodStart, &o.periodEnd, &o.dueAt, &o.completedAt, &o.voidedAt, &o.tax, &o.contacts)
		if err == pgx.ErrNoRows {
			return o, false
		}
		require.NoError(t, err)
		return o, true
	}
	got, ok := read(open)
	require.True(t, ok)
	require.Equal(t, []any{"usage", "open", "send_invoice", int64(50), int64(20), "PO-1"}, []any{got.origin, got.status, got.method, got.total, got.amountPaid, *got.poNumber})
	require.Nil(t, got.number, "numbered only once paid")
	require.True(t, got.dueAt.Equal(due) && got.periodStart.Equal(start) && got.periodEnd.Equal(end))
	require.JSONEq(t, `{"vat":"GB1"}`, got.tax)
	require.JSONEq(t, `[{"name":"AP","email":"ap@example.test"}]`, got.contacts)
	lines := map[string]int64{}
	rows, err := pool.Query(ctx, q(`SELECT source, amount FROM billing.order_lines WHERE merchant_id=$1 AND order_id=$2`), merchantID, open)
	require.NoError(t, err)
	for rows.Next() {
		var source string
		var amount int64
		require.NoError(t, rows.Scan(&source, &amount))
		lines[source] = amount
	}
	require.NoError(t, rows.Err())
	require.Equal(t, map[string]int64{"metered:tokens": 30, "owed_accrual": 20}, lines, "one line per accrual source")

	got, ok = read(paid)
	require.True(t, ok)
	require.Equal(t, []any{"complete", int64(10), int64(10), "INV-7"}, []any{got.status, got.total, got.amountPaid, *got.number})
	require.True(t, got.completedAt.Equal(paidAt))
	got, ok = read(voided)
	require.True(t, ok)
	require.Equal(t, "void", got.status)
	require.True(t, got.voidedAt.Equal(paidAt))
	_, ok = read(statement)
	require.False(t, ok, "a statement that billed none of its own charges is not an order")

	var billed, pending int
	require.NoError(t, pool.QueryRow(ctx, q(`SELECT count(*) FILTER (WHERE status='billed' AND order_id IS NOT NULL), count(*) FILTER (WHERE status='pending' AND order_id IS NULL)
		FROM billing.owed_items WHERE merchant_id=$1`), merchantID).Scan(&billed, &pending))
	require.Equal(t, []int{4, 1}, []int{billed, pending})
	var cadence time.Time
	require.NoError(t, pool.QueryRow(ctx, q(`SELECT monthly_period_started_at FROM billing.usage_collection_cadence WHERE merchant_id=$1`), merchantID).Scan(&cadence))
	require.True(t, cadence.Equal(start))
	var overdue int64
	require.NoError(t, pool.QueryRow(ctx, q(`SELECT overdue_orders FROM billing.customer_delinquency WHERE merchant_id=$1`), merchantID).Scan(&overdue))
	require.EqualValues(t, 1, overdue)
	var invoices bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, schema+".invoices").Scan(&invoices))
	require.False(t, invoices)
}
