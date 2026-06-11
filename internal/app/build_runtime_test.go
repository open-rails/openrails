package app

import (
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/require"
)

func TestCreateCCBillDataLinkClientPropagatesTestEnv(t *testing.T) {
	t.Parallel()

	cfg := config.GetDefaultBillingConfig()
	cfg.Mode = config.ModeFull
	cfg.TestEnv = true
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
