package tenancy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/open-rails/openrails/pkg/tenant"
)

// tenantOwnedTables are the billing.* tables that carry a tenant_id. Tenant
// deletion purges these rows for the target tenant. Kept in sync with the
// consolidated schema's tenant-owned table set.
var tenantOwnedTables = []string{
	"products", "prices", "catalog_drift_events", "payment_methods",
	"subscriptions", "entitlements", "payments", "admin_grants",
	"notification_queue", "processor_customers", "credit_types",
	"credit_transactions", "credit_blocks", "credit_balances",
	"checkout_sessions", "manual_rebill_attempts",
}

// Export performs a tenant-scoped logical export and records a completed
// billing.tenant_exports row (issue #225). The export here is a row-count
// manifest plus a Vault-side secret-name enumeration (values are NEVER exported
// in plaintext) — enough to satisfy export-before-delete and give a portability
// manifest. A managed deployment can extend `location` to a real dump artifact.
//
// Returns the export id. Delete is gated on at least one completed export.
func (s *Service) Export(ctx context.Context, id tenant.ID) (string, map[string]int, error) {
	if _, err := s.tenantByID(ctx, id); err != nil {
		return "", nil, err
	}

	tables, err := s.existingTenantTables(ctx)
	if err != nil {
		return "", nil, err
	}
	counts := make(map[string]int, len(tables))
	for _, tbl := range tables {
		var n int
		// table name comes from a fixed allow-list, never user input.
		err := s.pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM billing.%s WHERE tenant_id = $1::uuid`, tbl),
			id.String()).Scan(&n)
		if err != nil {
			return "", nil, fmt.Errorf("tenancy: export count %s: %w", tbl, err)
		}
		counts[tbl] = n
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
	err = s.pool.QueryRow(ctx, `
		INSERT INTO billing.tenant_exports (tenant_id, status, row_counts, completed_at)
		VALUES ($1::uuid, 'completed', $2::jsonb, current_timestamp)
		RETURNING id::text
	`, id.String(), string(manifestJSON)).Scan(&exportID)
	if err != nil {
		return "", nil, fmt.Errorf("tenancy: record export: %w", err)
	}
	return exportID, counts, nil
}

// DeleteOptions parameterizes a tenant delete.
type DeleteOptions struct {
	// Confirm must be true: deletion is a gated purge. A false Confirm returns an
	// error so an accidental call cannot purge tenant data.
	Confirm bool
}

// Delete performs the gated purge of a tenant (issue #225):
//
//   - requires Confirm (confirmation gate),
//   - requires a completed export (export-before-delete enforced),
//   - purges every tenant-owned billing.* row, the tenant's cached secrets, and
//     credential/audit/export bookkeeping, then tombstones the directory row
//     (status='deleted', deleted_at set) inside ONE transaction.
//
// The default tenant can never be deleted. Re-running Delete on an
// already-deleted tenant returns ErrTenantNotFound (the active-row lookup
// misses), so the purge itself is not re-run.
func (s *Service) Delete(ctx context.Context, id tenant.ID, opts DeleteOptions) error {
	if !opts.Confirm {
		return errors.New("tenancy: delete requires confirmation")
	}
	if id.IsDefault() {
		return errors.New("tenancy: the default tenant cannot be deleted")
	}
	if _, err := s.tenantByID(ctx, id); err != nil {
		return err
	}

	// export-before-delete: require at least one completed export.
	var exportCount int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM billing.tenant_exports
		 WHERE tenant_id = $1::uuid AND status = 'completed'
	`, id.String()).Scan(&exportCount); err != nil {
		return fmt.Errorf("tenancy: check export-before-delete: %w", err)
	}
	if exportCount == 0 {
		return ErrExportRequired
	}

	// Resolve which tenant-owned tables actually exist BEFORE opening the tx: a
	// statement error inside a Postgres tx aborts the whole tx (SQLSTATE 25P02),
	// so we must not issue a DELETE against a missing table.
	tables, err := s.existingTenantTables(ctx)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("tenancy: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	for _, tbl := range tables {
		if _, err := tx.Exec(ctx,
			fmt.Sprintf(`DELETE FROM billing.%s WHERE tenant_id = $1::uuid`, tbl),
			id.String()); err != nil {
			return fmt.Errorf("tenancy: purge %s: %w", tbl, err)
		}
	}

	// Purge secret store rows + credential audit (DB-backed secret store lives in
	// billing.tenant_secrets; the Vault-backed store is purged separately below).
	if _, err := tx.Exec(ctx, `DELETE FROM billing.tenant_secrets WHERE tenant_id = $1::uuid`, id.String()); err != nil {
		return fmt.Errorf("tenancy: purge tenant secrets: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM billing.tenant_credential_audit WHERE tenant_id = $1::uuid`, id.String()); err != nil {
		return fmt.Errorf("tenancy: purge credential audit: %w", err)
	}

	// Tombstone the directory row.
	if _, err := tx.Exec(ctx, `
		UPDATE billing.tenants
		   SET status = 'deleted', deleted_at = current_timestamp, updated_at = current_timestamp,
		       webhook_host = NULL, webhook_path = NULL
		 WHERE id = $1::uuid
	`, id.String()); err != nil {
		return fmt.Errorf("tenancy: tombstone tenant: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("tenancy: commit delete: %w", err)
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

// existingTenantTables returns the subset of tenantOwnedTables that actually
// exist in the billing schema, so export/delete tolerate a partial schema
// without aborting a transaction on a missing table. The names are intersected
// against a fixed allow-list, never user input.
func (s *Service) existingTenantTables(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'billing' AND table_name = ANY($1)
	`, tenantOwnedTables)
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
	// Preserve the canonical order from tenantOwnedTables.
	out := make([]string, 0, len(present))
	for _, t := range tenantOwnedTables {
		if present[t] {
			out = append(out, t)
		}
	}
	return out, nil
}
