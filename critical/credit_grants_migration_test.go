//go:build integration

package critical_test

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/migrate"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
	"github.com/stretchr/testify/require"
)

// Credit lots were grants rows of kind credit whose remainder was the face
// less a list of transfer types. The migration that moves them to
// credit_grants keeps their ids and counts what the ledger posted.
func TestCreditGrantsMigration(t *testing.T) {
	t.Run("OR-J31-S15/credit-lots-leave-grants-with-their-old-remainders", func(t *testing.T) {
		t.Run("moves-every-lot", migrateCreditLots)
		for _, refusal := range migrationRefusals {
			t.Run("refuses-"+refusal.name, func(t *testing.T) { refuseMigration(t, refusal) })
		}
		t.Run("a-migrated-purchase-deposit-replays", migratedPurchaseDepositReplays)
	})
}

// legacySchema is a schema migrated up to the migration under test, and the
// full chain that then runs it.
type legacySchema struct {
	pool   *pgxpool.Pool
	schema string
	full   fs.FS
}

func newLegacySchema(t *testing.T) *legacySchema {
	t.Helper()
	return newLegacySchemaBefore(t, "_credit_grants.up.sql")
}

// newLegacySchemaBefore is a schema migrated up to the migration whose name
// ends in suffix.
func newLegacySchemaBefore(t *testing.T, suffix string) *legacySchema {
	t.Helper()
	dsn := os.Getenv("OPENRAILS_CRITICAL_DSN")
	require.NotEmpty(t, dsn, "OPENRAILS_CRITICAL_DSN must name a disposable PostgreSQL database; critical scenarios never skip")
	pool, err := pgxpool.New(t.Context(), dsn)
	require.NoError(t, err)
	s := &legacySchema{pool: pool, schema: "critical_" + strings.ReplaceAll(uuid.NewString(), "-", ""), full: postgresmigrations.FS}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{s.schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
		pool.Close()
	})
	names, err := fs.Glob(postgresmigrations.FS, "*.up.sql")
	require.NoError(t, err)
	slices.Sort(names)
	at := slices.IndexFunc(names, func(name string) bool { return strings.HasSuffix(name, suffix) })
	require.GreaterOrEqual(t, at, 0)
	before := fstest.MapFS{}
	for _, name := range names[:at] {
		body, err := fs.ReadFile(postgresmigrations.FS, name)
		require.NoError(t, err)
		before[name] = &fstest.MapFile{Data: body}
	}
	require.NoError(t, s.apply(t.Context(), before))
	return s
}

func (s *legacySchema) apply(ctx context.Context, chain fs.FS) error {
	return migrate.Apply(ctx, s.pool, migrate.Options{Schema: s.schema, Chain: chain, Logger: slog.New(slog.DiscardHandler)})
}

func (s *legacySchema) table(name string) string { return pgx.Identifier{s.schema, name}.Sanitize() }

// seed writes rows as the code before credit_grants wrote them, in one
// transaction.
type seed struct {
	t  *testing.T
	s  *legacySchema
	tx pgx.Tx
	m  uuid.UUID
	at time.Time
}

func (s *legacySchema) seed(t *testing.T) *seed {
	t.Helper()
	tx, err := s.pool.Begin(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	d := &seed{t: t, s: s, tx: tx, m: uuid.New(), at: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	d.exec("INSERT INTO "+s.table("merchants")+" (id, slug, status) VALUES ($1, $2, 'active')", d.m, "migrate-"+d.m.String()[:8])
	return d
}

func (d *seed) exec(sql string, args ...any) {
	d.t.Helper()
	_, err := d.tx.Exec(d.t.Context(), sql, args...)
	require.NoError(d.t, err, sql)
}

func (d *seed) customer() uuid.UUID {
	id := uuid.New()
	d.exec("INSERT INTO "+d.s.table("customers")+" (merchant_id, id) VALUES ($1, $2)", d.m, id)
	return id
}

func (d *seed) account(kind string, customer *uuid.UUID) uuid.UUID {
	id := uuid.New()
	d.exec("INSERT INTO "+d.s.table("ledger_accounts")+" (id, merchant_id, customer_id, account_type, currency, debits_must_not_exceed_credits, credits_must_not_exceed_debits)"+
		" VALUES ($1, $2, $3, $4, 'USD', $5, false)", id, d.m, customer, kind, customer != nil && kind == "customer_balance")
	return id
}

// leg is a transfer naming lot, as the old code posted it.
func (d *seed) leg(debit, credit uuid.UUID, amount int64, kind, operation, source, sourceID string, lot uuid.UUID, customer *uuid.UUID) {
	d.exec("INSERT INTO "+d.s.table("ledger_transfers")+" (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type,"+
		" allow_debit_negative_up_to, source, source_id, grant_id, customer_id, operation) VALUES ($1, $2, $3, $4, 'USD', $5, 0, $6, $7, $8, $9, $10)",
		d.m, debit, credit, amount, kind, source, sourceID, lot, customer, operation)
}

// lot is a credit grant row as depositTx or purchasedcredits.Fund wrote it.
func (d *seed) lot(customer uuid.UUID, sourceType, sourceID string, payment, product *uuid.UUID, amount int64, spec string, reason *string) uuid.UUID {
	id := uuid.New()
	d.exec("INSERT INTO "+d.s.table("grants")+" (id, merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, spec_snapshot,"+
		" starts_at, ends_at, amount, currency, reason) VALUES ($1, $2, $3, $4, 'credit', $5, $6, $7, 'grant', $8::jsonb, $9, $10, $11, 'USD', $12)",
		id, d.m, customer, product, sourceType, sourceID, payment, spec, d.at, d.at.AddDate(1, 0, 0), amount, reason)
	return id
}

// revoke is the termination row terminate appended to a lot.
func (d *seed) revoke(lot uuid.UUID) (uuid.UUID, time.Time) { return d.terminate(lot, "revoke") }

func (d *seed) terminate(lot uuid.UUID, event string) (uuid.UUID, time.Time) {
	id, at := uuid.New(), d.at.Add(time.Hour)
	d.exec("INSERT INTO "+d.s.table("grants")+" (id, merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, event, supersedes_id,"+
		" spec_snapshot, starts_at, amount, currency, reason, actor)"+
		" SELECT $1, merchant_id, customer_id, product_id, kind, source_type, source_id, payment_id, $5, id, spec_snapshot, $3, amount, currency, 'abuse', 'staff'"+
		" FROM "+d.s.table("grants")+" WHERE merchant_id = $2 AND id = $4", id, d.m, at, lot, event)
	return id, at
}

// sale is a paid order line of a credit product, with its payment.
func (d *seed) sale(customer, product, price uuid.UUID, amount int64, interval *int) (payment, line uuid.UUID) {
	order, payment, line := uuid.New(), uuid.New(), uuid.New()
	d.exec("INSERT INTO "+d.s.table("orders")+" (merchant_id, id, customer_id, origin, status, currency, total, payment_status)"+
		" VALUES ($1, $2, $3, 'customer', 'processing', 'USD', $4, 'processing')", d.m, order, customer, amount)
	d.exec("INSERT INTO "+d.s.table("order_lines")+" (merchant_id, id, order_id, customer_id, position, price_id, product_id, description, quantity,"+
		" unit_amount, amount, ownership, credit_grant, billing_interval_hours)"+
		" VALUES ($1, $2, $3, $4, 0, $5, $6, 'Credit pack', 1, $7, $7, 'consumable', '{\"currency\":\"USD\",\"amount\":\"25000000\"}'::jsonb, $8)",
		d.m, line, order, customer, price, product, amount, interval)
	d.exec("INSERT INTO "+d.s.table("payments")+" (merchant_id, id, customer_id, order_id, channel, transaction_id, amount, list_amount, currency, status, purchased_at)"+
		" VALUES ($1, $2, $3, $4, 'manual', $5, $6, $6, 'USD', 'succeeded', $7)", d.m, payment, customer, order, "sale-"+payment.String(), amount, d.at)
	return payment, line
}

// reversal is a refund, chargeback or won dispute of payment.
func (d *seed) reversal(customer, payment uuid.UUID, amount int64, status, kind string) uuid.UUID {
	id := uuid.New()
	d.exec("INSERT INTO "+d.s.table("payments")+" (merchant_id, id, customer_id, order_id, channel, transaction_id, amount, list_amount, currency, status, purchased_at, refunded_payment_id, reversal_kind)"+
		" SELECT merchant_id, $3, customer_id, order_id, 'manual', $4, $5, list_amount, currency, $6, purchased_at, id, $7"+
		" FROM "+d.s.table("payments")+" WHERE merchant_id = $1 AND id = $2", d.m, payment, id, "reversal-"+id.String(), amount, status, kind)
	return id
}

// catalog is a credit product and its price.
func (d *seed) catalog() (product, price uuid.UUID) {
	product, price = uuid.New(), uuid.New()
	d.exec("INSERT INTO "+d.s.table("products")+" (merchant_id, id, key, display_name, tier_rank, archived, ownership) VALUES ($1, $2, $3, 'Credit pack', 0, false, 'consumable')", d.m, product, "pack-"+product.String()[:8])
	d.exec("INSERT INTO "+d.s.table("prices")+" (merchant_id, id, product_id, key, amount, currency, archived) VALUES ($1, $2, $3, 'pack', 10000000, 'USD', false)", d.m, price, product)
	return product, price
}

func (d *seed) commit() {
	d.t.Helper()
	require.NoError(d.t, d.tx.Commit(d.t.Context()))
}

// oldRemaining is GetCreditLotRemaining before credit_grants: the face less
// the transfer types the lot was drawn by.
const oldRemaining = `SELECT g.id, (g.amount - COALESCE((
    SELECT SUM(CASE WHEN t.transfer_type = 'credit_refund_restore' THEN -t.amount ELSE t.amount END) FROM %[1]s t
    WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
      AND t.transfer_type IN ('credit_spend', 'owed_repayment', 'credit_expire', 'credit_revoke', 'credit_refund', 'credit_refund_restore')
), 0))::bigint FROM %[2]s g WHERE g.merchant_id = $1 AND g.kind = 'credit' AND g.event = 'grant'`

// oldReserved is ListSpendableCreditLots' pending-refund share.
const oldReserved = `SELECT g.id, COALESCE((
    SELECT ceil(g.amount::numeric * LEAST(sum(-refund.amount), (g.spec_snapshot->'deposit'->>'paid_amount')::numeric) / (g.spec_snapshot->'deposit'->>'paid_amount')::numeric)
    FROM %[1]s refund JOIN %[1]s payment ON payment.merchant_id = g.merchant_id AND payment.id = g.payment_id AND payment.deleted_at IS NULL
    WHERE refund.merchant_id = g.merchant_id AND refund.customer_id = g.customer_id AND refund.currency = g.currency
      AND refund.refunded_payment_id = g.payment_id AND refund.status = 'pending' AND refund.amount < 0 AND refund.deleted_at IS NULL
      AND g.spec_snapshot->'deposit'->'paid_amount' IS NOT NULL
    GROUP BY payment.amount), 0)::bigint FROM %[2]s g WHERE g.merchant_id = $1 AND g.kind = 'credit' AND g.event = 'grant'`

func readByLot(t *testing.T, s *legacySchema, query string, merchant uuid.UUID) map[uuid.UUID]int64 {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), query, merchant)
	require.NoError(t, err)
	out := map[uuid.UUID]int64{}
	for rows.Next() {
		var id uuid.UUID
		var v int64
		require.NoError(t, rows.Scan(&id, &v))
		out[id] = v
	}
	require.NoError(t, rows.Err())
	return out
}

type migratedLot struct {
	customer                            uuid.UUID
	units, credited, debited, reserved  int64
	funding, source, invoker            string
	paidAmount                          *int64
	paidCurrency, sourceID, description *string
	payment, orderLine, product         *uuid.UUID
	revokedAt                           *time.Time
	loggedCredited, loggedDebited       int64
}

func readMigratedLots(t *testing.T, s *legacySchema, merchant uuid.UUID) map[uuid.UUID]migratedLot {
	t.Helper()
	rows, err := s.pool.Query(t.Context(), "SELECT g.id, g.customer_id, g.units, g.units_credited, g.units_debited, g.units_reserved, g.funding_source, g.source, g.invoker,"+
		" g.paid_amount, g.paid_currency, g.source_id, g.description, g.payment_id, g.order_line_id, g.product_id, g.revoked_at,"+
		" COALESCE(sum(t.amount) FILTER (WHERE t.credit_account_id = g.account_id), 0)::bigint, COALESCE(sum(t.amount) FILTER (WHERE t.debit_account_id = g.account_id), 0)::bigint"+
		" FROM "+s.table("credit_grants")+" g LEFT JOIN "+s.table("ledger_transfers")+" t ON t.merchant_id = g.merchant_id AND t.credit_grant_id = g.id"+
		" WHERE g.merchant_id = $1 GROUP BY g.merchant_id, g.id", merchant)
	require.NoError(t, err)
	out := map[uuid.UUID]migratedLot{}
	for rows.Next() {
		var id uuid.UUID
		var l migratedLot
		require.NoError(t, rows.Scan(&id, &l.customer, &l.units, &l.credited, &l.debited, &l.reserved, &l.funding, &l.source, &l.invoker,
			&l.paidAmount, &l.paidCurrency, &l.sourceID, &l.description, &l.payment, &l.orderLine, &l.product, &l.revokedAt,
			&l.loggedCredited, &l.loggedDebited))
		out[id] = l
	}
	require.NoError(t, rows.Err())
	return out
}

func migrateCreditLots(t *testing.T) {
	s := newLegacySchema(t)
	d := s.seed(t)
	customer, unfunded := d.customer(), d.customer()
	balance := d.account("customer_balance", &customer)
	clearing := d.account("processor_clearing", nil)
	promotion := d.account("promotional_funding", nil)
	revenue := d.account("platform_revenue", nil)
	revokedCredits := d.account("revoked_credits", nil)
	expiredCredits := d.account("expired_credits", nil)
	refundClearing := d.account("credit_refund_clearing", nil)
	owed := d.account("arrears_liability", &customer)
	product, price := d.catalog()
	deposit := func(lot uuid.UUID, from uuid.UUID, kind string, amount int64) {
		d.leg(from, balance, amount, kind, "deposit", "grant", lot.String(), lot, &customer)
	}
	spec := func(source, invoker, paid string) string {
		if paid == "" {
			return `{"deposit":{"source":"` + source + `","invoker":"` + invoker + `"}}`
		}
		return `{"deposit":{"source":"` + source + `","invoker":"` + invoker + `","paid_amount":"` + paid + `"}}`
	}

	// A $10 pack of 25 credits: $10 from the payment, 15 a bonus; 4 spent,
	// and a $5 refund of it pending.
	packPayment, packLine := d.sale(customer, product, price, 10_000_000, nil)
	pack := d.lot(customer, "purchase", packPayment.String(), &packPayment, &product, 25_000_000, spec("payment", customer.String(), "10000000"), ptr("Credit pack"))
	deposit(pack, clearing, "deposit", 10_000_000)
	deposit(pack, promotion, "deposit_bonus", 15_000_000)
	d.leg(balance, revenue, 4_000_000, "credit_spend", "spend", "critical", "spend-1", pack, &customer)
	d.exec("INSERT INTO "+s.table("payments")+" (merchant_id, customer_id, order_id, channel, transaction_id, amount, list_amount, currency, status, purchased_at, refunded_payment_id, reversal_kind)"+
		" SELECT merchant_id, customer_id, order_id, 'manual', 'pending-refund', -5000000, list_amount, currency, 'pending', purchased_at, id, 'refund'"+
		" FROM "+s.table("payments")+" WHERE merchant_id = $1 AND id = $2", d.m, packPayment)
	// A recurring price's cycle, paid at face.
	cyclePayment, cycleLine := d.sale(customer, product, price, 20_000_000, ptr(720))
	cycle := d.lot(customer, "purchase", cyclePayment.String(), &cyclePayment, &product, 20_000_000, spec("payment", customer.String(), "20000000"), nil)
	deposit(cycle, clearing, "deposit", 20_000_000)
	// The change a usage order's card charge left, and a host deposit
	// labelled a purchase: both were credits_sold, paid at face.
	change := d.lot(customer, "purchase", "invoice-rounding:intent", nil, nil, 6_299, spec("invoice_rounding_purchase", "", ""), ptr(""))
	deposit(change, clearing, "deposit", 6_299)
	hosted := d.lot(customer, "purchase", "pack-42", nil, nil, 1_000_000, spec("pack_purchase", "host", ""), nil)
	deposit(hosted, clearing, "deposit", 1_000_000)
	// Staff grants revoked with and without their clawback posted.
	clawed := d.lot(customer, "admin", "goodwill", nil, nil, 3_000_000, spec("support", "staff", ""), nil)
	deposit(clawed, clearing, "deposit", 3_000_000)
	clawedRevoke, clawedAt := d.revoke(clawed)
	d.leg(balance, revokedCredits, 3_000_000, "credit_revoke", "credit_revoke", "grant_revoke", clawed.String(), clawed, &customer)
	unclawed := d.lot(customer, "admin", "apology", nil, nil, 2_000_000, spec("support", "staff", ""), nil)
	deposit(unclawed, clearing, "deposit", 2_000_000)
	unclawedRevoke, unclawedAt := d.revoke(unclawed)
	// A grant whose deposit never posted, for a customer with no balance yet.
	unposted := d.lot(unfunded, "admin", "unposted", nil, nil, 1_000_000, spec("support", "staff", ""), nil)
	// A lot that lapsed, and one that repaid owed.
	lapsed := d.lot(customer, "admin", "lapsed", nil, nil, 2_000_000, spec("support", "staff", ""), nil)
	deposit(lapsed, clearing, "deposit", 2_000_000)
	d.leg(balance, expiredCredits, 1_500_000, "credit_expire", "credit_expire", "credit_expiry", lapsed.String(), lapsed, &customer)
	repaid := d.lot(customer, "admin", "repaid", nil, nil, 3_000_000, spec("support", "staff", ""), nil)
	deposit(repaid, clearing, "deposit", 3_000_000)
	d.leg(balance, owed, 1_000_000, "owed_repayment", "deposit", "grant", repaid.String(), repaid, &customer)
	// Paid more than its face: the excess is revenue, on no balance.
	overpaidPayment, _ := d.sale(customer, product, price, 10_000_000, nil)
	overpaid := d.lot(customer, "purchase", overpaidPayment.String(), &overpaidPayment, &product, 8_000_000, spec("payment", customer.String(), "10000000"), nil)
	deposit(overpaid, clearing, "deposit", 8_000_000)
	d.leg(clearing, revenue, 2_000_000, "credit_purchase_revenue", "deposit", "grant", overpaid.String(), overpaid, nil)
	refundLegs := func(lot, reversal uuid.UUID, amount int64, restore string) {
		withdraw, cash := "credit_refund", "credit_refund_cash"
		debit, credit, source, cashSource := balance, refundClearing, "credit", "cash"
		cashDebit, cashCredit := refundClearing, clearing
		if restore != "" {
			withdraw, cash = "credit_refund_restore", "credit_refund_cash_restore"
			debit, credit, source, cashSource = refundClearing, balance, "credit_restore:"+restore, "cash_restore:"+restore
			cashDebit, cashCredit = clearing, refundClearing
		}
		d.leg(debit, credit, amount, withdraw, "purchased_credit_refund", source, reversal.String(), lot, &customer)
		d.leg(cashDebit, cashCredit, amount, cash, "purchased_credit_refund", cashSource, reversal.String(), lot, nil)
	}
	// $4 of a $10 pack refunded, then $8 more pending: the reservation is the
	// $6 no reversal returned, not the $8 asked.
	reversedPayment, _ := d.sale(customer, product, price, 10_000_000, nil)
	reversed := d.lot(customer, "purchase", reversedPayment.String(), &reversedPayment, &product, 10_000_000, spec("payment", customer.String(), "10000000"), nil)
	deposit(reversed, clearing, "deposit", 10_000_000)
	refundLegs(reversed, d.reversal(customer, reversedPayment, -4_000_000, "succeeded", "refund"), 4_000_000, "")
	d.reversal(customer, reversedPayment, -8_000_000, "pending", "refund")
	// A chargeback, then its won dispute.
	disputedPayment, _ := d.sale(customer, product, price, 5_000_000, nil)
	disputed := d.lot(customer, "purchase", disputedPayment.String(), &disputedPayment, &product, 5_000_000, spec("payment", customer.String(), "5000000"), nil)
	deposit(disputed, clearing, "deposit", 5_000_000)
	chargeback := d.reversal(customer, disputedPayment, -5_000_000, "succeeded", "chargeback")
	refundLegs(disputed, chargeback, 5_000_000, "")
	refundLegs(disputed, d.reversal(customer, disputedPayment, 5_000_000, "succeeded", "dispute_reversal"), 5_000_000, chargeback.String())
	// History that stays a grant.
	history := uuid.New()
	d.exec("INSERT INTO "+s.table("grants")+" (id, merchant_id, customer_id, kind, source_type, source_id, event, spec_snapshot, starts_at)"+
		" VALUES ($1, $2, $3, 'entitlement', 'purchase', 'old-purchase', 'grant', '{\"entitlements\":[\"reader\"]}'::jsonb, $4)", history, d.m, customer, d.at)
	d.commit()

	remaining := readByLot(t, s, strings.ReplaceAll(strings.ReplaceAll(oldRemaining, "%[1]s", s.table("ledger_transfers")), "%[2]s", s.table("grants")), d.m)
	reserved := readByLot(t, s, strings.ReplaceAll(strings.ReplaceAll(oldReserved, "%[1]s", s.table("payments")), "%[2]s", s.table("grants")), d.m)
	require.Len(t, remaining, 12)

	require.NoError(t, s.apply(t.Context(), s.full))

	lots := readMigratedLots(t, s, d.m)
	require.Len(t, lots, 12, "every lot moved, with its id")
	for id, lot := range lots {
		require.Equal(t, lot.loggedCredited, lot.credited, "lot %s credits are its legs on its balance", id)
		require.Equal(t, lot.loggedDebited, lot.debited, "lot %s debits are its legs on its balance", id)
		require.LessOrEqual(t, lot.debited, lot.credited)
		if id != reversed {
			require.Equal(t, reserved[id], lot.reserved, "lot %s reserves the old pending-refund share", id)
		}
		if id == unposted {
			continue
		}
		require.Equal(t, remaining[id], lot.credited-lot.debited, "lot %s keeps the old remainder", id)
	}
	require.EqualValues(t, 500_000, lots[lapsed].credited-lots[lapsed].debited)
	require.EqualValues(t, 2_000_000, lots[repaid].credited-lots[repaid].debited)
	require.EqualValues(t, 8_000_000, lots[overpaid].credited, "the excess paid is revenue, not the lot's")
	require.EqualValues(t, 10_000_000, *lots[overpaid].paidAmount)
	require.EqualValues(t, 6_000_000, lots[reversed].credited-lots[reversed].debited)
	require.EqualValues(t, 8_000_000, reserved[reversed], "the old share ignored the refund already settled")
	require.EqualValues(t, 6_000_000, lots[reversed].reserved, "the pending $8 reserves the $6 of the line no reversal returned")
	require.EqualValues(t, 10_000_000, lots[disputed].credited, "the restore credits the lot again")
	require.EqualValues(t, 5_000_000, lots[disputed].credited-lots[disputed].debited)
	require.EqualValues(t, 12_500_000, lots[pack].reserved, "the pending $5 of the $10 line reserves half the pack")
	require.EqualValues(t, 21_000_000, lots[pack].credited-lots[pack].debited)

	require.Equal(t, "purchase", lots[pack].funding)
	require.EqualValues(t, 10_000_000, *lots[pack].paidAmount)
	require.Equal(t, "USD", *lots[pack].paidCurrency)
	require.Equal(t, packPayment, *lots[pack].payment)
	require.Equal(t, packLine, *lots[pack].orderLine)
	require.Equal(t, product, *lots[pack].product)
	require.Equal(t, "Credit pack", *lots[pack].description)
	require.Equal(t, "payment", lots[pack].source)
	require.Equal(t, customer.String(), lots[pack].invoker)

	require.Equal(t, "recurring", lots[cycle].funding)
	require.Equal(t, cycleLine, *lots[cycle].orderLine)

	for _, id := range []uuid.UUID{change, hosted} {
		require.Equal(t, "purchase", lots[id].funding, "a deposit labelled a purchase was bought")
		require.Equal(t, lots[id].units, *lots[id].paidAmount, "at face")
		require.Nil(t, lots[id].payment)
	}
	require.Equal(t, customer.String(), lots[change].invoker, "a deposit without an invoker was its customer's")
	require.Nil(t, lots[change].description, "an empty description is absent")
	require.Equal(t, "host", lots[hosted].invoker)
	require.Equal(t, "pack_purchase", lots[hosted].source)

	for id, at := range map[uuid.UUID]time.Time{clawed: clawedAt, unclawed: unclawedAt} {
		require.Equal(t, "grant", lots[id].funding)
		require.Nil(t, lots[id].paidAmount)
		require.NotNil(t, lots[id].revokedAt)
		require.True(t, at.Equal(*lots[id].revokedAt))
	}
	require.Zero(t, lots[clawed].credited-lots[clawed].debited)
	require.EqualValues(t, 2_000_000, lots[unclawed].credited-lots[unclawed].debited, "the revocation's units are still on the balance")
	var events int
	require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+s.table("credit_grant_events")+
		" WHERE merchant_id = $1 AND event = 'revoke' AND reason = 'abuse' AND actor = 'staff' AND ((id = $2 AND credit_grant_id = $3) OR (id = $4 AND credit_grant_id = $5))",
		d.m, clawedRevoke, clawed, unclawedRevoke, unclawed).Scan(&events))
	require.Equal(t, 2, events, "revocations keep their ids")

	require.Zero(t, lots[unposted].credited, "no unit of the unposted lot posted")
	var accounts int
	require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+s.table("ledger_accounts")+
		" WHERE merchant_id = $1 AND customer_id = $2 AND account_type = 'customer_units' AND currency = 'USD'", d.m, unfunded).Scan(&accounts))
	require.Equal(t, 1, accounts, "the unposted lot's customer has the balance it will be deposited to")

	var left, kept int
	require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT count(*) FILTER (WHERE kind = 'credit'), count(*) FILTER (WHERE id = $2) FROM "+s.table("grants")+" WHERE merchant_id = $1", d.m, history).Scan(&left, &kept))
	require.Zero(t, left)
	require.Equal(t, 1, kept, "grants keeps its history")
	_, err := s.pool.Exec(t.Context(), "UPDATE "+s.table("credit_grants")+" SET units_debited = units_debited + 1 WHERE merchant_id = $1 AND id = $2", d.m, pack)
	require.ErrorContains(t, err, "credit grant facts are immutable", "the ledger alone moves a lot's counters")
}

// migrationRefusal is old data the migration refuses with nothing moved:
// what it cannot place without guessing.
type migrationRefusal struct {
	name, message string
	seed          func(d *seed, customer, balance, clearing uuid.UUID)
}

var migrationRefusals = []migrationRefusal{
	{"a-lot-drawn-before-its-deposit", "drawn beyond what they credited", func(d *seed, customer, balance, _ uuid.UUID) {
		overdrawn := d.lot(customer, "admin", "overdrawn", nil, nil, 1_000_000, `{"deposit":{"source":"support","invoker":"staff"}}`, nil)
		d.revoke(overdrawn)
		d.leg(balance, d.account("revoked_credits", nil), 1_000_000, "credit_revoke", "credit_revoke", "grant_revoke", overdrawn.String(), overdrawn, &customer)
	}},
	{"a-termination-that-is-not-a-revocation", "terminations are not revocations", func(d *seed, customer, _, _ uuid.UUID) {
		lot := d.lot(customer, "admin", "expired-by-event", nil, nil, 1_000_000, `{"deposit":{"source":"support","invoker":"staff"}}`, nil)
		d.terminate(lot, "expire")
	}},
	{"a-paid-amount-without-a-payment", "disagree on their payment and paid amount", func(d *seed, customer, _, _ uuid.UUID) {
		d.lot(customer, "purchase", "unpaid", nil, nil, 1_000_000, `{"deposit":{"source":"payment","invoker":"staff","paid_amount":"1000000"}}`, nil)
	}},
	{"an-amount-on-a-grant-that-is-not-a-lot", "grants that are not credit lots carry an amount", func(d *seed, customer, _, _ uuid.UUID) {
		d.exec("INSERT INTO "+d.s.table("grants")+" (merchant_id, customer_id, kind, source_type, source_id, event, starts_at, amount, currency)"+
			" VALUES ($1, $2, 'entitlement', 'purchase', 'priced-history', 'grant', $3, 5, 'USD')", d.m, customer, d.at)
	}},
	{"a-transfer-naming-another-customers-lot", "ledger transfers name a lot that is not theirs", func(d *seed, customer, _, clearing uuid.UUID) {
		other := d.customer()
		otherBalance := d.account("customer_balance", &other)
		lot := d.lot(customer, "admin", "not-theirs", nil, nil, 1_000_000, `{"deposit":{"source":"support","invoker":"staff"}}`, nil)
		d.leg(clearing, otherBalance, 1_000_000, "deposit", "deposit", "grant", lot.String(), lot, &other)
	}},
}

// refuseMigration seeds one funded lot and the refused data; the migration
// fails with its message and moves nothing.
func refuseMigration(t *testing.T, refusal migrationRefusal) {
	s := newLegacySchema(t)
	d := s.seed(t)
	customer := d.customer()
	balance := d.account("customer_balance", &customer)
	clearing := d.account("processor_clearing", nil)
	funded := d.lot(customer, "admin", "funded", nil, nil, 5_000_000, `{"deposit":{"source":"support","invoker":"staff"}}`, nil)
	d.leg(clearing, balance, 5_000_000, "deposit", "deposit", "grant", funded.String(), funded, &customer)
	refusal.seed(d, customer, balance, clearing)
	d.commit()

	err := s.apply(t.Context(), s.full)
	require.ErrorContains(t, err, refusal.message)
	var lots int
	require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+s.table("grants")+" WHERE merchant_id = $1 AND kind = 'credit' AND event = 'grant'", d.m).Scan(&lots))
	require.GreaterOrEqual(t, lots, 1, "the refused migration moved nothing")
	var moved bool
	require.NoError(t, s.pool.QueryRow(t.Context(), "SELECT to_regclass($1) IS NOT NULL", s.schema+".credit_grants").Scan(&moved))
	require.False(t, moved)
}

// A host deposit labelled a purchase moved as a purchase lot. Its caller's
// identical retry, which names no funding source, replays it; one naming
// another funding source is a changed request.
func migratedPurchaseDepositReplays(t *testing.T) {
	w := newWorld(t)
	customer := billing.CustomerID(uuid.New())
	opening, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: customer, Currency: "USD", Amount: 1000, Source: "critical", SourceID: "opening"}})
	require.NoError(t, err)
	migrated := uuid.New()
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{w.schema, "credit_grants"}.Sanitize()+
		" (merchant_id, id, customer_id, account_id, unit, units, funding_source, paid_amount, paid_currency, source, source_id, invoker, starts_at)"+
		" SELECT merchant_id, $3, customer_id, account_id, unit, 2000, 'purchase', 2000, 'USD', 'pack_purchase', 'pack-42', 'host', starts_at"+
		" FROM "+pgx.Identifier{w.schema, "credit_grants"}.Sanitize()+" WHERE merchant_id = $1 AND id = $2",
		w.alpha.MerchantID().UUID(), opening[0].ID.UUID(), migrated)
	require.NoError(t, err)
	retry := billing.CreateCreditGrantParams{CustomerID: customer, Currency: "USD", Amount: 2000, Source: "pack_purchase", SourceID: "pack-42", Invoker: "host"}
	replayed, err := w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{retry})
	require.NoError(t, err)
	require.Len(t, replayed, 1)
	require.True(t, replayed[0].Replayed)
	require.Equal(t, migrated, replayed[0].ID.UUID())
	require.Equal(t, billing.CreditFundingPurchase, replayed[0].FundingSource)
	retry.FundingSource = billing.CreditGrantFundingGrant
	_, err = w.alpha.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{retry})
	require.ErrorIs(t, err, billing.ErrIdempotencyKeyReused)
}
