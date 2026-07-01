//go:build integration

package subscriptions

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
)

const providerAccountClientSchema = `
CREATE SCHEMA IF NOT EXISTS openrails;
CREATE TABLE IF NOT EXISTS openrails.payment_provider_accounts (
    id uuid DEFAULT gen_random_uuid() NOT NULL,
    merchant_id uuid NOT NULL,
    rail text NOT NULL,
    environment text DEFAULT 'live' NOT NULL,
    account_id text NOT NULL,
    display_name text,
    archived boolean DEFAULT false NOT NULL,
    evidence jsonb,
    first_seen_at timestamptz DEFAULT current_timestamp NOT NULL,
    last_verified_at timestamptz,
    replaced_at timestamptz,
    created_at timestamptz DEFAULT current_timestamp NOT NULL,
    updated_at timestamptz DEFAULT current_timestamp NOT NULL,
    owner text NOT NULL DEFAULT 'merchant',
    PRIMARY KEY (id),
    UNIQUE (rail, environment, account_id)
);
`

func newProviderAccountClientDB(t *testing.T) *db.DB {
	t.Helper()
	ctx := context.Background()
	var pool *pgxpool.Pool
	if dsn := strings.TrimSpace(os.Getenv("OPENRAILS_TEST_DB_DSN")); dsn != "" {
		adminCfg, err := pgxpool.ParseConfig(dsn)
		require.NoError(t, err)
		adminCfg.ConnConfig.Config.Database = "postgres"
		adminPool, err := pgxpool.NewWithConfig(ctx, adminCfg)
		require.NoError(t, err)
		dbName := fmt.Sprintf("openrails_provider_account_clients_%d", time.Now().UnixNano())
		_, err = adminPool.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{dbName}.Sanitize())
		require.NoError(t, err)
		testCfg, err := pgxpool.ParseConfig(dsn)
		require.NoError(t, err)
		testCfg.ConnConfig.Config.Database = dbName
		pool, err = pgxpool.NewWithConfig(ctx, testCfg)
		require.NoError(t, err)
		t.Cleanup(func() {
			pool.Close()
			_, _ = adminPool.Exec(context.Background(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", dbName)
			_, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{dbName}.Sanitize())
			adminPool.Close()
		})
	} else {
		container, err := postgres.Run(ctx,
			"postgres:18-alpine",
			postgres.WithDatabase("openrails"),
			postgres.WithUsername("test"),
			postgres.WithPassword("test"),
			dbtest.WithPostgresLimits(),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").
					WithOccurrence(2).WithStartupTimeout(60*time.Second)),
		)
		require.NoError(t, err)
		t.Cleanup(func() { _ = container.Terminate(context.Background()) })
		dsn, err := container.ConnectionString(ctx, "sslmode=disable")
		require.NoError(t, err)
		pool, err = pgxpool.New(ctx, dsn)
		require.NoError(t, err)
		t.Cleanup(pool.Close)
	}
	_, err := pool.Exec(ctx, providerAccountClientSchema)
	require.NoError(t, err)
	appDB, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)
	return appDB
}

func TestExistingSubscriptionUsesArchivedProviderAccountClient(t *testing.T) {
	ctx := context.Background()
	appDB := newProviderAccountClientDB(t)
	merchantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	const archivedAccountID = "archived-nmi-account"
	const activeAccountID = "active-nmi-account"
	var archivedProviderRowID uuid.UUID
	require.NoError(t, appDB.Pool().QueryRow(ctx, `
		INSERT INTO openrails.payment_provider_accounts (merchant_id, rail, environment, account_id, archived)
		VALUES ($1, 'nmi', 'live', $2, true)
		RETURNING id
	`, merchantID, archivedAccountID).Scan(&archivedProviderRowID))
	_, err := appDB.Pool().Exec(ctx, `
		INSERT INTO openrails.payment_provider_accounts (merchant_id, rail, environment, account_id, archived)
		VALUES ($1, 'nmi', 'live', $2, false)
	`, merchantID, activeAccountID)
	require.NoError(t, err)

	activeClient := &nmi.NMIClient{}
	archivedClient := &nmi.NMIClient{}
	clients := map[string]*nmi.NMIClient{
		"nmi":             activeClient,
		activeAccountID:   activeClient,
		archivedAccountID: archivedClient,
	}

	client, key, ok, err := NMIClientForExistingSubscription(ctx, appDB, clients, &models.Subscription{
		Rail:              models.RailNMI,
		ProviderAccountID: &archivedProviderRowID,
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, archivedAccountID, key)
	require.Same(t, archivedClient, client)

	client, key, ok, err = NMIClientForExistingSubscription(ctx, appDB, clients, &models.Subscription{Rail: models.RailNMI})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "nmi", key)
	require.Same(t, activeClient, client)
}
