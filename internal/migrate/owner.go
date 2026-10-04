package migrate

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notOwned lists, as ALTER targets, the schema and every table, view,
// sequence, routine and type in it that the role does not own. Indexes, owned
// sequences, row types and array types follow their parent.
const notOwned = `
SELECT kind, name FROM (
	SELECT 0 AS ord, 'SCHEMA' AS kind, quote_ident(n.nspname) AS name
	  FROM pg_namespace n WHERE n.nspname = $1 AND n.nspowner <> $2
	UNION ALL
	SELECT 1, CASE c.relkind WHEN 'v' THEN 'VIEW' WHEN 'm' THEN 'MATERIALIZED VIEW' WHEN 'S' THEN 'SEQUENCE' WHEN 'f' THEN 'FOREIGN TABLE' ELSE 'TABLE' END,
	       format('%I.%I', n.nspname, c.relname)
	  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	 WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f') AND c.relowner <> $2
	   AND (c.relkind <> 'S' OR NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
	                                         AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')))
	UNION ALL
	SELECT 2, CASE p.prokind WHEN 'p' THEN 'PROCEDURE' WHEN 'a' THEN 'AGGREGATE' ELSE 'FUNCTION' END,
	       format('%I.%I(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid))
	  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
	 WHERE n.nspname = $1 AND p.proowner <> $2
	UNION ALL
	SELECT 3, CASE t.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END, format('%I.%I', n.nspname, t.typname)
	  FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace
	 WHERE n.nspname = $1 AND t.typowner <> $2
	   AND (t.typrelid = 0 OR (SELECT c.relkind FROM pg_class c WHERE c.oid = t.typrelid) = 'c')
	   AND NOT EXISTS (SELECT 1 FROM pg_type e WHERE e.typarray = t.oid)
) o ORDER BY ord, name`

// HandOver makes owner the owner of schema and of everything in it, in one
// transaction, and returns what it reassigned. What owner already owns is left
// alone, so a rerun issues no ALTER and takes no lock. Concurrent migrations
// of a shared schema serialize on an advisory lock. The role must exist: roles
// are cluster infrastructure, never created here.
func HandOver(ctx context.Context, pool *pgxpool.Pool, schema, owner string) ([]string, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, errors.New("schema owner is empty")
	}
	var oid uint32
	if err := pool.QueryRow(ctx, `SELECT oid FROM pg_roles WHERE rolname = $1`, owner).Scan(&oid); errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("schema owner role %q does not exist; create it before migrating", owner)
	} else if err != nil {
		return nil, err
	}
	if pending, err := list(ctx, pool, schema, oid); err != nil || len(pending) == 0 {
		return nil, err
	}
	var done []string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '30s'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('openrails:schema-owner:' || $1, 0))`, schema); err != nil {
			return err
		}
		pending, err := list(ctx, tx, schema, oid)
		if err != nil {
			return err
		}
		for _, object := range pending {
			if _, err := tx.Exec(ctx, "ALTER "+object+" OWNER TO "+pgx.Identifier{owner}.Sanitize()); err != nil {
				return fmt.Errorf("ALTER %s OWNER: %w", object, err)
			}
		}
		if left, err := list(ctx, tx, schema, oid); err != nil {
			return err
		} else if len(left) > 0 {
			return fmt.Errorf("still not owned by %s: %s", owner, strings.Join(left, ", "))
		}
		done = pending
		return nil
	})
	return done, err
}

func list(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, schema string, owner uint32) ([]string, error) {
	rows, err := q.Query(ctx, notOwned, schema, owner)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var kind, name string
		err := row.Scan(&kind, &name)
		return kind + " " + name, err
	})
}
