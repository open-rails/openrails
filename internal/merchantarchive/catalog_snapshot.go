package merchantarchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
)

// ExportCatalog writes all persisted catalog rows, including archived revisions
// and original IDs. It never exports credentials, customer records, payments or
// provider jobs. Referenced PSP/customer identities are restore prerequisites.
func ExportCatalog(ctx context.Context, database *db.DB, id billing.MerchantID, out io.Writer) error {
	if database == nil || id.IsZero() {
		return &Error{Code: "merchant_mismatch"}
	}
	ctx = merchant.WithID(ctx, id)
	document := CatalogSnapshot{Kind: "catalog_snapshot", SchemaVersion: 1, MerchantID: id.String(), Tables: map[string][]map[string]json.RawMessage{}}
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
		var err error
		document.CatalogRevision, err = gen.New(tx).GetCatalogRevision(ctx, id.UUID())
		if err != nil {
			return err
		}
		if err := validateCatalogReferences(ctx, tx, id, document.CatalogRevision); err != nil {
			return err
		}
		var exportBytes, exportRows int
		for _, p := range catalogProfiles {
			document.Tables[p.Name] = []map[string]json.RawMessage{}
			rows, err := tx.Query(ctx, exportQuery(p), id.UUID())
			if err != nil {
				return err
			}
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
				exportRows++
				// Bound accumulated rows before building the YAML tree. Column/map
				// overhead is charged as well as their encoded scalar contents.
				exportBytes += 128 + len(values)*64
				for _, value := range values {
					if value != nil {
						exportBytes += len(*value)
					}
				}
				if exportBytes > CatalogSnapshotMaxBytes || exportRows > 100000 {
					rows.Close()
					return &Error{Code: "unsupported_state", Table: p.Name, Err: fmt.Errorf("catalog snapshot exceeds export size limit")}
				}
				if err := contract.ValidateValues(p, values); err != nil {
					rows.Close()
					return &Error{Code: "unsupported_state", Table: p.Name, Err: err}
				}
				row, err := catalogRow(p, values)
				if err != nil {
					rows.Close()
					return err
				}
				document.Tables[p.Name] = append(document.Tables[p.Name], row)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		document.Dependencies, err = catalogDependencies(ctx, tx, id, document)
		return err
	})
	if err != nil {
		return classify(err)
	}
	return WriteCatalogSnapshot(out, document)
}

func catalogDependencyIDs(document CatalogSnapshot) ([]string, []string, error) {
	psps, customers := map[string]bool{}, map[string]bool{}
	for table, field := range map[string]string{"price_psp_bindings": "psp_id", "catalog_rate_cards": "customer_id"} {
		for _, row := range document.Tables[table] {
			raw, ok := row[field]
			if !ok {
				continue
			}
			var id string
			if err := json.Unmarshal(raw, &id); err != nil {
				return nil, nil, fmt.Errorf("invalid %s dependency", table)
			}
			if table == "price_psp_bindings" {
				psps[id] = true
			} else {
				customers[id] = true
			}
		}
	}
	keys := func(m map[string]bool) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return keys(psps), keys(customers), nil
}
func catalogDependencies(ctx context.Context, tx pgx.Tx, id billing.MerchantID, document CatalogSnapshot) (CatalogDependencies, error) {
	out := CatalogDependencies{PSPs: []CatalogPSPIdentity{}, Customers: []string{}}
	psps, customers, err := catalogDependencyIDs(document)
	if err != nil {
		return out, err
	}
	for _, psp := range psps {
		value := CatalogPSPIdentity{ID: psp}
		err := tx.QueryRow(ctx, `SELECT rail,environment,account_id FROM billing.psps WHERE merchant_id=$1 AND id=$2`, id.UUID(), psp).Scan(&value.Rail, &value.Environment, &value.AccountID)
		if err != nil {
			return out, fmt.Errorf("catalog PSP prerequisite %s: %w", psp, err)
		}
		out.PSPs = append(out.PSPs, value)
	}
	out.Customers = customers
	return out, nil
}
func verifyCatalogDependencies(ctx context.Context, tx pgx.Tx, id billing.MerchantID, document CatalogSnapshot) error {
	psps, customers, err := catalogDependencyIDs(document)
	if err != nil {
		return err
	}
	if len(psps) != len(document.Dependencies.PSPs) || len(customers) != len(document.Dependencies.Customers) {
		return &Error{Code: "invalid_artifact", Err: fmt.Errorf("catalog prerequisite inventory mismatch")}
	}
	expected := map[string]CatalogPSPIdentity{}
	for _, value := range document.Dependencies.PSPs {
		if _, ok := expected[value.ID]; ok {
			return &Error{Code: "invalid_artifact", Err: fmt.Errorf("duplicate PSP prerequisite")}
		}
		expected[value.ID] = value
	}
	for _, psp := range psps {
		want, ok := expected[psp]
		if !ok {
			return &Error{Code: "invalid_artifact", Err: fmt.Errorf("missing PSP prerequisite")}
		}
		got := CatalogPSPIdentity{ID: psp}
		err := tx.QueryRow(ctx, `SELECT rail,environment,account_id FROM billing.psps WHERE merchant_id=$1 AND id=$2 FOR SHARE`, id.UUID(), psp).Scan(&got.Rail, &got.Environment, &got.AccountID)
		if err != nil || want != got {
			return &Error{Code: "dependency_mismatch", Table: "psps", Err: err}
		}
	}
	seen := map[string]bool{}
	for _, customer := range document.Dependencies.Customers {
		if seen[customer] {
			return &Error{Code: "invalid_artifact", Err: fmt.Errorf("duplicate customer prerequisite")}
		}
		seen[customer] = true
	}
	for _, customer := range customers {
		if !seen[customer] {
			return &Error{Code: "invalid_artifact", Err: fmt.Errorf("missing customer prerequisite")}
		}
		var found string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM billing.customers WHERE merchant_id=$1 AND id=$2 FOR SHARE`, id.UUID(), customer).Scan(&found); err != nil {
			return &Error{Code: "dependency_mismatch", Table: "customers", Err: err}
		}
	}
	return nil
}

// RestoreCatalog inserts a sealed snapshot into an empty catalog for the same
// already provisioned merchant. It never replaces existing catalog rows. The
// identical artifact replays forever, including after subsequent catalog edits.
func RestoreCatalog(ctx context.Context, database *db.DB, id billing.MerchantID, in io.Reader) (Result, error) {
	if database == nil || id.IsZero() {
		return Result{}, &Error{Code: "merchant_mismatch"}
	}
	signed, count, err := readCatalogSnapshot(in)
	if err != nil {
		return Result{}, &Error{Code: "invalid_artifact", Err: err}
	}
	document, err := upgradeCatalogSnapshot(signed)
	if err != nil {
		return Result{}, &Error{Code: "invalid_artifact", Err: err}
	}
	if document.MerchantID != id.String() {
		return Result{}, &Error{Code: "merchant_mismatch"}
	}
	result := Result{MerchantID: id.String(), Digest: document.SHA256, Rows: count}
	ctx = merchant.WithID(ctx, id)
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := scope(ctx, tx, id); err != nil {
			return err
		}
		if err := checkSchema(ctx, tx); err != nil {
			return err
		}
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT catalog_revision FROM billing.merchants WHERE id=$1 FOR UPDATE`, id.UUID()).Scan(&revision); err != nil {
			return err
		}
		var digest string
		var priorCount int64
		err := tx.QueryRow(ctx, `SELECT digest,rows FROM billing.catalog_restore_receipts WHERE merchant_id=$1`, id.UUID()).Scan(&digest, &priorCount)
		if err == nil {
			if digest != document.SHA256 || priorCount != count {
				return &Error{Code: "not_empty"}
			}
			result.Replayed = true
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if revision != 0 {
			return &Error{Code: "not_empty"}
		}
		for _, p := range catalogProfiles {
			var rows int64
			if err := tx.QueryRow(ctx, countQuery(p), id.UUID()).Scan(&rows); err != nil {
				return err
			}
			if rows != 0 {
				return &Error{Code: "not_empty", Table: p.Name, Count: rows}
			}
		}
		if err := verifyCatalogDependencies(ctx, tx, id, document); err != nil {
			return err
		}
		q := gen.New(tx)
		if err := q.SetCatalogBatchMerchant(ctx, id.String()); err != nil {
			return err
		}
		for _, p := range catalogProfiles {
			for _, row := range document.Tables[p.Name] {
				values, err := catalogValues(p, row, id.String())
				if err != nil {
					return err
				}
				args := make([]any, len(values))
				for i, v := range values {
					if v != nil {
						args[i] = *v
					}
				}
				if _, err := tx.Exec(ctx, insertQuery("billing."+p.Name, p), args...); err != nil {
					return err
				}
			}
		}
		if err := q.SetCatalogRevision(ctx, gen.SetCatalogRevisionParams{MerchantID: id.UUID(), Revision: document.CatalogRevision}); err != nil {
			return err
		}
		if err := validateCatalogReferences(ctx, tx, id, document.CatalogRevision); err != nil {
			return err
		}

		_, err = tx.Exec(ctx, `INSERT INTO billing.catalog_restore_receipts(merchant_id,digest,rows) VALUES($1,$2,$3)`, id.UUID(), document.SHA256, count)
		return err
	})
	if err != nil {
		return Result{}, classify(err)
	}
	return result, nil
}

func validateCatalogReferences(ctx context.Context, tx pgx.Tx, id billing.MerchantID, revision int64) error {
	invalid, err := gen.New(tx).CatalogApplicationsAfterRevisionExist(ctx, gen.CatalogApplicationsAfterRevisionExistParams{MerchantID: id.UUID(), Revision: revision})
	if err != nil {
		return err
	}
	if invalid {
		return &Error{Code: "integrity", Table: "catalog_applications"}
	}
	// Current available offers share the full billing archive's credit contract;
	// archived price terms remain historical facts, independent of current benefits.
	for _, check := range referenceChecks {
		if check.table == "prices" {
			if err := refuseRows(ctx, tx, id, check.table, check.predicate); err != nil {
				return err
			}
		}
	}
	return nil
}
