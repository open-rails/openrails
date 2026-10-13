package migrate

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/migratekit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Stored Solana PSPs take their rail's environments in place: live becomes
// mainnet, test and sandbox devnet, each keeping its id (derived from its
// money class, which the rename keeps). Card rails keep their values, and the
// schema then refuses an environment its rail lacks.
func TestPSPRailEnvironmentsMigration(t *testing.T) {
	dsn := os.Getenv("SQLC_DATABASE_URL")
	if dsn == "" {
		t.Skip("SQLC_DATABASE_URL is required for the populated migration test")
	}
	ctx := t.Context()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	schema := "rail_env_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	t.Cleanup(func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		require.NoError(t, err)
	})
	migrations, err := loadMigrations(schema)
	require.NoError(t, err)
	at := -1
	for i, m := range migrations {
		if strings.HasPrefix(m.Name, "0099_") {
			at = i
		}
	}
	require.Positive(t, at, "0099 is in the chain")
	migrator, err := migratekit.NewPostgresFromPGXPool(pool, "openrails")
	require.NoError(t, err)
	defer migrator.Close()
	migrator.WithSchema(schema).WithStrictIntegrity().WithRender(loadMigrations)
	require.NoError(t, migrator.ApplyMigrations(ctx, migrations[:at]))
	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, strings.ReplaceAll(sql, "billing.", schema+"."), args...)
		return err
	}
	merchant := uuid.New()
	require.NoError(t, exec(`INSERT INTO billing.merchants(id,slug,status) VALUES($1,'rail-env','active')`, merchant))
	id := func(rail, class, account string) uuid.UUID {
		return uuidutil.DeterministicID(uuidutil.DeterministicNamespace, rail, class, account)
	}
	rows := []struct{ key, rail, environment, class, account, want string }{
		{"wallet-live", "solana", "live", "live", "SoLive", "mainnet"},
		{"wallet-test", "solana", "test", "test", "SoTest", "devnet"},
		{"wallet-sandbox", "solana", "sandbox", "test", "SoSandbox", "devnet"},
		{"mobius", "nmi", "test", "test", "100001", "test"},
		{"ccbill", "ccbill", "test", "test", "999999-0000", "test"},
		{"stripe", "stripe", "live", "live", "acct_1", "live"},
	}
	for _, r := range rows {
		require.NoError(t, exec(`INSERT INTO billing.psps(id,merchant_id,key,rail,environment,account_id) VALUES($1,$2,$3,$4,$5,$6)`,
			id(r.rail, r.class, r.account), merchant, r.key, r.rail, r.environment, r.account), r.key)
	}

	require.NoError(t, migrator.ApplyMigrations(ctx, migrations))
	for _, r := range rows {
		var environment string
		var live bool
		require.NoError(t, pool.QueryRow(ctx, "SELECT environment, live FROM "+pgx.Identifier{schema, "psps"}.Sanitize()+" WHERE id = $1",
			id(r.rail, r.class, r.account)).Scan(&environment, &live), r.key)
		require.Equal(t, r.want, environment, r.key)
		require.Equal(t, r.class == "live", live, r.key)
	}
	for _, refused := range [][3]string{{"solana", "sandbox", "SoNew"}, {"solana", "live", "SoNew2"}, {"nmi", "devnet", "200002"}} {
		err := exec(`INSERT INTO billing.psps(id,merchant_id,key,rail,environment,account_id) VALUES($1,$2,$3,$4,$5,$6)`,
			uuid.New(), merchant, "k-"+refused[2], refused[0], refused[1], refused[2])
		require.ErrorContains(t, err, "psps_environment_check", "%q", refused)
	}
	err = exec(`INSERT INTO billing.psps(id,merchant_id,key,rail,environment,account_id) VALUES($1,$2,'again','solana','testnet','SoTest')`, uuid.New(), merchant)
	require.ErrorContains(t, err, "psps_rail_live_account_id_key", "devnet and testnet are one money class")
	var pending bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'psps' AND column_name = 'pending_signer_public_key')`, schema).Scan(&pending))
	require.False(t, pending)
}
