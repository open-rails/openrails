package app

import (
	"context"

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

// ----------------------------------------------------------------------------
// #360 Solana token pricing policy: boot must NEVER fail over token pricing.
// Incident: the doujins stack crash-looped with "solana token USD1 missing
// pyth price feed" — defaults injected AFTER config.Validate hit #352's hard
// feed check. These tests pin the degrade-not-die policy matrix.
// ----------------------------------------------------------------------------

func solanaCfg(t *testing.T, testEnv bool, tokens map[string]config.TokenConfig) *config.Config {
	t.Helper()
	cfg := config.GetDefaultBillingConfig()
	cfg.Mode = config.ModeFull
	cfg.TestEnv = testEnv
	cfg.Processors = map[string]*config.ProcessorConfig{
		"solana": {Tokens: tokens},
	}
	return cfg
}

func TestConfigureSolanaProcessorPolicyMatrix(t *testing.T) {
	t.Parallel()

	t.Run("devnet: unknown token without feed is kept (no pricing requirements)", func(t *testing.T) {
		cfg := solanaCfg(t, true, map[string]config.TokenConfig{
			"WEIRD": {Name: "Weird Devnet Coin", Mint: "WeirdMint1111111111111111111111111111111111", Decimals: 6},
		})
		require.NoError(t, configureSolanaProcessor(cfg))
		proc := cfg.GetSolanaProcessor()
		require.Equal(t, "devnet", proc.Network)
		require.Contains(t, proc.Tokens, "WEIRD")
	})

	t.Run("mainnet: USD-pegged stablecoin without feed degrades to parity, stays enabled", func(t *testing.T) {
		// USDT is in the stablecoin registry (USD peg) but has no built-in feed.
		cfg := solanaCfg(t, false, map[string]config.TokenConfig{
			"USDT": {Name: "Tether", Mint: "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", Decimals: 6},
		})
		require.NoError(t, configureSolanaProcessor(cfg))
		proc := cfg.GetSolanaProcessor()
		require.Equal(t, "mainnet", proc.Network)
		require.Contains(t, proc.Tokens, "USDT")
	})

	t.Run("mainnet: non-USD-pegged stablecoin without feed is disabled, boot succeeds", func(t *testing.T) {
		cfg := solanaCfg(t, false, map[string]config.TokenConfig{
			"EURC": {Name: "Euro Coin", Mint: "HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr", Decimals: 6},
			"USDC": {Name: "USD Coin", Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Decimals: 6},
		})
		require.NoError(t, configureSolanaProcessor(cfg))
		proc := cfg.GetSolanaProcessor()
		require.NotContains(t, proc.Tokens, "EURC", "EURC cannot default to USD parity")
		require.Contains(t, proc.Tokens, "USDC", "other tokens keep working")
	})

	t.Run("mainnet: unknown token without feed is disabled, boot succeeds", func(t *testing.T) {
		cfg := solanaCfg(t, false, map[string]config.TokenConfig{
			"NOPE": {Name: "Mystery Coin", Mint: "NopeMint1111111111111111111111111111111111", Decimals: 6},
			"SOL":  {Name: "Solana", Mint: "So11111111111111111111111111111111111111112", Decimals: 9},
		})
		require.NoError(t, configureSolanaProcessor(cfg))
		proc := cfg.GetSolanaProcessor()
		require.NotContains(t, proc.Tokens, "NOPE")
		require.Contains(t, proc.Tokens, "SOL")
	})

	t.Run("mainnet: feed-backed tokens are kept (feed pricing)", func(t *testing.T) {
		cfg := solanaCfg(t, false, nil) // empty -> defaults injected at runtime
		require.NoError(t, configureSolanaProcessor(cfg))
		proc := cfg.GetSolanaProcessor()
		// Incident regression: the full mainnet default set (incl. USD1/USDG,
		// which now have verified Pyth feeds) must boot.
		for _, sym := range []string{"SOL", "USDC", "PYUSD", "USD1", "USDG"} {
			require.Contains(t, proc.Tokens, sym)
		}
	})

	t.Run("devnet: empty token map defaults to devnet set and boots", func(t *testing.T) {
		cfg := solanaCfg(t, true, nil)
		require.NoError(t, configureSolanaProcessor(cfg))
		proc := cfg.GetSolanaProcessor()
		require.Equal(t, "devnet", proc.Network)
		for _, sym := range []string{"SOL", "USDC", "PYUSD"} {
			require.Contains(t, proc.Tokens, sym)
		}
	})

	t.Run("malformed token entries are dropped, never fatal", func(t *testing.T) {
		cfg := solanaCfg(t, false, map[string]config.TokenConfig{
			"":       {Name: "Empty", Mint: "SomeMint", Decimals: 6},
			"NOMINT": {Name: "No Mint", Decimals: 6},
			"BADDEC": {Name: "Bad Decimals", Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Decimals: -1},
			"USDC":   {Name: "USD Coin", Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Decimals: 6},
		})
		require.NoError(t, configureSolanaProcessor(cfg))
		proc := cfg.GetSolanaProcessor()
		require.Len(t, proc.Tokens, 1)
		require.Contains(t, proc.Tokens, "USDC")
	})
}

func TestCreatePythPriceProviderDevnetParity(t *testing.T) {
	t.Parallel()

	t.Run("devnet provider never errors for feedless tokens (no Hermes required)", func(t *testing.T) {
		cfg := solanaCfg(t, true, nil)
		provider, err := createPythPriceProvider(cfg)
		require.NoError(t, err)
		require.NotNil(t, provider)
		// Unknown symbol -> the inner pyth client fails locally on the missing
		// feed (no network call) and the devnet wrapper degrades to parity.
		price, err := provider.PriceUSD(context.Background(), "WEIRD")
		require.NoError(t, err)
		require.Equal(t, 1.0, price)
	})

	t.Run("mainnet provider still errors on unknown feed", func(t *testing.T) {
		cfg := solanaCfg(t, false, nil)
		provider, err := createPythPriceProvider(cfg)
		require.NoError(t, err)
		require.NotNil(t, provider)
		_, err = provider.PriceUSD(context.Background(), "WEIRD")
		require.Error(t, err)
	})
}
