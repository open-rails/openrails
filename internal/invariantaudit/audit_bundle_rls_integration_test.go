//go:build integration

package invariantaudit

import (
	"context"
	"testing"

	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// globalTables are deliberate deployment-wide objects without merchant_id.
var globalTables = map[string]string{
	"merchants":                 "global merchant directory — the thing merchant_id points at",
	"worker_state":              "deployment-wide worker liveness and capped-sweep resume points",
	"destructive_action_switch": "or#836 kill switch — must be readable with no merchant context",
}

// pools supplies owner and separately provisioned normal-login connections.
// Tenant correctness must be identical for both.
func pools(t *testing.T) (ctx context.Context, super, app *pgxpool.Pool) {
	t.Helper()
	ctx = context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	super, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	t.Cleanup(super.Close)

	app, err = pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	t.Cleanup(app.Close)

	return ctx, super, app
}

// TEN-1 / TEN-3: every non-global table has a merchant column; no RLS
// policy or flag may conceal a missing application predicate in these tests.
func TestTEN1_ExplicitScopeSchemaWithoutRLS(t *testing.T) {
	ctx, _, app := pools(t)
	rows, err := app.Query(ctx, `
        SELECT c.relname, c.relrowsecurity OR c.relforcerowsecurity,
            EXISTS(SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid),
            EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attname='merchant_id' AND NOT a.attisdropped)
        FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
        WHERE n.nspname='billing' AND c.relkind='r' ORDER BY c.relname`)
	require.NoError(t, err)
	defer rows.Close()
	count := 0
	globals := map[string]string{}
	for rows.Next() {
		var name string
		var flags, policies, merchantColumn bool
		require.NoError(t, rows.Scan(&name, &flags, &policies, &merchantColumn))
		require.False(t, flags, name)
		require.False(t, policies, name)
		if reason, global := globalTables[name]; global {
			globals[name] = reason
		} else {
			require.True(t, merchantColumn, name)
		}
		count++
	}
	require.NoError(t, rows.Err())
	require.Greater(t, count, len(globalTables), "schema assertion must not be vacuous")
	require.Equal(t, globalTables, globals)
}

// TEN-2: actual tenant SQL rejects foreign and missing scope even on the owner
// connection. A GUC is not a substitute for the query's explicit parameter.
func TestTEN2_QueryScopeIndependentOfDatabaseRole(t *testing.T) {
	ctx, owner, normal := pools(t)
	a, b, productID := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b} {
		_, err := owner.Exec(ctx, `INSERT INTO billing.merchants(id,slug) VALUES($1,$2)`, id, "scope-"+id.String())
		require.NoError(t, err)
	}
	_, err := owner.Exec(ctx, `INSERT INTO billing.products(id,merchant_id,key,display_name) VALUES($1,$2,'private-product','Original')`, productID, b)
	require.NoError(t, err)
	for name, pool := range map[string]*pgxpool.Pool{"owner": owner, "normal": normal} {
		t.Run(name, func(t *testing.T) {
			q := dbtest.Queries(pool)
			for _, scope := range []uuid.UUID{uuid.Nil, a} {
				_, err := q.GetProductByID(ctx, gen.GetProductByIDParams{ID: productID, MerchantID: scope})
				require.ErrorIs(t, err, pgx.ErrNoRows)
				title := "Forged"
				_, err = q.PatchProduct(ctx, gen.PatchProductParams{ID: productID, MerchantID: scope, DisplayName: &title})
				require.ErrorIs(t, err, pgx.ErrNoRows)
			}
			own, err := q.GetProductByID(ctx, gen.GetProductByIDParams{ID: productID, MerchantID: b})
			require.NoError(t, err)
			require.Equal(t, "Original", own.DisplayName)
		})
	}
}

// LED-5: normal runtime grants stay narrow. Owner-compatible DML guards are
// exercised separately by TestOwningLoginInitializesWithoutRLSAndPreservesFinancialFacts.
func TestLED5_OptionalRuntimeGrantsKeepLedgerAppendOnly(t *testing.T) {
	ctx, _, app := pools(t)
	for _, table := range []string{"ledger_transfers", "ledger_accounts"} {
		rows, err := app.Query(ctx, `SELECT privilege_type FROM information_schema.table_privileges
            WHERE grantee=current_user AND table_schema='billing' AND table_name=$1 ORDER BY 1`, table)
		require.NoError(t, err)
		var privileges []string
		for rows.Next() {
			var privilege string
			require.NoError(t, rows.Scan(&privilege))
			privileges = append(privileges, privilege)
		}
		rows.Close()
		require.NoError(t, rows.Err())
		require.ElementsMatch(t, []string{"INSERT", "SELECT"}, privileges)
	}
}

// ledgerFixture seeds a merchant plus two same-currency accounts and returns a
// merchant-pinned transaction using a normal runtime login.
func ledgerFixture(t *testing.T, ctx context.Context, super, app *pgxpool.Pool, currency string, floorOnDebit bool) (tx pgx.Tx, debit, credit, merchantID uuid.UUID) {
	t.Helper()
	merchantID = uuid.New()
	_, err := super.Exec(ctx,
		`INSERT INTO billing.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		merchantID, "inv-led-"+uuid.NewString()[:8])
	require.NoError(t, err)

	require.NoError(t, super.QueryRow(ctx,
		`INSERT INTO billing.ledger_accounts (merchant_id, account_type, currency, debits_must_not_exceed_credits)
		 VALUES ($1,'customer_balance',$2,$3) RETURNING id`, merchantID, currency, floorOnDebit).Scan(&debit))
	require.NoError(t, super.QueryRow(ctx,
		`INSERT INTO billing.ledger_accounts (merchant_id, account_type, currency)
		 VALUES ($1,'platform_revenue',$2) RETURNING id`, merchantID, currency).Scan(&credit))

	pgtx, err := app.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pgtx.Rollback(context.Background()) })
	_, err = pgtx.Exec(ctx, `SELECT set_config('app.merchant_id', $1::text, true)`, merchantID.String())
	require.NoError(t, err)
	return pgtx, debit, credit, merchantID
}

// attemptInTx runs one statement inside a SAVEPOINT so an EXPECTED error does
// not poison the surrounding transaction.
//
// Postgres aborts a transaction on ANY error, so a guard that asserts "this
// must raise" and then asserts "this must be allowed" cannot pass without
// isolating the first: the second statement returns 25P02 (transaction
// aborted) whatever the schema does. Both ledger guards below were failing
// that way — permanently red, and therefore about to be ignored, which is the
// same failure mode as a guard that can never fail.
func attemptInTx(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	if _, err := tx.Exec(ctx, "SAVEPOINT audit_probe"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		_, _ = tx.Exec(ctx, "ROLLBACK TO SAVEPOINT audit_probe")
		return err
	}
	_, _ = tx.Exec(ctx, "RELEASE SAVEPOINT audit_probe")
	return nil
}

// MONEY-8 + LED-6: amount > 0, distinct accounts, non-negative floor.
func TestMONEY8_TransferAmountAndFloorChecks(t *testing.T) {
	ctx, super, app := pools(t)
	tx, debit, credit, merchantID := ledgerFixture(t, ctx, super, app, "USD", false)

	insert := func(amount int64, d, c uuid.UUID, floor int64) error {
		return attemptInTx(ctx, tx,
			`INSERT INTO billing.ledger_transfers
			   (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, allow_debit_negative_up_to,
			    operation, source, source_id)
			 VALUES ($1,$2,$3,$4,'USD','credit_spend',$5,'spend','audit',gen_random_uuid()::text)`, merchantID, d, c, amount, floor)
	}
	require.Error(t, insert(0, debit, credit, 0), "MONEY-8: amount = 0 must be rejected")
	require.Error(t, insert(-1, debit, credit, 0), "MONEY-8: negative amount must be rejected")
	require.Error(t, insert(100, debit, debit, 0), "LED-6: debit == credit must be rejected")
	require.Error(t, insert(100, debit, credit, -1), "MONEY-8: negative debit floor must be rejected")
	require.NoError(t, insert(100, debit, credit, 0), "a well-formed transfer must be accepted")
}

// CUR-3: FX inside the ledger raises. Verified through the trigger, on the app
// role, with the GUC set — i.e. the exact path a real transfer takes.
func TestCUR3_CrossCurrencyTransferRaises(t *testing.T) {
	ctx, super, app := pools(t)
	tx, debit, _, merchantID := ledgerFixture(t, ctx, super, app, "USD", false)

	var eurCredit uuid.UUID
	require.NoError(t, super.QueryRow(ctx,
		`INSERT INTO billing.ledger_accounts (merchant_id, account_type, currency)
		 VALUES ($1,'processor_clearing','EUR') RETURNING id`, merchantID).Scan(&eurCredit))

	// transfer_type is 'credit_spend' (a real GAP-7 vocabulary value) rather
	// than the 'capture' this used to send. 'capture' also violates
	// ledger_transfers_type_check, so the row had TWO reasons to be rejected;
	// the assertion below stayed honest only because a BEFORE INSERT trigger
	// fires ahead of CHECK constraints. Relying on that ordering to keep a
	// guard pointed at the right failure is luck, not design.
	err := attemptInTx(ctx, tx,
		`INSERT INTO billing.ledger_transfers
		   (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type,
		    operation, source, source_id)
		 VALUES ($1,$2,$3,100,'USD','credit_spend','spend','audit',gen_random_uuid()::text)`, merchantID, debit, eurCredit)
	require.ErrorContains(t, err, "cross-currency transfer",
		"CUR-3: a transfer whose account currency differs from the transfer currency must raise")
}

// LED-2: a missing account raises rather than silently no-opping. Under RLS this
// is doubly important — a wrong-merchant account id is INVISIBLE, so "not found"
// is the only safe answer.
func TestLED2_MissingAccountRaises(t *testing.T) {
	ctx, super, app := pools(t)
	tx, debit, _, merchantID := ledgerFixture(t, ctx, super, app, "USD", false)

	_, err := tx.Exec(ctx,
		`INSERT INTO billing.ledger_transfers
		   (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type,
		    operation, source, source_id)
		 VALUES ($1,$2,$3,100,'USD','credit_spend','spend','audit',gen_random_uuid()::text)`, merchantID, debit, uuid.New())
	require.Error(t, err, "LED-2: an unknown credit account must raise, never silently apply")
}

// LED-3: the insufficient-funds floor.
func TestLED3_InsufficientFundsFloorRaises(t *testing.T) {
	ctx, super, app := pools(t)
	tx, debit, credit, merchantID := ledgerFixture(t, ctx, super, app, "USD", true)

	err := attemptInTx(ctx, tx,
		`INSERT INTO billing.ledger_transfers
		   (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, allow_debit_negative_up_to,
		    operation, source, source_id)
		 VALUES ($1,$2,$3,500,'USD','credit_spend',0,'spend','audit',gen_random_uuid()::text)`, merchantID, debit, credit)
	require.ErrorContains(t, err, "ledger_insufficient_funds",
		"LED-3: debiting an empty balance-floored account must raise")

	// …and the arrears allowance is honoured, so the floor is a real threshold
	// rather than a blanket refusal.
	err = attemptInTx(ctx, tx,
		`INSERT INTO billing.ledger_transfers
		   (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, allow_debit_negative_up_to,
		    operation, source, source_id)
		 VALUES ($1,$2,$3,500,'USD','owed_accrual',500,'arrears_accrual','audit',gen_random_uuid()::text)`, merchantID, debit, credit)
	require.NoError(t, err, "LED-3: a debit within the declared floor must be allowed")
}

// LED-7: a credit lot deposits/expires/revokes at most once.
func TestLED7_CreditLotTerminatesOnce(t *testing.T) {
	ctx, super, app := pools(t)
	tx, debit, credit, merchantID := ledgerFixture(t, ctx, super, app, "USD", false)
	lot := uuid.New()

	dep := func() error {
		_, err := tx.Exec(ctx,
			`INSERT INTO billing.ledger_transfers
			   (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, grant_id,
			    operation, source, source_id)
			 VALUES ($1,$2,$3,100,'USD','deposit',$4,'deposit','audit',gen_random_uuid()::text)`, merchantID, debit, credit, lot)
		return err
	}
	require.NoError(t, dep())
	require.Error(t, dep(), "LED-7: a second deposit for the same lot must be rejected")
}

// GAP-7 / #832: transfer_type is a closed vocabulary. Without the CHECK, a typo
// slips past idx_ledger_transfers_lot_once (which keys on the literal string).
func TestGAP7_TransferTypeIsConstrained(t *testing.T) {
	ctx, super, app := pools(t)
	tx, debit, credit, merchantID := ledgerFixture(t, ctx, super, app, "USD", false)

	_, err := tx.Exec(ctx,
		`INSERT INTO billing.ledger_transfers
		   (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type,
		    operation, source, source_id)
		 VALUES ($1,$2,$3,100,'USD','depsoit','deposit','audit',gen_random_uuid()::text)`, merchantID, debit, credit)
	require.Error(t, err, "GAP-7: an unknown transfer_type must be rejected by a CHECK")
}

// CUR-1: exactly one currency column may be NULL-able, and it is grants'.
func TestCUR1_CurrencyColumnsAreNotNull(t *testing.T) {
	ctx, _, app := pools(t)
	rows, err := app.Query(ctx, `
		SELECT table_name FROM information_schema.columns
		 WHERE column_name = 'currency' AND table_schema = 'billing' AND is_nullable = 'YES'
		 ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()
	var nullable []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		nullable = append(nullable, n)
	}
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"checkout_sessions", "grants"}, nullable,
		"CUR-1: currency is required except nonmonetary checkout setup and grants; each has a conditional schema constraint")
}

// GAP-10: a UNIQUE index that omits merchant_id lets one merchant block
// another's insert — the only cross-merchant coupling the schema can have.
func TestGAP10_UniqueIndexesAreMerchantScoped(t *testing.T) {
	ctx, _, app := pools(t)

	// Tables with no merchant_id at all cannot scope by it; and the exempt
	// directory tables are global on purpose.
	rows, err := app.Query(ctx, `
		SELECT i.tablename, i.indexname, i.indexdef
		  FROM pg_indexes i
		 WHERE i.schemaname = 'billing'
		   AND i.indexdef LIKE '%UNIQUE%'
		   AND i.indexdef NOT LIKE '%merchant_id%'
		   AND EXISTS (SELECT 1 FROM information_schema.columns c
		                WHERE c.table_schema='billing' AND c.table_name=i.tablename
		                  AND c.column_name='merchant_id')
		 ORDER BY 1,2`)
	require.NoError(t, err)
	defer rows.Close()
	var offenders []string
	seen := 0
	for rows.Next() {
		var tbl, idx, def string
		require.NoError(t, rows.Scan(&tbl, &idx, &def))
		seen++
		// ONE exemption list, shared with the migration-text guard
		// (internal/migrate/postgres/unique_scope_exemptions.go). A surrogate-id
		// primary key is not a tenancy statement; every other exception is
		// named there with a reason.
		if postgresmigrations.UniqueScopeExemptDef(idx, def) {
			continue
		}
		offenders = append(offenders, tbl+"."+idx+": "+def)
	}
	require.NoError(t, rows.Err())
	// Vacuity guard: if the query stops returning rows the check is a no-op.
	require.GreaterOrEqual(t, seen, 40,
		"pg_indexes returned only %d unique indexes lacking merchant_id: the query is broken and this guard would pass vacuously", seen)
	require.Emptyf(t, offenders,
		"ID-11 (was GAP-10): unique index on a merchant-owned table omits merchant_id — one merchant can block another:\n%v", offenders)
}

// ID-11 in the flesh (or#902). TestGAP10 above is a census; this is the one
// index it used to EXEMPT, checked by inserting rows rather than by reading a
// catalogue.
//
// 0001 shipped destructive_run_before_images' identity unique as
// (destructive_run_id, table_name, row_id) — a key spanning merchants on an
// RLS-FORCED table, which is the exact ID-11 hazard: the conflicting row is
// invisible to the inserting session, so the victim gets a unique violation
// naming a row it cannot select. 0003 leads the key with merchant_id.
//
// The merchant-led unique is necessary but not sufficient: the before-image's
// run reference must carry the same merchant identity so a row cannot cite a
// different merchant's destructive run through a globally unique run ID.
func TestID11_BeforeImagesIdentityUniqueIsMerchantLed(t *testing.T) {
	ctx, super, app := pools(t)

	var def string
	require.NoError(t, app.QueryRow(ctx, `
		SELECT indexdef FROM pg_indexes
		 WHERE schemaname = 'billing'
		   AND tablename  = 'destructive_run_before_images'
		   AND indexname  = 'uq_destructive_run_before_images_identity'`).Scan(&def),
		"the or#859 undo-evidence identity index is missing entirely")
	require.Contains(t, def, "(merchant_id, destructive_run_id, table_name, row_id)",
		"ID-11: the before-images identity unique must LEAD with merchant_id, got: %s", def)

	var fkDef string
	require.NoError(t, super.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE connamespace = 'billing'::regnamespace
		   AND conrelid = 'billing.destructive_run_before_images'::regclass
		   AND conname = 'destructive_run_before_images_run_fk'`).Scan(&fkDef),
		"the before-image-to-run foreign key is missing entirely")
	require.Contains(t, fkDef,
		"FOREIGN KEY (merchant_id, destructive_run_id, destructive_run_class) REFERENCES billing.maintenance_runs(merchant_id, id, run_class)",
		"ID-11: the run foreign key must carry merchant identity, got: %s", fkDef)

	a, b := uuid.New(), uuid.New()
	for id, slug := range map[uuid.UUID]string{
		a: "inv-bi-a-" + uuid.NewString()[:8],
		b: "inv-bi-b-" + uuid.NewString()[:8],
	} {
		_, err := super.Exec(ctx,
			`INSERT INTO billing.merchants (id, slug, status) VALUES ($1, $2, 'active')`, id, slug)
		require.NoError(t, err)
	}

	runID := uuid.New()
	_, err := super.Exec(ctx,
		`INSERT INTO billing.maintenance_runs (id, merchant_id, kind, actor)
		 VALUES ($1, $2, 'converge_enforce', 'or902-invariant-audit')`, runID, a)
	require.NoError(t, err)

	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1::text, true)`, b.String())
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		INSERT INTO billing.destructive_run_before_images
		    (merchant_id, destructive_run_id, table_name, row_id, before)
		VALUES ($1, $2, 'subscriptions', $3, '{}'::jsonb)`, b, runID, uuid.New())
	require.Error(t, err,
		"a before-image must not cite another merchant's destructive run")
}

// GAP-9: org ↔ merchant is 1:1. Enforced on the policy-free merchants
// directory, so this read is legitimate without a GUC.
func TestGAP9_PermissionGroupIsUniquePerMerchant(t *testing.T) {
	ctx, _, app := pools(t)
	var dupes int64
	require.NoError(t, app.QueryRow(ctx, `
		SELECT count(*) FROM (
		  SELECT permission_group_id FROM billing.merchants
		   WHERE permission_group_id IS NOT NULL
		   GROUP BY 1 HAVING count(*) > 1) d`).Scan(&dupes))
	require.EqualValues(t, 0, dupes, "GAP-9: two merchants share one permission group")

	// Probe the invariant itself. The unbound-name index also mentions
	// permission_group_id in its predicate, so index-definition substring
	// counting cannot identify the group-ownership key.
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	owner, group := uuid.New(), uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO billing.merchants(id,slug,permission_group_id) VALUES($1,$2,$3)`, owner, "group-owner-"+owner.String(), group)
	require.NoError(t, err)
	for _, deleted := range []bool{false, true} {
		if deleted {
			_, err = tx.Exec(ctx, `UPDATE billing.merchants SET status='deleted',deleted_at=now() WHERE id=$1`, owner)
			require.NoError(t, err)
		}
		next := uuid.New()
		err = attemptInTx(ctx, tx, `INSERT INTO billing.merchants(id,slug,permission_group_id) VALUES($1,$2,$3)`, next, "group-duplicate-"+next.String(), group)
		var violation *pgconn.PgError
		require.ErrorAs(t, err, &violation, "GAP-9: a group cannot acquire another billing identity (old owner deleted=%v)", deleted)
		require.Equal(t, "23505", violation.Code, "group ownership must remain unique after deletion")
	}
}
