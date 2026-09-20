//go:build integration

package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestMerchantPinnedDSNEncodesStartupOptionSpace(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	got := MerchantPinnedDSN(t, id)
	cfg, err := pgconn.ParseConfig(got)
	require.NoError(t, err)
	require.Equal(t, "-c app.merchant_id="+id.String(), cfg.RuntimeParams["options"])
}

func TestReplaceDSNDatabaseTargetsOwnedDatabase(t *testing.T) {
	for name, original := range map[string]string{
		"URL":           "postgres://test:secret@localhost/original?sslmode=disable",
		"URL overrides": "postgresql://test:secret@localhost/original?sslmode=disable&dbname=foreign&dbname=another",
		"keyword":       "host=localhost user=test password='test secret' dbname=original sslmode=disable",
	} {
		t.Run(name, func(t *testing.T) {
			rewritten, err := replaceDSNDatabase(original, "owned_fixture")
			require.NoError(t, err)
			cfg, err := pgx.ParseConfig(rewritten)
			require.NoError(t, err)
			require.Equal(t, "owned_fixture", cfg.Database)
		})
	}
}

func TestMain(m *testing.M) { RunMain(m) }

func TestExternalDatabaseLifecyclePreservesOtherRuns(t *testing.T) {
	ctx := t.Context()
	// An isolated server keeps the negative control safe: all three databases
	// below belong to this test, even the two standing in for other processes.
	server, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "postgres:18-alpine", ExposedPorts: []string{"5432/tcp"},
			Env:                map[string]string{"POSTGRES_USER": "test", "POSTGRES_PASSWORD": "test", "POSTGRES_DB": "postgres"},
			WaitingFor:         wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
			HostConfigModifier: PostgresHostConfigModifier,
		}, Started: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, server.Terminate(context.Background())) })
	host, err := server.Host(ctx)
	require.NoError(t, err)
	port, err := server.MappedPort(ctx, "5432/tcp")
	require.NoError(t, err)
	dsn := fmt.Sprintf("postgres://test:test@%s:%s/postgres?sslmode=disable", host, port.Port())
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	var peers []*pgx.Conn
	for _, prefix := range []string{"openrails_it_peer", "openrails_bootstrap_peer"} {
		name := fmt.Sprintf("%s_%d", prefix, time.Now().Add(-time.Hour).UnixNano())
		_, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize())
		require.NoError(t, err)
		cfg, err := pgx.ParseConfig(dsn)
		require.NoError(t, err)
		cfg.Database = name
		peer, err := pgx.ConnectConfig(ctx, cfg)
		require.NoError(t, err)
		t.Cleanup(func() { _ = peer.Close(context.Background()) })
		peers = append(peers, peer)
	}
	previousDSN, previousName := sharedExternalAdminDSN, sharedExternalDBName
	defer func() { sharedExternalAdminDSN, sharedExternalDBName = previousDSN, previousName }()
	created, err := createExternalTestDatabase(ctx, dsn+"&dbname="+peers[0].Config().Database)
	require.NoError(t, err)
	cfg, err := pgx.ParseConfig(created)
	require.NoError(t, err)
	require.Equal(t, cfg.Database, sharedExternalDBName)
	owned, err := pgx.Connect(ctx, created)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owned.Close(context.Background()) })
	require.NoError(t, owned.Ping(ctx))
	dropExternalTestDatabase(dsn, cfg.Database)
	var remains bool
	require.NoError(t, admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)", cfg.Database).Scan(&remains))
	require.False(t, remains, "exact owned database must be removed even with a lingering connection")
	for _, peer := range peers {
		require.NoError(t, peer.Ping(ctx), "new fixture and owned cleanup must preserve an older other run's live connection")
		require.NoError(t, admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)", peer.Config().Database).Scan(&remains))
		require.True(t, remains)
	}
}
