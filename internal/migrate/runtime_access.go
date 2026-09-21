package migrate

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	postgresmigrations "github.com/open-rails/openrails/internal/migrate/postgres"
)

//go:embed runtime_access.sql
var runtimeAccessSQL string

// runtimeLogin validates the supplied connection before schema mutation.
func runtimeLogin(ctx context.Context, owner, runtime *pgxpool.Pool) (string, error) {
	var user, runtimeDB, ownerDB string
	if err := runtime.QueryRow(ctx, "SELECT current_user, current_database()").Scan(&user, &runtimeDB); err != nil {
		return "", fmt.Errorf("identify runtime login: %w", err)
	}
	if err := owner.QueryRow(ctx, "SELECT current_database()").Scan(&ownerDB); err != nil {
		return "", err
	}
	if runtimeDB != ownerDB {
		return "", fmt.Errorf("runtime database %q differs from migration database %q", runtimeDB, ownerDB)
	}
	return user, nil
}

// provisionRuntimeAccess grants only library runtime operations. It never
// creates roles, changes credentials, or grants membership.
func provisionRuntimeAccess(ctx context.Context, owner *pgxpool.Pool, user, schema, riverSchema string, hostRiver bool) error {
	sql, err := postgresmigrations.RewriteSchema(runtimeAccessSQL, schema)
	if err != nil {
		return err
	}
	sql = strings.ReplaceAll(sql, `:"runtime_user"`, pgx.Identifier{user}.Sanitize())
	tx, err := owner.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // committed transactions are already closed
	// AuthKit uses the same database-wide protocol. Both libraries can grant
	// schema public access, so separate schema locks would race on its ACL tuple.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended('open-rails:runtime-access',0))"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		return fmt.Errorf("billing privileges: %w", err)
	}
	if !hostRiver {
		role := pgx.Identifier{user}.Sanitize()
		if _, err := tx.Exec(ctx, "GRANT USAGE ON SCHEMA "+pgx.Identifier{riverSchema}.Sanitize()+" TO "+role); err != nil {
			return err
		}
		for _, table := range []string{"river_job", "river_queue", "river_leader", "river_notification"} {
			if _, err := tx.Exec(ctx, "GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE "+pgx.Identifier{riverSchema, table}.Sanitize()+" TO "+role); err != nil {
				return err
			}
		}
		for _, sequence := range []string{"river_job_id_seq", "river_notification_id_seq"} {
			if _, err := tx.Exec(ctx, "GRANT USAGE, SELECT, UPDATE ON SEQUENCE "+pgx.Identifier{riverSchema, sequence}.Sanitize()+" TO "+role); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}
