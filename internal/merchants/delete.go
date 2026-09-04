package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DestructiveRunKindMerchantPurge is the merchant purge's kind in the general
// openrails.destructive_runs ledger (or#859 §5.1 — the same ledger --prune
// writes, so one query answers "what did this deployment destroy, and who
// asked for it").
const DestructiveRunKindMerchantPurge = "merchant_purge"

// merchantOwnedTables are the openrails.* tables that carry the persisted
// merchant_id column, IN PURGE ORDER: children before the rows they reference.
//
// The order is load-bearing, not cosmetic. prices→products, subscriptions→prices,
// checkout_sessions→payments and rail_mutation_logs→rail_intents are all
// RESTRICT or NO ACTION, so the previous alphabetical-ish order aborted the
// whole purge (SQLSTATE 23001) for any merchant that owned a product with a
// price — i.e. every real one. Nothing caught it because the only test seeded a
// single entitlement row.
//
// Kept in sync with queries/merchant_lifecycle.sql + the dispatch switches
// below (#334: each table has a STATIC generated count/purge query — no runtime
// SQL assembly). existingMerchantTables preserves this order.
var merchantOwnedTables = []string{
	"notification_queue", "catalog_drift_events",
	"rail_mutation_logs", "rail_intents",
	"checkout_sessions", "entitlements", "payments", "subscriptions",
	"money_settings", "payment_methods", "rail_customer_accounts",
	"prices", "products",
	// money ledger (#512 hard cut): the single-entry money_blocks/money_transactions
	// tables are gone. The append-only ledger_transfers/grants are immutable
	// (REVOKE DELETE) and intentionally NOT row-purged here — which is also why a
	// merchant whose grants pin payments/products cannot be purged at all; see
	// ErrPurgeBlockedByRetainedHistory.
}

// countMerchantRows dispatches to the table's generated count query.
func countMerchantRows(ctx context.Context, q *gen.Queries, table string, id uuid.UUID) (int64, error) {
	switch table {
	case "products":
		return q.CountMerchantRowsProducts(ctx, id)
	case "prices":
		return q.CountMerchantRowsPrices(ctx, id)
	case "catalog_drift_events":
		return q.CountMerchantRowsCatalogDriftEvents(ctx, id)
	case "payment_methods":
		return q.CountMerchantRowsPaymentMethods(ctx, id)
	case "subscriptions":
		return q.CountMerchantRowsSubscriptions(ctx, id)
	case "entitlements":
		return q.CountMerchantRowsEntitlements(ctx, id)
	case "payments":
		return q.CountMerchantRowsPayments(ctx, id)
	case "notification_queue":
		return q.CountMerchantRowsNotificationQueue(ctx, id)
	case "rail_customer_accounts":
		return q.CountMerchantRowsRailCustomers(ctx, id)
	case "checkout_sessions":
		return q.CountMerchantRowsCheckoutSessions(ctx, id)
	case "rail_mutation_logs":
		return q.CountMerchantRowsExternalProviderMutationLogs(ctx, id)
	case "rail_intents":
		return q.CountMerchantRowsProviderIntents(ctx, id)
	case "money_settings":
		return q.CountMerchantRowsMoneyAccounts(ctx, id)
	default:
		return 0, fmt.Errorf("merchants: no count query for table %q", table)
	}
}

// purgeMerchantRows dispatches to the table's generated purge query.
func purgeMerchantRows(ctx context.Context, q *gen.Queries, table string, id uuid.UUID) error {
	switch table {
	case "products":
		return q.PurgeMerchantRowsProducts(ctx, id)
	case "prices":
		return q.PurgeMerchantRowsPrices(ctx, id)
	case "catalog_drift_events":
		return q.PurgeMerchantRowsCatalogDriftEvents(ctx, id)
	case "payment_methods":
		return q.PurgeMerchantRowsPaymentMethods(ctx, id)
	case "subscriptions":
		return q.PurgeMerchantRowsSubscriptions(ctx, id)
	case "entitlements":
		return q.PurgeMerchantRowsEntitlements(ctx, id)
	case "payments":
		return q.PurgeMerchantRowsPayments(ctx, id)
	case "notification_queue":
		return q.PurgeMerchantRowsNotificationQueue(ctx, id)
	case "rail_customer_accounts":
		return q.PurgeMerchantRowsRailCustomers(ctx, id)
	case "checkout_sessions":
		return q.PurgeMerchantRowsCheckoutSessions(ctx, id)
	case "rail_mutation_logs":
		return q.PurgeMerchantRowsExternalProviderMutationLogs(ctx, id)
	case "rail_intents":
		return q.PurgeMerchantRowsProviderIntents(ctx, id)
	case "money_settings":
		return q.PurgeMerchantRowsMoneyAccounts(ctx, id)
	default:
		return fmt.Errorf("merchants: no purge query for table %q", table)
	}
}

// PurgeInventory is the manifest of what a merchant purge is ABOUT TO DESTROY.
//
// IT IS NOT A BACKUP AND IT RESTORES NOTHING. It holds counts, secret NAMES and
// an explicit list of everything it does not capture — no customer, subscription,
// payment, entitlement or catalog row is copied anywhere, and no secret VALUE
// ever leaves its store. Its whole job is to put the blast radius in front of
// the operator before they type the confirmation.
//
// The only restore path for a purged merchant is Postgres point-in-time recovery
// plus the ENCRYPTION_MASTER_KEY plus Vault — see docs/backup-and-recovery.md. A
// real per-merchant archive is or#859 phase 2 (`openrails merchant snapshot`);
// until that exists, a purge is one-way.
type PurgeInventory struct {
	// ID is the openrails.merchant_purge_inventories row id.
	ID string
	// MerchantSlug is the merchant this inventory describes.
	MerchantSlug string
	// RowCounts is per-table, including rows a previous prune soft-deleted: the
	// purge hard-deletes those too, so they belong in the blast radius.
	RowCounts map[string]int
	// TotalRows is the number Delete requires the operator to type back.
	TotalRows int
	// SecretNames are the merchant's secret names. Values are never read.
	SecretNames []string
	// NotCaptured spells out, in operator-facing prose, everything this
	// inventory does not and cannot bring back.
	NotCaptured []string
}

// IsBackup answers the question the old name invited. It is always false.
func (PurgeInventory) IsBackup() bool { return false }

// notCaptured builds the honest list. Some entries are quantified from the
// inventory itself so the sentence names a number the operator can check.
func notCaptured(counts map[string]int, secrets int) []string {
	out := []string{
		"ROW DATA. This inventory holds counts, not rows. Not one customer, subscription, " +
			"payment, entitlement, price or product row is copied anywhere by taking it.",
		fmt.Sprintf("SECRET VALUES. %d secret NAMES are listed; no value is ever read or written out. "+
			"A purge deletes the merchant's secrets from Vault and from the DB-encrypted store, "+
			"and nothing here can recreate them.", secrets),
		"THE APPEND-ONLY SPINE. ledger_transfers, ledger_accounts, grants and " +
			"subscription_status_transitions are not purged (the app role holds no DELETE on them) " +
			"and are not captured here either. After a purge they outlive the control-plane rows " +
			"they referenced.",
	}
	if n := counts["payment_methods"]; n > 0 {
		out = append(out, fmt.Sprintf(
			"STORED PAYMENT METHODS (%d). The purge drops the LOCAL custody mirror only. The "+
				"instruments themselves stay at the PSP and are NOT revoked — after this you no longer "+
				"know which vault tokens exist to be revoked. Only the end user deletes their own "+
				"instrument; a purge must not be used as a way to.", n))
	}
	if n := counts["rail_intents"] + counts["rail_mutation_logs"]; n > 0 {
		out = append(out, fmt.Sprintf(
			"THE PROVIDER-WRITE AUDIT TRAIL (%d rows across rail_intents and rail_mutation_logs). "+
				"The record of every external write this deployment attempted for the merchant is "+
				"destroyed with it, including intents queued but never fired.", n))
	}
	out = append(out,
		"PROVIDER-SIDE STATE. Subscriptions, plans, customer vaults and webhook endpoints at "+
			"NMI / Stripe / CCBill / Solana are untouched by a purge and unreachable afterwards: "+
			"the local rows naming them are gone, so nothing remains to reconcile against.",
		"THE RESTORE PATH. There is exactly one — Postgres point-in-time recovery, with the "+
			"ENCRYPTION_MASTER_KEY and Vault restored alongside it (docs/backup-and-recovery.md). "+
			"If you do not have PITR configured and tested, a purge is final.")
	return out
}

// TakePurgeInventory records what a purge of this merchant would destroy and
// returns it. It writes an openrails.merchant_purge_inventories row that Delete
// then requires — the gate exists so the operator has SEEN the blast radius,
// not because the inventory can undo anything. See PurgeInventory.
//
// Was Export (#225). The old name promised a restore point that never existed.
func (s *Service) TakePurgeInventory(ctx context.Context, id merchant.ID) (PurgeInventory, error) {
	m, err := s.merchantByID(ctx, id)
	if err != nil {
		return PurgeInventory{}, err
	}

	tables, err := s.existingMerchantTables(ctx)
	if err != nil {
		return PurgeInventory{}, err
	}
	counts, total, err := s.countMerchantOwnedRows(ctx, id, tables)
	if err != nil {
		return PurgeInventory{}, err
	}

	// Enumerate per-merchant secret NAMES (never values).
	var secretNames []string
	if s.secrets != nil {
		names, err := s.secrets.List(ctx, id)
		if err != nil {
			return PurgeInventory{}, fmt.Errorf("merchants: purge inventory enumerate secrets: %w", err)
		}
		secretNames = append(secretNames, names...)
		sort.Strings(secretNames)
	}

	inv := PurgeInventory{
		MerchantSlug: m.Slug,
		RowCounts:    counts,
		TotalRows:    total,
		SecretNames:  secretNames,
		NotCaptured:  notCaptured(counts, len(secretNames)),
	}

	manifestJSON, err := json.Marshal(map[string]any{
		"kind":          "purge_inventory",
		"is_backup":     false,
		"restores":      "nothing",
		"merchant_slug": inv.MerchantSlug,
		"row_counts":    inv.RowCounts,
		"total_rows":    inv.TotalRows,
		"secret_names":  inv.SecretNames,
		"not_captured":  inv.NotCaptured,
		"restore_path": "Postgres point-in-time recovery + ENCRYPTION_MASTER_KEY + Vault " +
			"(docs/backup-and-recovery.md). This inventory restores nothing.",
	})
	if err != nil {
		return PurgeInventory{}, fmt.Errorf("merchants: marshal purge inventory: %w", err)
	}

	if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO openrails.merchant_purge_inventories (merchant_id, status, manifest, completed_at)
			VALUES ($1::uuid, 'completed', $2::jsonb, current_timestamp)
			RETURNING id::text
		`, id.String(), string(manifestJSON)).Scan(&inv.ID)
	}); err != nil {
		return PurgeInventory{}, fmt.Errorf("merchants: record purge inventory: %w", err)
	}
	return inv, nil
}

// countMerchantOwnedRows returns per-table counts and the total. Counts are
// deliberately unfiltered by deleted_at: a purge hard-deletes soft-deleted rows
// too, so they are part of the blast radius.
func (s *Service) countMerchantOwnedRows(ctx context.Context, id merchant.ID, tables []string) (map[string]int, int, error) {
	counts := make(map[string]int, len(tables))
	total := 0
	if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		for _, tbl := range tables {
			n, err := countMerchantRows(ctx, q, tbl, id.UUID())
			if err != nil {
				return fmt.Errorf("merchants: count %s: %w", tbl, err)
			}
			counts[tbl] = int(n)
			total += int(n)
		}
		return nil
	}); err != nil {
		return nil, 0, err
	}
	return counts, total, nil
}

// PurgeConfirmPhrase is the exact string DeleteOptions.ConfirmPhrase must carry.
// Typing it is the point: the sentence states the true situation, so an operator
// cannot authorise a purge without asserting in their own keystrokes that no
// backup exists.
func PurgeConfirmPhrase(slug string) string {
	return fmt.Sprintf("purge merchant %s permanently, no backup exists", slug)
}

// DeleteOptions parameterizes a merchant purge. A bare boolean is not a
// confirmation: it authorises a blast radius the operator never saw.
type DeleteOptions struct {
	// ConfirmPhrase must equal PurgeConfirmPhrase(slug) exactly.
	ConfirmPhrase string
	// ExpectRows is the operator's typed blast radius — the total merchant-owned
	// row count they believe they are destroying. It must match what the purge
	// discovers, or nothing is written.
	ExpectRows *int
	// Actor is who asked for it; recorded on the destructive run.
	Actor string
}

// ErrPurgeNotConfirmed: the typed confirmation was absent or wrong. Its message
// carries the true blast radius and the exact phrase required.
type ErrPurgeNotConfirmed struct {
	Slug      string
	TotalRows int
	Want      string
}

func (e *ErrPurgeNotConfirmed) Error() string {
	return fmt.Sprintf(
		"refusing to purge merchant %s: this destroys %d rows across every merchant-owned table and is NOT reversible — "+
			"the purge inventory is not a backup, and only Postgres PITR can bring the merchant back. "+
			"To proceed, take a fresh inventory and pass ConfirmPhrase=%q with ExpectRows=%d",
		e.Slug, e.TotalRows, e.Want, e.TotalRows)
}

// ErrPurgeRowCountMismatch: the typed row count disagrees with what the purge
// found. Nothing is written.
type ErrPurgeRowCountMismatch struct {
	Expected *int
	Found    int
}

func (e *ErrPurgeRowCountMismatch) Error() string {
	if e.Expected == nil {
		return fmt.Sprintf("refusing to purge: ExpectRows is required. This merchant holds %d rows; take a purge inventory, read it, then confirm %d", e.Found, e.Found)
	}
	return fmt.Sprintf("refusing to purge: ExpectRows says %d, this merchant holds %d. Take a fresh purge inventory and confirm the number it reports", *e.Expected, e.Found)
}

// ErrPurgeInventoryStale: no inventory exists for the merchant's CURRENT row
// count. An inventory taken before the book changed proves the operator looked
// at a state that no longer exists.
type ErrPurgeInventoryStale struct {
	Slug      string
	TotalRows int
}

func (e *ErrPurgeInventoryStale) Error() string {
	return fmt.Sprintf(
		"refusing to purge merchant %s: no purge inventory matches its current %d rows. "+
			"Take a fresh inventory (TakePurgeInventory), read what it says is NOT captured, then purge. "+
			"The inventory is not a backup — it exists so the blast radius is seen, not so the data can come back",
		e.Slug, e.TotalRows)
}

// ErrPurgeBlockedByRetainedHistory: a row the purge tried to delete is pinned by
// a table the purge deliberately does not touch — most often the append-only
// grant log, which FK-references the payments and products it justifies.
//
// This is a refusal, not a partial purge: the whole transaction rolls back, so
// the merchant is untouched. It is the correct outcome. Removing the pin would
// mean deleting the append-only record that entitlements are derived from,
// which is not something a purge may do.
type ErrPurgeBlockedByRetainedHistory struct {
	Slug       string
	Constraint string
	Detail     string
}

func (e *ErrPurgeBlockedByRetainedHistory) Error() string {
	return fmt.Sprintf(
		"refusing to purge merchant %s: its retained history pins rows the purge would have to delete (constraint %q: %s). "+
			"NOTHING was deleted — the transaction rolled back. The append-only ledger, grant log and status-transition history are "+
			"never purged, and they reference the payments/products/prices they justify, so a merchant with real billing history "+
			"cannot be purged this way. Retire the merchant (status/archived) instead, or take the question to a real per-merchant "+
			"archive (or#859)",
		e.Slug, e.Constraint, e.Detail)
}

// Delete performs the gated purge of a merchant. It is one-way: read
// PurgeInventory before wiring anything to this.
//
// Five refusals stand in front of it, in order:
//
//   - the destructive-action gate (#836/#835) — the same instance kill switch and
//     per-merchant policy that hold a mass cancellation. A nil gate DENIES.
//   - a typed confirmation phrase naming the merchant and stating that no backup
//     exists (PurgeConfirmPhrase).
//   - a typed row count that must equal the true blast radius.
//   - a purge inventory whose recorded total still matches that count, so the
//     operator demonstrably looked at THIS state.
//   - Confirm-by-construction: DeleteOptions cannot be satisfied by a boolean.
//
// It then purges the supported merchant-owned rows and DB-backed secrets,
// and tombstones the directory row (status='deleted', deleted_at) inside one
// transaction, stamped with a destructive_runs row (kind=merchant_purge). The
// run captures database completion and any external cleanup target atomically.
// A Vault purge completes the run only after external cleanup is verified; failed
// cleanup stays retryable through RetrySecretCleanup and its scheduled worker.
//
// Re-running Delete on an already-deleted merchant returns ErrMerchantNotFound.
func (s *Service) Delete(ctx context.Context, id merchant.ID, opts DeleteOptions) error {
	m, err := s.merchantByID(ctx, id)
	if err != nil {
		return err
	}

	// Resolve which merchant-owned tables actually exist BEFORE opening the tx: a
	// statement error inside a Postgres tx aborts the whole tx (SQLSTATE 25P02),
	// so we must not issue a DELETE against a missing table.
	tables, err := s.existingMerchantTables(ctx)
	if err != nil {
		return err
	}

	// The true blast radius, measured before any confirmation is judged, so every
	// refusal below can state it.
	counts, total, err := s.countMerchantOwnedRows(ctx, id, tables)
	if err != nil {
		return err
	}

	want := PurgeConfirmPhrase(m.Slug)
	if opts.ConfirmPhrase != want {
		return &ErrPurgeNotConfirmed{Slug: m.Slug, TotalRows: total, Want: want}
	}
	if opts.ExpectRows == nil || *opts.ExpectRows != total {
		return &ErrPurgeRowCountMismatch{Expected: opts.ExpectRows, Found: total}
	}

	// The gate is the LAST wall, deliberately after the read-only checks above:
	// every refusal an operator can still act on states the true blast radius,
	// and the gate then stands between a fully-typed request and the first write.
	// Fail-closed — an unwired gate denies.
	gate := s.destructive
	if gate == nil {
		gate = deniedPolicy{}
	}
	if allowed, reason := gate.AllowDestructive(ctx, id.UUID()); !allowed {
		return fmt.Errorf("merchants: refusing to purge merchant %s: %s", m.Slug, reason)
	}

	actor := opts.Actor
	if actor == "" {
		actor = "unknown"
	}
	cleanupPlan, err := captureSecretCleanup(ctx, s.secrets, id)
	if err != nil {
		return err
	}
	runID := uuid.New()
	expected := int64(total)
	note := fmt.Sprintf("merchant purge %s (%d rows) — one-way; restore path is PITR only", m.Slug, total)
	proof := map[string]any{
		"kind":       "merchant_purge",
		"is_backup":  false,
		"row_counts": counts,
		"total_rows": total,
	}
	if cleanupPlan != nil {
		proof["secret_cleanup"] = cleanupPlan
	}
	inventoryProof, err := json.Marshal(proof)
	if err != nil {
		return fmt.Errorf("merchants: marshal purge proof: %w", err)
	}

	if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := gen.New(tx).LockLiveMerchantForSecretWrite(ctx, id.UUID()); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrMerchantNotFound
			}
			return err
		}
		// inventory-before-purge: an inventory for the merchant's CURRENT row
		// count. A stale one proves nothing about what is about to be destroyed.
		var matching int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM openrails.merchant_purge_inventories
			 WHERE merchant_id = $1::uuid AND status = 'completed'
			   AND (manifest->>'total_rows')::bigint = $2::bigint
		`, id.String(), int64(total)).Scan(&matching); err != nil {
			return fmt.Errorf("merchants: check inventory-before-purge: %w", err)
		}
		if matching == 0 {
			return &ErrPurgeInventoryStale{Slug: m.Slug, TotalRows: total}
		}

		txq := gen.New(tx)
		if _, err := txq.CreateDestructiveRun(ctx, gen.CreateDestructiveRunParams{
			ID: runID, MerchantID: id.UUID(), Kind: DestructiveRunKindMerchantPurge,
			Actor: actor, DryRun: false, Coverage: inventoryProof,
			ExpectedRows: &expected, Note: &note,
		}); err != nil {
			return fmt.Errorf("merchants: open destructive run: %w", err)
		}

		for _, tbl := range tables {
			if err := purgeMerchantRows(ctx, txq, tbl, id.UUID()); err != nil {
				return fmt.Errorf("merchants: purge %s: %w", tbl, err)
			}
		}

		// Purge DB-backed secret store rows; the Vault-backed store is purged
		// separately below.
		if _, err := tx.Exec(ctx, `DELETE FROM openrails.merchant_secrets WHERE merchant_id = $1::uuid`, id.String()); err != nil {
			return fmt.Errorf("merchants: purge merchant secrets: %w", err)
		}

		// Tombstone the directory row.
		if _, err := tx.Exec(ctx, `
			UPDATE openrails.merchants
			   SET status = 'deleted', deleted_at = current_timestamp, updated_at = current_timestamp
			 WHERE id = $1::uuid
		`, id.String()); err != nil {
			return fmt.Errorf("merchants: tombstone merchant: %w", err)
		}

		completionCounts := make(map[string]any, len(counts)+1)
		for table, count := range counts {
			completionCounts[table] = count
		}
		completionCounts["database_purged"] = true
		affected, _ := json.Marshal(completionCounts)
		if err := txq.MarkMerchantDatabasePurged(ctx, gen.MarkMerchantDatabasePurgedParams{MerchantID: id.UUID(), ID: runID, Affected: affected}); err != nil {
			return err
		}
		if cleanupPlan != nil {
			return nil
		}
		if _, err := txq.FinishDestructiveRun(ctx, gen.FinishDestructiveRunParams{
			MerchantID: id.UUID(), ID: runID, Status: "completed",
			Now: time.Now().UTC(), Affected: affected,
		}); err != nil {
			return fmt.Errorf("merchants: close destructive run %s: %w", runID, err)
		}
		return nil
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23001") {
			return &ErrPurgeBlockedByRetainedHistory{Slug: m.Slug, Constraint: pgErr.ConstraintName, Detail: pgErr.Detail}
		}
		return err
	}

	if cleanupPlan != nil {
		return s.RetrySecretCleanup(ctx, id, runID)
	}

	return nil
}

// existingMerchantTables returns the subset of merchantOwnedTables that actually
// exist in the billing schema, so inventory/purge tolerate a partial schema
// without aborting a transaction on a missing table. The names are intersected
// against a fixed allow-list, never user input.
func (s *Service) existingMerchantTables(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		 WHERE table_schema = $1 AND table_name = ANY($2)
	`, s.pool.Schema(), merchantOwnedTables)
	if err != nil {
		return nil, fmt.Errorf("merchants: list existing merchant tables: %w", err)
	}
	defer rows.Close()
	present := make(map[string]bool)
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		present[n] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Preserve the canonical order from merchantOwnedTables.
	out := make([]string, 0, len(present))
	for _, t := range merchantOwnedTables {
		if present[t] {
			out = append(out, t)
		}
	}
	return out, nil
}
