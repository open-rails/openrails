// Package merchantarchive moves a retained billing book between deployments.
// It never resolves provider clients, invokes lifecycle services, or schedules
// work. The source's writers must remain stopped during the final cutover.
package merchantarchive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/retention"
)

type Result struct {
	MerchantID string `json:"merchant_id"`
	Digest     string `json:"digest"`
	Rows       int64  `json:"rows"`
	Replayed   bool   `json:"replayed"`
}

type Error struct {
	Code  string
	Table string
	Count int64
	Err   error
}

func (e *Error) Error() string {
	if e.Table != "" {
		return fmt.Sprintf("merchant archive %s: %s (%d rows)", e.Code, e.Table, e.Count)
	}
	return "merchant archive " + e.Code
}
func (e *Error) Unwrap() error { return e.Err }

func classify(err error) error {
	if err == nil {
		return nil
	}
	var archiveErr *Error
	if errors.As(err, &archiveErr) {
		return err
	}
	code := "database"
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "55000":
			code = "not_empty"
		case "23514", "23503", "23505", "23502":
			code = "integrity"
		case "P0002":
			code = "merchant_mismatch"
		}
	}
	return &Error{Code: code, Err: err}
}

// Export writes a consistent snapshot. The caller must discard a partial
// artifact on error; only the verified footer establishes a complete artifact.
func Export(ctx context.Context, database *db.DB, id billing.MerchantID, out io.Writer) error {
	if database == nil || id.IsZero() {
		return &Error{Code: "merchant_mismatch"}
	}
	ctx = merchant.WithID(ctx, id)
	undo, err := fenceExport(ctx, database, id)
	if err != nil {
		return classify(err)
	}
	// RunInTx (not MerchantTx) lets isolation be set before even the GUC query.
	err = database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET TRANSACTION ISOLATION LEVEL REPEATABLE READ, READ ONLY"); err != nil {
			return err
		}
		if err := scope(ctx, tx, id); err != nil {
			return err
		}
		if err := checkSchema(ctx, tx); err != nil {
			return err
		}
		if err := preflight(ctx, tx, id); err != nil {
			return err
		}
		q := gen.New(tx)
		if err := q.CheckBillingRestoreLedger(ctx, id.UUID()); err != nil {
			return err
		}
		catalogRevision, err := q.GetCatalogRevision(ctx, id.UUID())
		if err != nil {
			return err
		}
		w, err := archivewire.NewWriter(out, id.String(), catalogRevision)
		if err != nil {
			return err
		}
		for _, p := range contract.Profiles {
			if err := w.Table(p.Name); err != nil {
				return err
			}
			query := exportQuery(p)
			rows, err := tx.Query(ctx, query, id.UUID())
			if err != nil {
				return err
			}
			var count int64
			for rows.Next() {
				values := make([]*string, len(p.Columns))
				dest := make([]any, len(values))
				for i := range values {
					dest[i] = &values[i]
				}
				if err := rows.Scan(dest...); err != nil {
					rows.Close()
					return err
				}
				if err := contract.ValidateValues(p, values); err != nil {
					rows.Close()
					return &Error{Code: "unsupported_state", Table: p.Name, Count: 1, Err: err}
				}
				if err := w.Row(values); err != nil {
					rows.Close()
					return &Error{Code: "unsupported_state", Table: p.Name, Count: 1, Err: err}
				}
				count++
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			var expected int64
			if err := tx.QueryRow(ctx, countQuery(p), id.UUID()).Scan(&expected); err != nil {
				return err
			}
			if count != expected {
				return &Error{Code: "integrity", Table: p.Name, Count: expected - count}
			}
		}
		return w.Close()
	})
	if err != nil {
		undo()
	}
	return classify(err)
}

// Restore validates and inserts in one transaction. Footer failure, disconnect,
// mismatched identity, unsupported state or integrity errors roll back all rows.
// A matching committed receipt is a no-op even if the target has since advanced.
func Restore(ctx context.Context, database *db.DB, id billing.MerchantID, in io.Reader) (Result, error) {
	var result Result
	if database == nil || id.IsZero() {
		return result, &Error{Code: "merchant_mismatch"}
	}
	ctx = merchant.WithID(ctx, id)
	// Archived rows may be older than anything written here: their months need
	// partitions before the restore transaction, which must not hold DDL locks.
	restoreNow := time.Now()
	if _, err := retention.EnsureRetainedPartitions(ctx, database.GenDirectory(), restoreNow); err != nil {
		return result, classify(err)
	}
	err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := scope(ctx, tx, id); err != nil {
			return err
		}
		if err := checkSchema(ctx, tx); err != nil {
			return err
		}
		q := gen.New(tx)
		var previousDigest *string
		var previousRows *int64
		var restoredCatalogRevision int64
		info, err := contract.Read(in, func(h archivewire.Header) error {
			if h.MerchantID != id.String() {
				return &Error{Code: "merchant_mismatch"}
			}
			restoredCatalogRevision = h.CatalogRevision
			if err := q.SetCatalogBatchMerchant(ctx, id.String()); err != nil {
				return err
			}
			if h.Version == 1 {
				if _, err := tx.Exec(ctx, legacyEntitlementsDDL); err != nil {
					return err
				}
			}
			receipt, err := q.BeginBillingRestore(ctx, id.UUID())
			if err != nil {
				return err
			}
			previous, err := q.GetBillingRestoreReceipt(ctx, gen.GetBillingRestoreReceiptParams{MerchantID: id.UUID(), ID: receipt})
			if err != nil {
				return err
			}
			previousDigest, previousRows = previous.Digest, previous.Rows
			return nil
		}, func(p contract.Profile, values []*string) error {
			if previousDigest != nil {
				return nil
			}
			args := make([]any, len(values), len(values)+1)
			for i, v := range values {
				if v != nil {
					args[i] = *v
				}
			}
			if dropBefore, ok := partitionDropBefore[p.Name]; ok {
				args = append(args, dropBefore(restoreNow))
			}
			target := "billing." + p.Name
			if p.Name == contract.LegacyEntitlements.Name {
				target = legacyEntitlementsTable
			}
			_, err := tx.Exec(ctx, insertQuery(target, p), args...)
			return err
		})
		if err != nil {
			var ae *Error
			var pe *pgconn.PgError
			if errors.As(err, &ae) || errors.As(err, &pe) {
				return err
			}
			return &Error{Code: "invalid_artifact", Err: err}
		}
		result = Result{MerchantID: info.MerchantID, Digest: info.Digest, Rows: info.Rows, Replayed: previousDigest != nil}
		if previousDigest != nil {
			if *previousDigest != info.Digest || previousRows == nil || *previousRows != info.Rows {
				return &Error{Code: "not_empty"}
			}
			return nil
		}
		if err := q.SetCatalogRevision(ctx, gen.SetCatalogRevisionParams{MerchantID: id.UUID(), Revision: restoredCatalogRevision}); err != nil {
			return err
		}
		invalidReceipts, err := q.CatalogApplicationsAfterRevisionExist(ctx, gen.CatalogApplicationsAfterRevisionExistParams{MerchantID: id.UUID(), Revision: restoredCatalogRevision})
		if err != nil {
			return err
		}
		if invalidReceipts {
			return &Error{Code: "integrity", Table: "catalog_applications"}
		}
		if info.LegacyAccess {
			if err := convertLegacyAccess(ctx, tx, id, info.LegacyDurations, restoreNow); err != nil {
				return err
			}
		}
		if err := validateReferences(ctx, tx, id); err != nil {
			return err
		}
		if err := landReadonly(ctx, q, id, restoreNow); err != nil {
			return err
		}
		return q.FinishBillingRestore(ctx, gen.FinishBillingRestoreParams{MerchantID: id.UUID(), Digest: info.Digest, Rows: info.Rows})
	})
	if err != nil {
		return Result{}, classify(err)
	}
	return result, nil
}

func scope(ctx context.Context, tx pgx.Tx, id billing.MerchantID) error {
	q := gen.New(tx)
	if _, err := q.SetConfig(ctx, gen.SetConfigParams{Setting: db.MerchantGUC, Value: id.String(), IsLocal: true}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'; SET LOCAL DateStyle TO 'ISO, YMD'; SET LOCAL IntervalStyle TO 'iso_8601'; SET LOCAL extra_float_digits TO 3"); err != nil {
		return err
	}
	active, err := q.LiveMerchantExists(ctx, id.UUID())
	if err != nil {
		return err
	}
	if !active {
		return &Error{Code: "merchant_mismatch"}
	}
	return nil
}

// legacyEntitlementsTable holds a version 1 archive's per-key windows while
// restore converts them to product access.
const legacyEntitlementsTable = "pg_temp.legacy_entitlements"

const legacyEntitlementsDDL = `CREATE TEMP TABLE legacy_entitlements (
	merchant_id uuid NOT NULL, id uuid NOT NULL, entitlement text COLLATE "C" NOT NULL,
	starts_at timestamptz NOT NULL, ends_at timestamptz, source_id uuid, source_type text,
	revoked_at timestamptz, revoke_reason text, created_at timestamptz, updated_at timestamptz,
	deleted_at timestamptz, customer_id uuid NOT NULL, grant_id uuid, destructive_run_id uuid
) ON COMMIT DROP`

// legacySubscriptionBounds bounds the open subscription windows of archives
// preceding explicit access terms by their immutable paid grants, as
// migration 8 did.
const legacySubscriptionBounds = `WITH paid_bounds AS (
	SELECT e.id, CASE WHEN bool_or(g.ends_at IS NULL) THEN NULL ELSE max(g.ends_at) END AS ends_at
	FROM pg_temp.legacy_entitlements e
	JOIN billing.subscriptions s ON s.merchant_id = e.merchant_id AND s.id = e.source_id
	JOIN billing.grants g ON g.merchant_id = e.merchant_id AND g.customer_id = e.customer_id
	  AND g.source_type = 'subscription' AND g.source_id = s.id::text
	  AND g.kind = 'entitlement' AND g.event = 'grant'
	  AND g.spec_snapshot->'entitlements' ? e.entitlement
	WHERE e.merchant_id = $1 AND e.source_type = 'subscription' AND e.ends_at IS NULL
	  AND e.revoked_at IS NULL AND e.deleted_at IS NULL
	  AND s.access_duration_hours_snapshot IS NOT NULL
	  AND NOT EXISTS (SELECT 1 FROM billing.grants terminal
	      WHERE terminal.merchant_id = g.merchant_id AND terminal.supersedes_id = g.id
	        AND terminal.event IN ('revoke', 'expire', 'supersede'))
	GROUP BY e.id
)
UPDATE pg_temp.legacy_entitlements e SET ends_at = b.ends_at
FROM paid_bounds b WHERE b.id = e.id AND b.ends_at IS NOT NULL`

// convertLegacyAccess converts a version 1 archive's per-key windows to
// product access, as migration 20 converts a database. A restore cannot take
// the operator approval that conversion needs, so an archive whose conversion
// changes any customer's access is refused, to be cut over where it was
// exported.
func convertLegacyAccess(ctx context.Context, tx pgx.Tx, id billing.MerchantID, legacyDurations bool, at time.Time) error {
	if legacyDurations {
		if _, err := tx.Exec(ctx, legacySubscriptionBounds, id.UUID()); err != nil {
			return err
		}
	}
	q := gen.New(tx)
	if _, err := q.ConvertEntitlementWindows(ctx, gen.ConvertEntitlementWindowsParams{MerchantID: id.UUID(), Source: legacyEntitlementsTable, At: at}); err != nil {
		return err
	}
	changes, err := q.ListProductAccessChanges(ctx, gen.ListProductAccessChangesParams{MerchantID: id.UUID(), Source: legacyEntitlementsTable, At: at})
	if err != nil {
		return err
	}
	if len(changes) > 0 {
		c := changes[0]
		return &Error{Code: "unsupported_state", Table: "entitlements", Count: int64(len(changes)),
			Err: fmt.Errorf("product access conversion changes access (customer %s %s %q); cut the archive over before exporting it", c.CustomerID, c.Change, c.Entitlement)}
	}
	return nil
}

// insertQuery inserts one archived row. A partitioned table takes one more
// parameter, the oldest key it still keeps: a row older than that has no
// partition, and retention would drop it on the next pass, so it is not
// restored.
func insertQuery(target string, p contract.Profile) string {
	cols, params := make([]string, len(p.Columns)), make([]string, len(p.Columns))
	keep := ""
	for i, c := range p.Columns {
		cols[i] = pgx.Identifier{c.Name}.Sanitize()
		params[i] = fmt.Sprintf("$%d::text::%s", i+1, c.Type)
		if key, ok := partitionKeys[p.Name]; ok && c.Name == key {
			keep = fmt.Sprintf(" WHERE %s >= $%d::timestamptz", params[i], len(p.Columns)+1)
		}
	}
	return "INSERT INTO " + target + " (" + strings.Join(cols, ",") + ") SELECT " + strings.Join(params, ",") + keep
}

// partitionKeys and partitionDropBefore index retention.Partitioned by table.
var partitionKeys, partitionDropBefore = func() (map[string]string, map[string]func(time.Time) time.Time) {
	keys, drops := map[string]string{}, map[string]func(time.Time) time.Time{}
	for _, p := range retention.Partitioned {
		keys[p.Table], drops[p.Table] = p.Key, p.DropBefore
	}
	return keys, drops
}()

func countQuery(p contract.Profile) string {
	return "SELECT count(*) FROM billing." + p.Name + " t WHERE " + exportWhere(p.Name)
}

func exportQuery(p contract.Profile) string {
	cols := make([]string, len(p.Columns))
	for i, c := range p.Columns {
		expr := "t." + pgx.Identifier{c.Name}.Sanitize()
		switch p.Name + "." + c.Name {
		case "psps.settings":
			expr = "t.settings - 'rpc_api_key'"
		case "mandates.storing_attempt_id":
			// Payment attempts are analytics the archive leaves behind; the
			// mandate keeps its references.
			expr = "NULL::uuid"
		}
		cols[i] = "(" + expr + ")::text"
	}
	from := "billing." + p.Name + " t"
	order := ""
	for _, c := range p.Columns {
		if c.Name == "id" {
			order = "t.id"
			break
		}
	}
	if order == "" {
		parts := make([]string, len(p.Columns))
		for i, c := range p.Columns {
			parts[i] = "t." + pgx.Identifier{c.Name}.Sanitize() + "::text COLLATE \"C\" NULLS FIRST"
		}
		order = strings.Join(parts, ",")
	}
	prefix := ""
	parent := ""
	if p.Name == "payments" {
		parent = "refunded_payment_id"
	}
	if p.Name == "grants" {
		parent = "supersedes_id"
	}
	if parent != "" {
		prefix = "WITH RECURSIVE lineage AS (SELECT id,0 depth FROM billing." + p.Name + " WHERE merchant_id=$1 AND " + parent + " IS NULL UNION ALL SELECT c.id,l.depth+1 FROM billing." + p.Name + " c JOIN lineage l ON c." + parent + "=l.id WHERE c.merchant_id=$1) "
		from += " JOIN lineage l ON l.id=t.id"
		order = "l.depth,t.id"
	}
	return prefix + "SELECT " + strings.Join(cols, ",") + " FROM " + from + " WHERE " + exportWhere(p.Name) + " ORDER BY " + order
}

func exportWhere(table string) string {
	where := "t.merchant_id=$1"
	if table == "maintenance_runs" {
		where += " AND t.kind IN ('prune','converge_enforce','merchant_purge')"
	}
	return where
}
