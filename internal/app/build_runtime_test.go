package app

import (
	"testing"

	"github.com/riverqueue/river"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestCreateCCBillDataLinkClientPropagatesTestMode(t *testing.T) {
	t.Parallel()

	testMode := true
	cfg := config.GetDefaultBillingConfig()
	cfg.TestMode = &testMode
	cfg.Processors = map[string]*config.ProcessorConfig{
		"ccbill": {
			ClientAccNum:     "945280",
			ClientSubAcc:     "0001",
			DataLinkUsername: "datalink-user",
			DataLinkPassword: "datalink-pass",
		},
	}

	client := createCCBillDataLinkClient(cfg)
	require.NotNil(t, client)
	require.True(t, client.DevMode)
}

// TestStandaloneRiverSchemaTracksDBSchema verifies the issue #165 standalone rule:
// the schema OpenRails hands to its self-constructed River client is exactly the
// configured OpenRails Postgres schema (db.schema), defaulting to `billing`.
func TestStandaloneRiverSchemaTracksDBSchema(t *testing.T) {
	t.Parallel()

	t.Run("default is billing", func(t *testing.T) {
		cfg := config.GetDefaultBillingConfig()
		require.Equal(t, "billing", standaloneRiverSchema(cfg))
		// Standalone River schema == OpenRails DB schema.
		require.Equal(t, cfg.DB.SchemaName(), standaloneRiverSchema(cfg))
	})

	t.Run("custom schema is honored", func(t *testing.T) {
		cfg := config.GetDefaultBillingConfig()
		cfg.DB.Schema = "host_billing"
		require.Equal(t, "host_billing", standaloneRiverSchema(cfg))
		require.Equal(t, cfg.DB.SchemaName(), standaloneRiverSchema(cfg))
	})

	t.Run("nil db falls back to default", func(t *testing.T) {
		require.Equal(t, "billing", standaloneRiverSchema(&config.Config{}))
		require.Equal(t, "billing", standaloneRiverSchema(nil))
	})
}

// TestBuildRiverClientUsesConfiguredSchema proves the configured schema actually
// reaches the constructed River client (not just the helper). pgxpool.New is lazy
// (it parses the URL without dialing), so the client builds without a live DB.
func TestBuildRiverClientUsesConfiguredSchema(t *testing.T) {
	t.Parallel()

	for _, schema := range []string{"billing", "host_billing"} {
		schema := schema
		t.Run(schema, func(t *testing.T) {
			cfg := config.GetDefaultBillingConfig()
			cfg.DB.Schema = schema
			cfg.DB.URL = "postgres://u:p@localhost:5432/db?sslmode=disable"

			client, pool, err := buildRiverClient(cfg, river.NewWorkers())
			require.NoError(t, err)
			t.Cleanup(func() {
				if pool != nil {
					pool.Close()
				}
			})
			require.NotNil(t, client)
			require.Equal(t, schema, client.Schema())
		})
	}
}
