package config

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

// #711: the Solana runtime knobs live in per-merchant rail-account settings.
func TestParseSolanaAccountSettings(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		s, err := ParseSolanaAccountSettings(nil)
		require.NoError(t, err)
		require.True(t, s.IsZero())
	})

	t.Run("full", func(t *testing.T) {
		s, err := ParseSolanaAccountSettings(map[string]any{
			"rpc_provider": "Helius",
			"rpc_api_key":  "key-123",
			"tokens": map[string]any{
				"usdc": map[string]any{"mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "name": "USD Coin"},
			},
			"recipient_wallet": "9hSR6S7WPtxmTojgo6GG3k4yDPecgJY292j7xrsUGWBu", // ignored here, checkout reads it
		})
		require.NoError(t, err)
		require.Equal(t, "helius", s.RPCProvider)
		require.Equal(t, "key-123", s.RPCAPIKey)
		require.Equal(t, "USD Coin", s.Tokens["USDC"].Name)
	})

	// #817: decimals belong to the mint on-chain. A merchant-declared copy could
	// disagree and misprice by 10^n, so the retired key fails loudly.
	t.Run("tokens.<sym>.decimals is rejected", func(t *testing.T) {
		_, err := ParseSolanaAccountSettings(map[string]any{
			"tokens": map[string]any{
				"usdc": map[string]any{"mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", "decimals": float64(6)},
			},
		})
		require.ErrorContains(t, err, "no longer configurable")
		require.ErrorContains(t, err, "read from the SPL mint on-chain")
	})

	t.Run("malformed values fail loudly", func(t *testing.T) {
		_, err := ParseSolanaAccountSettings(map[string]any{"rpc_provider": "quicknode"})
		require.ErrorContains(t, err, "rpc_provider must be helius or public")

		_, err = ParseSolanaAccountSettings(map[string]any{"rpc_provider": "public", "rpc_api_key": "k"})
		require.ErrorContains(t, err, "public cannot use rpc_api_key")

		_, err = ParseSolanaAccountSettings(map[string]any{"tokens": map[string]any{"USDC": map[string]any{"name": "No Mint"}}})
		require.ErrorContains(t, err, "requires mint")

		_, err = ParseSolanaAccountSettings(map[string]any{"tokens": map[string]any{"USDC": map[string]any{"mint": "m", "decimls": 6}}})
		require.ErrorContains(t, err, "unknown field")
	})

	t.Run("unknown keys ignored on read, rejected by strict validation", func(t *testing.T) {
		in := map[string]any{"rpc_provder": "helius"} // typo
		s, err := ParseSolanaAccountSettings(in)
		require.NoError(t, err)
		require.True(t, s.IsZero())
		require.ErrorContains(t, ValidateSolanaAccountSettings(in), "unknown key(s) rpc_provder")
	})
}

func TestSettingIntOverflow(t *testing.T) {
	n, err := settingInt("k", uint64(42))
	require.NoError(t, err)
	require.Equal(t, 42, n)

	_, err = settingInt("k", uint64(math.MaxInt64)+1)
	require.ErrorContains(t, err, "exceeds platform int range")
}

func TestSolanaAccountSettingsApplyTo(t *testing.T) {
	base := &SolanaRailConfig{
		RPCProvider: "helius",
		RPCAPIKey:   "boot-key",
		Tokens:      map[string]TokenConfig{"SOL": {Mint: "So1"}},
		Network:     "devnet",
	}
	overlay := SolanaAccountSettings{
		RPCAPIKey: "store-key",
		Tokens:    map[string]TokenConfig{"USDC": {Mint: "EPj"}},
	}

	out := overlay.ApplyTo(base)
	// Store-wins on declared knobs (#699); undeclared knobs keep the boot value.
	require.Equal(t, "helius", out.RPCProvider)
	require.Equal(t, "store-key", out.RPCAPIKey)
	require.Equal(t, map[string]TokenConfig{"USDC": {Mint: "EPj"}}, out.Tokens)
	require.Equal(t, "devnet", out.Network)

	// base is never mutated.
	require.Equal(t, "boot-key", base.RPCAPIKey)
	require.Contains(t, base.Tokens, "SOL")

	// nil base: standalone, where the store is the only plane.
	out = overlay.ApplyTo(nil)
	require.Equal(t, "store-key", out.RPCAPIKey)

	// empty overlay returns an unchanged copy.
	out = (SolanaAccountSettings{}).ApplyTo(base)
	require.Equal(t, *base, func() SolanaRailConfig { c := *out; c.Tokens = base.Tokens; return c }())
}
