//go:build integration

package invariantaudit

import (
	"context"
	"testing"

	postgresmigrations "github.com/open-rails/openrails/migrations/postgres"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

// exemptTables are the tables deliberately NOT under RLS (TEN-3, plus the
// or#836 kill switch which must be readable before any merchant is resolved).
var exemptTables = map[string]string{
	"merchants":                 "global merchant directory — the thing merchant_id points at",
	"probe_verdicts":            "deployment-wide probe results",
	"worker_health":             "deployment-wide worker liveness",
	"destructive_action_switch": "or#836 kill switch — must be readable with no merchant context",
	"worker_sweep_cursors":      "or#837 capped-sweep resume points — operator-global process state, no tenant data",
}

// pools opens a super pool (seeding) and an app-role pool (assertions), and
// PROVES the app pool is the production role: not superuser, not BYPASSRLS.
// Every DB-facing assertion below runs on appPool.
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

	var isSuper, bypass bool
	require.NoError(t, app.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&isSuper, &bypass))
	require.False(t, isSuper, "audit harness must NOT run as superuser — that is what hid or#824")
	require.False(t, bypass, "audit harness must NOT run as a BYPASSRLS role (TEN-9)")
	return ctx, super, app
}

// TEN-1 / TEN-3: every table in the app schema has RLS enabled, except the
// documented exempt set — and the exempt set is exactly what we expect, in both
// directions (a new unpoliced table fails here; so does removing an exemption
// without updating the register).
func TestTEN1_AllTablesUnderRLSExceptDocumentedExemptions(t *testing.T) {
	ctx, _, app := pools(t)

	rows, err := app.Query(ctx, `
		SELECT c.relname FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'openrails' AND c.relkind = 'r' AND NOT c.relrowsecurity
		 ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()

	var unpoliced []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		unpoliced = append(unpoliced, name)
	}
	require.NoError(t, rows.Err())

	for _, name := range unpoliced {
		_, ok := exemptTables[name]
		require.Truef(t, ok,
			"table openrails.%s has NO row level security and is not a documented TEN-3 exemption. "+
				"Either add ENABLE+FORCE RLS with a merchant_isolation policy, or document the exemption "+
				"in docs/invariants.md TEN-3 and add it to exemptTables here.", name)
	}
	require.Len(t, unpoliced, len(exemptTables),
		"the TEN-3 exempt set drifted: got %v, register says %v", unpoliced, keys(exemptTables))
}

// TEN-1 (second half): ENABLE alone is not enough. A table owner escapes its own
// policies unless FORCE is set, and a policy without WITH CHECK lets a merchant
// WRITE rows it cannot read. Both are silent failures, so assert both.
func TestTEN1_PoliciedTablesForceRLSAndCheckWrites(t *testing.T) {
	ctx, _, app := pools(t)

	rows, err := app.Query(ctx, `
		SELECT c.relname, c.relforcerowsecurity,
		       (SELECT count(*) FROM pg_policy p
		         WHERE p.polrelid = c.oid AND p.polname = 'merchant_isolation') AS pol,
		       (SELECT count(*) FROM pg_policy p
		         WHERE p.polrelid = c.oid AND p.polname = 'merchant_isolation'
		           AND p.polqual IS NOT NULL AND p.polwithcheck IS NOT NULL) AS both_clauses
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'openrails' AND c.relkind = 'r' AND c.relrowsecurity
		 ORDER BY 1`)
	require.NoError(t, err)
	defer rows.Close()

	var noForce, noPolicy, halfPolicy []string
	for rows.Next() {
		var name string
		var force bool
		var pol, both int
		require.NoError(t, rows.Scan(&name, &force, &pol, &both))
		if !force {
			noForce = append(noForce, name)
		}
		switch {
		case pol == 0:
			noPolicy = append(noPolicy, name)
		case both == 0:
			halfPolicy = append(halfPolicy, name)
		}
	}
	require.NoError(t, rows.Err())

	require.Empty(t, noForce, "RLS enabled but not FORCEd — the table owner escapes its own isolation (TEN-1)")
	require.Empty(t, noPolicy, "RLS enabled with no merchant_isolation policy (TEN-1)")
	require.Empty(t, halfPolicy, "merchant_isolation policy missing USING or WITH CHECK — writes go unchecked (TEN-1)")
}

// TEN-2 / FC-1: no GUC ⇒ zero rows, no error. This is the fail-closed guarantee
// AND the trap: it is exactly why or#860's rate ceiling counts 0 forever. Pin
// both halves — the isolation, and the silence.
func TestTEN2_UnsetGUCYieldsZeroRowsAndNoError(t *testing.T) {
	ctx, super, app := pools(t)

	merchantID := uuid.New()
	slug := "inv-ten2-" + uuid.NewString()[:8]
	_, err := super.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, merchantID, slug)
	require.NoError(t, err)
	_, err = super.Exec(ctx,
		`INSERT INTO openrails.customers (merchant_id, subject) VALUES ($1, $2)`, merchantID, uuid.NewString())
	require.NoError(t, err)

	// No GUC on this connection.
	var n int64
	require.NoError(t, app.QueryRow(ctx,
		`SELECT count(*) FROM openrails.customers WHERE merchant_id = $1`, merchantID).Scan(&n),
		"a GUC-less read must NOT error — it silently returns nothing, which is the whole hazard")
	require.EqualValues(t, 0, n,
		"TEN-2: unset app.merchant_id must yield zero rows")

	// Superuser sees the row that the app role cannot: proves the seed is real
	// and the emptiness above is RLS, not a missing fixture.
	require.NoError(t, super.QueryRow(ctx,
		`SELECT count(*) FROM openrails.customers WHERE merchant_id = $1`, merchantID).Scan(&n))
	require.EqualValues(t, 1, n)

	// With the GUC set transaction-locally, the same read answers.
	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1::text, true)`, merchantID.String())
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT count(*) FROM openrails.customers WHERE merchant_id = $1`, merchantID).Scan(&n))
	require.EqualValues(t, 1, n, "GUC-scoped read must see the merchant's own row")
}

// TEN-2, cross-merchant: a GUC pinned to merchant A must not see merchant B,
// on read OR on write (WITH CHECK).
func TestTEN2_CrossMerchantReadAndWriteBlocked(t *testing.T) {
	ctx, super, app := pools(t)

	a, b := uuid.New(), uuid.New()
	for id, slug := range map[uuid.UUID]string{a: "inv-a-" + uuid.NewString()[:8], b: "inv-b-" + uuid.NewString()[:8]} {
		_, err := super.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, id, slug)
		require.NoError(t, err)
	}
	_, err := super.Exec(ctx,
		`INSERT INTO openrails.customers (merchant_id, subject) VALUES ($1, $2)`, b, uuid.NewString())
	require.NoError(t, err)

	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1::text, true)`, a.String())
	require.NoError(t, err)

	var n int64
	require.NoError(t, tx.QueryRow(ctx,
		`SELECT count(*) FROM openrails.customers WHERE merchant_id = $1`, b).Scan(&n))
	require.EqualValues(t, 0, n, "merchant A read merchant B's customers")

	_, err = tx.Exec(ctx,
		`INSERT INTO openrails.customers (merchant_id, subject) VALUES ($1, $2)`, b, uuid.NewString())
	require.Error(t, err, "WITH CHECK must reject writing a row into another merchant's scope")
}

// TEN-9: the role the whole register depends on.
func TestTEN9_AppRoleIsNotSuperAndNotBypassRLS(t *testing.T) {
	ctx, _, app := pools(t)
	var isSuper, bypass, canLogin bool
	require.NoError(t, app.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls, rolcanlogin FROM pg_roles WHERE rolname = 'openrails_app'`).
		Scan(&isSuper, &bypass, &canLogin))
	require.False(t, isSuper)
	require.False(t, bypass)
	_ = canLogin // tests grant LOGIN; production wires credentials out of band.
}

// LED-5: the ledger is append-only by ROLE PRIVILEGE, not by convention. If the
// app role ever gains UPDATE or DELETE on either ledger table, the "S" grade in
// the register is a lie.
func TestLED5_LedgerIsAppendOnlyByPrivilege(t *testing.T) {
	ctx, _, app := pools(t)

	for _, table := range []string{"ledger_transfers", "ledger_accounts"} {
		rows, err := app.Query(ctx, `
			SELECT privilege_type FROM information_schema.table_privileges
			 WHERE grantee = 'openrails_app' AND table_schema = 'openrails' AND table_name = $1
			 ORDER BY 1`, table)
		require.NoError(t, err)
		var privs []string
		for rows.Next() {
			var p string
			require.NoError(t, rows.Scan(&p))
			privs = append(privs, p)
		}
		rows.Close()
		require.NoError(t, rows.Err())
		require.ElementsMatch(t, []string{"INSERT", "SELECT"}, privs,
			"LED-5: %s must grant SELECT,INSERT only — got %v", table, privs)
	}
}

// ledgerFixture seeds a merchant plus two same-currency accounts and returns a
// GUC-pinned tx on the APP role. Everything the ledger tests assert therefore
// runs through the same policy path production uses.
func ledgerFixture(t *testing.T, ctx context.Context, super, app *pgxpool.Pool, currency string, floorOnDebit bool) (tx pgx.Tx, debit, credit, merchantID uuid.UUID) {
	t.Helper()
	merchantID = uuid.New()
	_, err := super.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		merchantID, "inv-led-"+uuid.NewString()[:8])
	require.NoError(t, err)

	require.NoError(t, super.QueryRow(ctx,
		`INSERT INTO openrails.ledger_accounts (merchant_id, account_type, currency, debits_must_not_exceed_credits)
		 VALUES ($1,'customer_balance',$2,$3) RETURNING id`, merchantID, currency, floorOnDebit).Scan(&debit))
	require.NoError(t, super.QueryRow(ctx,
		`INSERT INTO openrails.ledger_accounts (merchant_id, account_type, currency)
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
			`INSERT INTO openrails.ledger_transfers
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
		`INSERT INTO openrails.ledger_accounts (merchant_id, account_type, currency)
		 VALUES ($1,'processor_clearing','EUR') RETURNING id`, merchantID).Scan(&eurCredit))

	// transfer_type is 'credit_spend' (a real GAP-7 vocabulary value) rather
	// than the 'capture' this used to send. 'capture' also violates
	// ledger_transfers_type_check, so the row had TWO reasons to be rejected;
	// the assertion below stayed honest only because a BEFORE INSERT trigger
	// fires ahead of CHECK constraints. Relying on that ordering to keep a
	// guard pointed at the right failure is luck, not design.
	err := attemptInTx(ctx, tx,
		`INSERT INTO openrails.ledger_transfers
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
		`INSERT INTO openrails.ledger_transfers
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
		`INSERT INTO openrails.ledger_transfers
		   (merchant_id, debit_account_id, credit_account_id, amount, currency, transfer_type, allow_debit_negative_up_to,
		    operation, source, source_id)
		 VALUES ($1,$2,$3,500,'USD','credit_spend',0,'spend','audit',gen_random_uuid()::text)`, merchantID, debit, credit)
	require.ErrorContains(t, err, "ledger_insufficient_funds",
		"LED-3: debiting an empty balance-floored account must raise")

	// …and the arrears allowance is honoured, so the floor is a real threshold
	// rather than a blanket refusal.
	err = attemptInTx(ctx, tx,
		`INSERT INTO openrails.ledger_transfers
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
			`INSERT INTO openrails.ledger_transfers
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
		`INSERT INTO openrails.ledger_transfers
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
		 WHERE column_name = 'currency' AND table_schema = 'openrails' AND is_nullable = 'YES'
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
	require.Equal(t, []string{"grants"}, nullable,
		"CUR-1: only grants.currency may be nullable (CUR-2 makes it conditionally required)")
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
		 WHERE i.schemaname = 'openrails'
		   AND i.indexdef LIKE '%UNIQUE%'
		   AND i.indexdef NOT LIKE '%merchant_id%'
		   AND EXISTS (SELECT 1 FROM information_schema.columns c
		                WHERE c.table_schema='openrails' AND c.table_name=i.tablename
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
		// (migrations/postgres/unique_scope_exemptions.go). A surrogate-id
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
		 WHERE schemaname = 'openrails'
		   AND tablename  = 'destructive_run_before_images'
		   AND indexname  = 'uq_destructive_run_before_images_identity'`).Scan(&def),
		"the or#859 undo-evidence identity index is missing entirely")
	require.Contains(t, def, "(merchant_id, destructive_run_id, table_name, row_id)",
		"ID-11: the before-images identity unique must LEAD with merchant_id, got: %s", def)

	var fkDef string
	require.NoError(t, super.QueryRow(ctx, `
		SELECT pg_get_constraintdef(oid)
		  FROM pg_constraint
		 WHERE connamespace = 'openrails'::regnamespace
		   AND conrelid = 'openrails.destructive_run_before_images'::regclass
		   AND conname = 'destructive_run_before_images_run_fk'`).Scan(&fkDef),
		"the before-image-to-run foreign key is missing entirely")
	require.Contains(t, fkDef,
		"FOREIGN KEY (merchant_id, destructive_run_id) REFERENCES openrails.destructive_runs(merchant_id, id)",
		"ID-11: the run foreign key must carry merchant identity, got: %s", fkDef)

	a, b := uuid.New(), uuid.New()
	for id, slug := range map[uuid.UUID]string{
		a: "inv-bi-a-" + uuid.NewString()[:8],
		b: "inv-bi-b-" + uuid.NewString()[:8],
	} {
		_, err := super.Exec(ctx,
			`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`, id, slug)
		require.NoError(t, err)
	}

	runID := uuid.New()
	_, err := super.Exec(ctx,
		`INSERT INTO openrails.destructive_runs (id, merchant_id, kind, actor)
		 VALUES ($1, $2, 'converge_enforce', 'or902-invariant-audit')`, runID, a)
	require.NoError(t, err)

	tx, err := app.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1::text, true)`, b.String())
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		INSERT INTO openrails.destructive_run_before_images
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
		  SELECT permission_group_id FROM openrails.merchants
		   WHERE permission_group_id IS NOT NULL
		   GROUP BY 1 HAVING count(*) > 1) d`).Scan(&dupes))
	require.EqualValues(t, 0, dupes, "GAP-9: two merchants share one permission group")

	var idx int64
	require.NoError(t, app.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		 WHERE schemaname='openrails' AND tablename='merchants'
		   AND indexdef LIKE '%UNIQUE%' AND indexdef LIKE '%permission_group_id%'`).Scan(&idx))
	require.EqualValues(t, 1, idx, "GAP-9: the unique index on merchants.permission_group_id is missing")
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
