//go:build integration

package embed

import (
	"context"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNewAcceptsOwnerAndRuntimeConnections(t *testing.T) {
	ownerDSN, runtimeDSN := dbtest.SharedRLSPostgres(t)
	for name, dsn := range map[string]string{"owner": ownerDSN, "runtime": runtimeDSN} {
		t.Run(name, func(t *testing.T) {
			pool, err := pgxpool.New(t.Context(), dsn)
			require.NoError(t, err)
			t.Cleanup(pool.Close)
			cfg := &config.Config{Env: "staging", TestMode: config.CredentialPostureLive, ProviderWriteMode: config.ProviderWriteModeReadOnly, DB: &config.DBConfig{URL: dsn}}
			runtime, err := New(t.Context(), Options{Config: cfg, PGXPool: pool, River: RiverManagedByOpenRails()})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, runtime.Close(context.Background())) })
		})
	}
}
