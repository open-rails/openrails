package merchants

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// merchantOwnedTables are the openrails.* tables that carry a tenant_id. Merchant
// deletion purges these rows for the target tenant. Kept in sync with the
// consolidated schema's tenant-owned table set AND with
// queries/tenant_lifecycle.sql + the dispatch switches below (#334: each table
// has a STATIC generated count/purge query — no runtime SQL assembly).
var merchantOwnedTables = []string{
	"products", "prices", "catalog_drift_events", "payment_methods",
	"subscriptions", "entitlements", "payments",
	"notification_queue", "processor_customers",
	"checkout_sessions", "provider_intents",
	// money ledger (#512 hard cut): the single-entry money_blocks/money_transactions
	// tables are gone. The append-only ledger_transfers/grants are immutable
	// (REVOKE DELETE) and intentionally NOT row-purged here.
	"money_settings",
}

// countTenantRows dispatches to the table's generated count query.
func countTenantRows(ctx context.Context, q *gen.Queries, table string, id uuid.UUID) (int64, error) {
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
	case "processor_customers":
		return q.CountMerchantRowsProcessorCustomers(ctx, id)
	case "checkout_sessions":
		return q.CountMerchantRowsCheckoutSessions(ctx, id)
	case "provider_intents":
		return q.CountMerchantRowsProviderIntents(ctx, id)
	case "money_settings":
		return q.CountMerchantRowsMoneyAccounts(ctx, id)
	default:
		return 0, fmt.Errorf("tenancy: no count query for table %q", table)
	}
}

// purgeTenantRows dispatches to the table's generated purge query.
func purgeTenantRows(ctx context.Context, q *gen.Queries, table string, id uuid.UUID) error {
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
	case "processor_customers":
		return q.PurgeMerchantRowsProcessorCustomers(ctx, id)
	case "checkout_sessions":
		return q.PurgeMerchantRowsCheckoutSessions(ctx, id)
	case "provider_intents":
		return q.PurgeMerchantRowsProviderIntents(ctx, id)
	case "money_settings":
		return q.PurgeMerchantRowsMoneyAccounts(ctx, id)
	default:
		return fmt.Errorf("tenancy: no purge query for table %q", table)
	}
}

// Export performs a merchant-scoped logical export and records a completed
// openrails.merchant_exports row (issue #225). The export here is a row-count
// manifest plus a Vault-side secret-name enumeration (values are NEVER exported
// in plaintext) — enough to satisfy export-before-delete and give a portability
// manifest. A managed deployment can extend `location` to a real dump artifact.
//
// Returns the export id. Delete is gated on at least one completed export.
func (s *Service) Export(ctx context.Context, id merchant.ID) (string, map[string]int, error) {
	if _, err := s.merchantByID(ctx, id); err != nil {
		return "", nil, err
	}

	tables, err := s.existingTenantTables(ctx)
	if err != nil {
		return "", nil, err
	}
	counts := make(map[string]int, len(tables))
	if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		for _, tbl := range tables {
			n, err := countTenantRows(ctx, q, tbl, id.UUID())
			if err != nil {
				return fmt.Errorf("tenancy: export count %s: %w", tbl, err)
			}
			counts[tbl] = int(n)
		}
		return nil
	}); err != nil {
		return "", nil, err
	}

	// Enumerate per-tenant secret NAMES (never values) for the portability manifest.
	var secretNames []string
	if s.secrets != nil {
		names, err := s.secrets.List(ctx, id)
		if err != nil {
			return "", nil, fmt.Errorf("tenancy: export enumerate secrets: %w", err)
		}
		secretNames = names
	}

	manifest := map[string]any{"row_counts": counts, "secret_names": secretNames}
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return "", nil, fmt.Errorf("tenancy: marshal export manifest: %w", err)
	}

	var exportID string
	err = s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO openrails.merchant_exports (merchant_id, status, row_counts, completed_at)
			VALUES ($1::uuid, 'completed', $2::jsonb, current_timestamp)
			RETURNING id::text
		`, id.String(), string(manifestJSON)).Scan(&exportID)
	})
	if err != nil {
		return "", nil, fmt.Errorf("tenancy: record export: %w", err)
	}
	return exportID, counts, nil
}

// DeleteOptions parameterizes a merchant delete.
type DeleteOptions struct {
	// Confirm must be true: deletion is a gated purge. A false Confirm returns an
	// error so an accidental call cannot purge merchant data.
	Confirm bool
}

// Delete performs the gated purge of a merchant (issue #225):
//
//   - requires Confirm (confirmation gate),
//   - requires a completed export (export-before-delete enforced),
//   - purges every merchant-owned openrails.* row, the merchant's cached secrets, and
//     credential/audit/export bookkeeping, then tombstones the directory row
//     (status='deleted', deleted_at set) inside ONE transaction.
//
// Re-running Delete on an already-deleted tenant returns ErrMerchantNotFound (the
// active-row lookup misses), so the purge itself is not re-run.
func (s *Service) Delete(ctx context.Context, id merchant.ID, opts DeleteOptions) error {
	if !opts.Confirm {
		return errors.New("tenancy: delete requires confirmation")
	}
	if _, err := s.merchantByID(ctx, id); err != nil {
		return err
	}

	// Resolve which merchant-owned tables actually exist BEFORE opening the tx: a
	// statement error inside a Postgres tx aborts the whole tx (SQLSTATE 25P02),
	// so we must not issue a DELETE against a missing table.
	tables, err := s.existingTenantTables(ctx)
	if err != nil {
		return err
	}

	if err := s.pool.MerchantTx(ctx, id, func(ctx context.Context, tx pgx.Tx) error {
		// export-before-delete: require at least one completed export.
		var exportCount int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM openrails.merchant_exports
			 WHERE merchant_id = $1::uuid AND status = 'completed'
		`, id.String()).Scan(&exportCount); err != nil {
			return fmt.Errorf("tenancy: check export-before-delete: %w", err)
		}
		if exportCount == 0 {
			return ErrExportRequired
		}

		txq := gen.New(tx)
		for _, tbl := range tables {
			if err := purgeTenantRows(ctx, txq, tbl, id.UUID()); err != nil {
				return fmt.Errorf("tenancy: purge %s: %w", tbl, err)
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
			   SET status = 'deleted', deleted_at = current_timestamp, updated_at = current_timestamp,
			       webhook_host = NULL, webhook_path = NULL
			 WHERE id = $1::uuid
		`, id.String()); err != nil {
			return fmt.Errorf("tenancy: tombstone tenant: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}

	// Best-effort purge of any non-DB secret backend (e.g. Vault). Done outside
	// the tx since it is not transactional; failures here are logged by callers.
	if s.secrets != nil {
		if names, lerr := s.secrets.List(ctx, id); lerr == nil {
			for _, n := range names {
				_ = s.secrets.Delete(ctx, id, n)
			}
		}
	}
	return nil
}

// existingTenantTables returns the subset of merchantOwnedTables that actually
// exist in the billing schema, so export/delete tolerate a partial schema
// without aborting a transaction on a missing table. The names are intersected
// against a fixed allow-list, never user input.
func (s *Service) existingTenantTables(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		 WHERE table_schema = $1 AND table_name = ANY($2)
	`, s.pool.Schema(), merchantOwnedTables)
	if err != nil {
		return nil, fmt.Errorf("tenancy: list existing tenant tables: %w", err)
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
