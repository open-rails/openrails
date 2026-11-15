package migrate

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/migratekit"
	"github.com/open-rails/openrails/internal/config"
)

// The single baseline replaced a chain of numbered migrations. A database that
// applied all of that chain has exactly the baseline's schema: its ledger is
// rewritten to the baseline and nothing runs. One that applied only part of it
// is refused, never migrated from the middle.

//go:embed retired.txt
var retiredFile string

type retiredChain struct {
	release string
	names   map[int64]string
	last    string
}

func loadRetired() (retiredChain, error) {
	chain := retiredChain{names: map[int64]string{}}
	for line := range strings.Lines(retiredFile) {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "release "):
			chain.release = strings.TrimPrefix(line, "release ")
		default:
			seq, err := migratekit.Sequence(line)
			if err != nil {
				return chain, fmt.Errorf("openrails: retired chain: %w", err)
			}
			chain.names[seq] = line
			chain.last = line
		}
	}
	if chain.release == "" || len(chain.names) == 0 {
		return chain, errors.New("openrails: retired chain list is incomplete")
	}
	return chain, nil
}

type ledgerRow struct {
	sequence int64
	filename string
	digest   string
	status   string
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ledger reads schema's rows of OpenRails' migration ledger; none when
// migratekit has not created it yet.
func ledger(ctx context.Context, q querier, schema string, lock bool) ([]ledgerRow, error) {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('public.migrations') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	sql := `SELECT sequence, coalesce(filename, ''), coalesce(content_sha256, ''), status FROM public.migrations
	 WHERE app = $1 AND database = 'postgres' AND schema = $2 ORDER BY sequence`
	if lock {
		sql += ` FOR UPDATE`
	}
	rows, err := q.Query(ctx, sql, config.MigratekitApp, schema)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ledgerRow, error) {
		var l ledgerRow
		err := r.Scan(&l.sequence, &l.filename, &l.digest, &l.status)
		return l, err
	})
}

// retiredState decides what a ledger holds of the retired chain: nothing
// (fresh, or already on the baseline), all of it, or an error naming why it
// cannot be adopted. A row is retired when it names a retired file the current
// chain does not carry; the first file shares its name with the baseline and
// counts as retired only beside them.
func retiredState(rows []ledgerRow, chain retiredChain, current []migratekit.Migration, schema string) (retired bool, err error) {
	carried := func(name string) bool {
		return slices.ContainsFunc(current, func(m migratekit.Migration) bool { return m.Name == name })
	}
	var held, other []ledgerRow
	for _, r := range rows {
		if r.sequence != 1 && chain.names[r.sequence] == r.filename && !carried(r.filename) {
			held = append(held, r)
		} else if r.sequence != 1 {
			other = append(other, r)
		}
	}
	if len(held) == 0 {
		return false, nil
	}
	have := map[int64]bool{}
	for _, r := range rows {
		if r.status != "applied" {
			return true, fmt.Errorf("openrails: schema %q records %s as %s; finish it with OpenRails %s, the last release of the migration chain this build replaced, before starting this build", schema, r.filename, r.status, chain.release)
		}
		if chain.names[r.sequence] == r.filename {
			have[r.sequence] = true
		}
	}
	if len(other) > 0 {
		names := make([]string, len(other))
		for i, r := range other {
			names[i] = r.filename
		}
		return true, fmt.Errorf("openrails: schema %q was built by the migration chain this build replaced but also records %s, which that chain does not include; it cannot be adopted onto the single baseline", schema, strings.Join(names, ", "))
	}
	var missing []string
	for seq, name := range chain.names {
		if !have[seq] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return true, fmt.Errorf("openrails: schema %q applied the migration chain this build replaced only through %s, %d migrations short of its end (%s first); this build starts only from the end of that chain: migrate the database with OpenRails %s, then start this build", schema, held[len(held)-1].filename, len(missing), missing[0], chain.release)
	}
	return true, nil
}

// adoptBaseline rewrites the ledger of a schema that applied the whole retired
// chain to the baseline, with an audit row per change, and refuses one that
// applied part of it. It changes no schema object.
func adoptBaseline(ctx context.Context, pool *pgxpool.Pool, schema string, current []migratekit.Migration) error {
	chain, err := loadRetired()
	if err != nil {
		return err
	}
	rows, err := ledger(ctx, pool, schema, false)
	if err != nil {
		return fmt.Errorf("openrails: read the migration ledger: %w", err)
	}
	if retired, err := retiredState(rows, chain, current, schema); !retired || err != nil {
		return err
	}
	first := slices.IndexFunc(current, func(m migratekit.Migration) bool { return migratekit.Prefix(m.Name) == "1" })
	if first < 0 {
		return errors.New("openrails: the migrations carry no baseline")
	}
	baseline := current[first]
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('openrails-baseline'), hashtext($1))`, schema); err != nil {
			return err
		}
		rows, err := ledger(ctx, tx, schema, true)
		if err != nil {
			return err
		}
		retired, err := retiredState(rows, chain, current, schema)
		if !retired || err != nil {
			return err
		}
		reason := "single baseline replaces the migration chain through " + chain.last
		for _, r := range rows {
			newName, newDigest := "", ""
			if r.sequence == 1 {
				newName, newDigest = baseline.Name, migratekit.ContentDigest(baseline.Content)
				if _, err := tx.Exec(ctx, `UPDATE public.migrations SET filename = $4, content_sha256 = $5, semantic_sha256 = $6
				 WHERE app = $1 AND database = 'postgres' AND schema = $2 AND sequence = $3`,
					config.MigratekitApp, schema, r.sequence, newName, newDigest, migratekit.SemanticContentDigest(baseline.Content)); err != nil {
					return err
				}
			} else if _, err := tx.Exec(ctx, `DELETE FROM public.migrations WHERE app = $1 AND database = 'postgres' AND schema = $2 AND sequence = $3`,
				config.MigratekitApp, schema, r.sequence); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO public.migration_repairs
			 (app, database, schema, sequence, verb, reason, operator, old_filename, old_digest, new_filename, new_digest)
			 VALUES ($1, 'postgres', $2, $3, 'convert', $4, 'openrails', $5, $6, nullif($7, ''), nullif($8, ''))`,
				config.MigratekitApp, schema, r.sequence, reason, r.filename, r.digest, newName, newDigest); err != nil {
				return err
			}
		}
		return nil
	})
}

// checkRetired is adoptBaseline's read-only half for startup validation.
func checkRetired(ctx context.Context, pool *pgxpool.Pool, schema string, current []migratekit.Migration) error {
	chain, err := loadRetired()
	if err != nil {
		return err
	}
	rows, err := ledger(ctx, pool, schema, false)
	if err != nil {
		return err
	}
	retired, err := retiredState(rows, chain, current, schema)
	if err != nil {
		return err
	}
	if retired {
		return fmt.Errorf("schema %q still records the migration chain through %s; openrails.Migrate adopts it onto the single baseline without changing the schema", schema, chain.last)
	}
	return nil
}
