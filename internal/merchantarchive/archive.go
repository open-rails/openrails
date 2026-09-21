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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/pkg/merchant"
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
func Export(ctx context.Context, database *db.DB, id merchant.ID, out io.Writer) error {
	if database == nil || id.IsZero() {
		return &Error{Code: "merchant_mismatch"}
	}
	ctx = merchant.WithID(ctx, id)
	// RunInTx (not MerchantTx) lets isolation be set before even the GUC query.
	err := database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
		if _, err := tx.Exec(ctx, "SELECT openrails.check_billing_restore_ledger($1)", id.UUID()); err != nil {
			return err
		}
		w, err := archivewire.NewWriter(out, id.String())
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
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM openrails."+p.Name+" t WHERE "+exportWhere(p.Name), id.UUID()).Scan(&expected); err != nil {
				return err
			}
			if count != expected {
				return &Error{Code: "integrity", Table: p.Name, Count: expected - count}
			}
		}
		return w.Close()
	})
	return classify(err)
}

// Restore validates and inserts in one transaction. Footer failure, disconnect,
// mismatched identity, unsupported state or integrity errors roll back all rows.
// A matching committed receipt is a no-op even if the target has since advanced.
func Restore(ctx context.Context, database *db.DB, id merchant.ID, in io.Reader) (Result, error) {
	var result Result
	if database == nil || id.IsZero() {
		return result, &Error{Code: "merchant_mismatch"}
	}
	ctx = merchant.WithID(ctx, id)
	err := database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := scope(ctx, tx, id); err != nil {
			return err
		}
		if err := checkSchema(ctx, tx); err != nil {
			return err
		}
		var previousDigest *string
		var previousRows *int64
		info, err := contract.Read(in, func(h archivewire.Header) error {
			if h.MerchantID != id.String() {
				return &Error{Code: "merchant_mismatch"}
			}
			var receipt string
			if err := tx.QueryRow(ctx, "SELECT openrails.begin_billing_restore($1)::text", id.UUID()).Scan(&receipt); err != nil {
				return err
			}
			return tx.QueryRow(ctx, "SELECT summary->>'digest',(summary->>'rows')::bigint FROM openrails.maintenance_runs WHERE merchant_id=$1 AND id=$2::uuid", id.UUID(), receipt).Scan(&previousDigest, &previousRows)
		}, func(p contract.Profile, values []*string) error {
			if previousDigest != nil {
				return nil
			}
			args := make([]any, len(values))
			for i, v := range values {
				if v != nil {
					args[i] = *v
				}
			}
			_, err := tx.Exec(ctx, insertQuery(p), args...)
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
		if err := validateReferences(ctx, tx, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "SELECT openrails.finish_billing_restore($1,$2,$3)", id.UUID(), info.Digest, info.Rows)
		return err
	})
	if err != nil {
		return Result{}, classify(err)
	}
	return result, nil
}

func scope(ctx context.Context, tx pgx.Tx, id merchant.ID) error {
	if _, err := tx.Exec(ctx, "SELECT set_config('app.merchant_id',$1,true)", id.String()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL TIME ZONE 'UTC'; SET LOCAL DateStyle TO 'ISO, YMD'; SET LOCAL IntervalStyle TO 'iso_8601'; SET LOCAL extra_float_digits TO 3"); err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM openrails.merchants WHERE id=$1 AND status='active' AND deleted_at IS NULL)", id.UUID()).Scan(&active); err != nil {
		return err
	}
	if !active {
		return &Error{Code: "merchant_mismatch"}
	}
	return nil
}

func insertQuery(p contract.Profile) string {
	cols, params := make([]string, len(p.Columns)), make([]string, len(p.Columns))
	for i, c := range p.Columns {
		cols[i] = pgx.Identifier{c.Name}.Sanitize()
		params[i] = fmt.Sprintf("$%d::text::%s", i+1, c.Type)
	}
	return "INSERT INTO openrails." + p.Name + " (" + strings.Join(cols, ",") + ") VALUES (" + strings.Join(params, ",") + ")"
}

func exportQuery(p contract.Profile) string {
	cols := make([]string, len(p.Columns))
	for i, c := range p.Columns {
		expr := "t." + pgx.Identifier{c.Name}.Sanitize()
		switch p.Name + "." + c.Name {
		case "psps.evidence":
			expr = "jsonb_strip_nulls(jsonb_build_object('settings',(t.evidence->'settings')-'rpc_api_key','signer',t.evidence->'signer','public_config',t.evidence->'public_config'))"
		}
		cols[i] = "(" + expr + ")::text"
	}
	from := "openrails." + p.Name + " t"
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
		prefix = "WITH RECURSIVE lineage AS (SELECT id,0 depth FROM openrails." + p.Name + " WHERE merchant_id=$1 AND " + parent + " IS NULL UNION ALL SELECT c.id,l.depth+1 FROM openrails." + p.Name + " c JOIN lineage l ON c." + parent + "=l.id WHERE c.merchant_id=$1) "
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
