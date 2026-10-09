//go:build e2e && integration

package ci_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// Two apps sharing one schema migrate as a privileged role and run as logins
// that inherit a shared owner: Migrate hands everything to that owner, a
// rerun alters nothing, and a login that only inherits the owner runs the
// engine.
func TestMigrateHandsTheSchemaToItsOwner(t *testing.T) {
	ctx := t.Context()
	dsn := strings.TrimSpace(os.Getenv("OPENRAILS_E2E_DSN"))
	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(admin.Close)
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	schema, owner, login := "owned_"+suffix, "owner_"+suffix, "login_"+suffix
	_, err = admin.Exec(ctx, `CREATE ROLE `+pgx.Identifier{owner}.Sanitize()+` NOLOGIN; CREATE ROLE `+pgx.Identifier{login}.Sanitize()+` LOGIN PASSWORD 'login' IN ROLE `+pgx.Identifier{owner}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, `DROP SCHEMA IF EXISTS `+pgx.Identifier{schema}.Sanitize()+` CASCADE`)
		_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS `+pgx.Identifier{login}.Sanitize())
		_, _ = admin.Exec(ctx, `DROP ROLE IF EXISTS `+pgx.Identifier{owner}.Sanitize())
	})

	cfg := openrails.Config{Schema: schema, RiverSchema: schema, SchemaOwner: "nobody_" + suffix}
	require.ErrorContains(t, openrails.Migrate(ctx, admin, cfg), "does not exist", "roles are infrastructure, never created")

	cfg.SchemaOwner = owner
	require.NoError(t, openrails.Migrate(ctx, admin, cfg))
	notOwned := func() int {
		var n int
		require.NoError(t, admin.QueryRow(ctx, `
			SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1 AND c.relowner <> $2::regrole)
			     + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = $1 AND p.proowner <> $2::regrole)
			     + (SELECT count(*) FROM pg_namespace WHERE nspname = $1 AND nspowner <> $2::regrole)`, schema, owner).Scan(&n))
		return n
	}
	require.Zero(t, notOwned(), "every object is the owner's")

	// A rerun takes no ownership lock: it holds while another session holds
	// a table lock that ALTER … OWNER would wait on.
	locker, err := admin.Begin(ctx)
	require.NoError(t, err)
	_, err = locker.Exec(ctx, `LOCK TABLE `+pgx.Identifier{schema, "merchants"}.Sanitize()+` IN ACCESS SHARE MODE`)
	require.NoError(t, err)
	quick, cancel := context.WithTimeout(ctx, 20*time.Second)
	require.NoError(t, openrails.Migrate(quick, admin, cfg))
	cancel()
	require.NoError(t, locker.Rollback(ctx))

	// The login owns nothing itself but runs the engine through the owner.
	config, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	config.ConnConfig.User, config.ConnConfig.Password = login, "login"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	var merchants int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{schema, "merchants"}.Sanitize()).Scan(&merchants))
	_, err = pool.Exec(ctx, `ALTER TABLE `+pgx.Identifier{schema, "merchants"}.Sanitize()+` ADD COLUMN owner_probe int`)
	require.NoError(t, err, "the owner's privileges reach the login")
	_, err = pool.Exec(ctx, `ALTER TABLE `+pgx.Identifier{schema, "merchants"}.Sanitize()+` DROP COLUMN owner_probe`)
	require.NoError(t, err)
}
