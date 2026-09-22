//go:build integration

package pgidentity_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/pgidentity"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

func poolFor(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestIdentitySamePoolWithOnlyConnectionInTransaction(t *testing.T) {
	pool := poolFor(t, dbtest.SharedPostgresDSN(t))
	tx, err := pool.Begin(t.Context())
	require.NoError(t, err)
	defer tx.Rollback(context.Background()) //nolint:errcheck // test owns the transaction
	require.NoError(t, pgidentity.RequireSameDatabase(t.Context(), pool, pool))
	var one int
	require.NoError(t, tx.QueryRow(t.Context(), "SELECT 1").Scan(&one))
	require.Equal(t, 1, one, "identity checks do not consume an ambient transaction")
}

func TestIdentityDifferentRolesAndAliases(t *testing.T) {
	owner := poolFor(t, dbtest.SharedSuperuserDSN(t))
	cfg, err := pgxpool.ParseConfig(dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	// Exercise a different connection alias, not a textual host comparison.
	switch cfg.ConnConfig.Host {
	case "127.0.0.1":
		cfg.ConnConfig.Host = "localhost"
	case "localhost":
		cfg.ConnConfig.Host = "127.0.0.1"
	default:
		t.Skip("loopback allocator required for connection-alias control")
	}
	cfg.MaxConns = 1
	runtime, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(runtime.Close)
	require.NoError(t, pgidentity.RequireSameDatabase(t.Context(), owner, runtime))
	require.NoError(t, pgidentity.RequireSameDatabase(t.Context(), runtime, owner))
	require.NoError(t, owner.Ping(t.Context()))
	require.NoError(t, runtime.Ping(t.Context()), "neither host pool was closed")
}

func databaseFor(t *testing.T, admin *pgxpool.Pool, name string) *pgxpool.Pool {
	t.Helper()
	_, err := admin.Exec(t.Context(), "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		require.NoError(t, err)
	})
	cfg := admin.Config().Copy()
	cfg.ConnConfig.Database = name
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool
}

func TestIdentityDifferentDatabaseWithSameQueueSchema(t *testing.T) {
	owner := poolFor(t, dbtest.SharedSuperuserDSN(t))
	other := databaseFor(t, owner, "identity_"+strings.ReplaceAll(uuid.NewString(), "-", ""))
	// The matching table name deliberately cannot serve as database identity.
	_, err := other.Exec(t.Context(), "CREATE TABLE public.river_job(id bigint)")
	require.NoError(t, err)
	require.ErrorIs(t, pgidentity.RequireSameDatabase(t.Context(), owner, other), pgidentity.ErrDifferentDatabase)
}

func TestIdentityDifferentClusterWithSameDatabaseName(t *testing.T) {
	dsn := os.Getenv("OPENRAILS_IDENTITY_OTHER_CLUSTER_DSN")
	if dsn == "" {
		dsn = dbtest.IsolatedPostgresDSN(t)
	}
	owner := poolFor(t, dbtest.SharedSuperuserDSN(t))
	otherAdmin := poolFor(t, dsn)
	var name string
	require.NoError(t, owner.QueryRow(t.Context(), "SELECT current_database()").Scan(&name))
	other := databaseFor(t, otherAdmin, name)
	require.ErrorIs(t, pgidentity.RequireSameDatabase(t.Context(), owner, other), pgidentity.ErrDifferentDatabase)
}

type proofStartedKey struct{}
type proofTracer struct {
	held chan struct{}
	once sync.Once
}

func (p *proofTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, proofStartedKey{}, strings.Contains(data.SQL, "pg_try_advisory_xact_lock"))
}
func (p *proofTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if yes, _ := ctx.Value(proofStartedKey{}).(bool); yes && data.Err == nil {
		p.once.Do(func() { close(p.held) })
	}
}

func TestIdentityCancellationReleasesProofAndConnections(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(dbtest.SharedPostgresDSN(t))
	require.NoError(t, err)
	cfg.MaxConns = 1
	trace := &proofTracer{held: make(chan struct{})}
	cfg.ConnConfig.Tracer = trace
	owner, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(owner.Close)
	other := poolFor(t, dbtest.SharedPostgresDSN(t))
	held, err := other.Acquire(t.Context())
	require.NoError(t, err)
	var pid int32
	require.NoError(t, owner.QueryRow(t.Context(), "SELECT pg_backend_pid()").Scan(&pid))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- pgidentity.RequireSameDatabase(ctx, owner, other) }()
	<-trace.held
	cancel()
	err = <-result
	held.Release()
	require.True(t, errors.Is(err, context.Canceled), "%v", err)
	var locks int
	require.NoError(t, owner.QueryRow(t.Context(), "SELECT count(*) FROM pg_catalog.pg_locks WHERE locktype='advisory' AND pid=$1", pid).Scan(&locks))
	require.Zero(t, locks)
	require.Zero(t, owner.Stat().AcquiredConns())
	require.Zero(t, other.Stat().AcquiredConns())
	require.NoError(t, other.Ping(t.Context()))
}
