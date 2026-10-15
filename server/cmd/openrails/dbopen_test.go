package main

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/server/internal/hostconfig"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

func TestMigrateRequiresPlatformTenantBeforeDatabase(t *testing.T) {
	err := applyStandaloneMigrations(t.Context(), &hostconfig.Config{
		Config: &config.Config{DB: &config.DBConfig{URL: "postgres://unreachable.invalid/db"}},
	})
	require.ErrorContains(t, err, "auth.default_tenant_id")
}

func TestCommandDatabaseFailureDoesNotRetryOrPrintStartup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := listener.Addr().String()
	require.NoError(t, listener.Close())
	var output bytes.Buffer
	old := log.StandardLogger().Out
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(old) })
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	database, err := openCLIDB(ctx, &config.Config{DB: &config.DBConfig{URL: "postgres://fixture@" + address + "/fixture?sslmode=disable"}})
	require.Error(t, err)
	require.Nil(t, database)
	require.NoError(t, ctx.Err(), "a connection refusal returns immediately instead of consuming the whole retry budget")
	require.Empty(t, output.String(), "the command returns one error instead of logging a server retry loop")
}

func TestAdminLockoutRequiresSharedRedis(t *testing.T) {
	cmd := newAdminLockoutsCmd()
	cmd.SetContext(context.WithValue(t.Context(), config.ConfigContextKey, &config.Config{}))
	out, err := execute(cmd, "unlock", uuid.NewString())
	require.ErrorContains(t, err, "Redis is required")
	require.NotContains(t, out, "unlocked", "a fresh process-local store cannot report unlocking a serving process")
}
