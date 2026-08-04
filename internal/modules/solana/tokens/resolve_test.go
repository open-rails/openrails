package tokens

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

// or#881: the mint of a well-known token comes from the registry. A merchant
// selects the symbol; they never re-type the address.

func TestResolveDeclared_RegistrySymbolSelectsWithoutAMint(t *testing.T) {
	t.Parallel()

	out, err := ResolveDeclared("mainnet", map[string]config.TokenConfig{"usdc": {}})
	require.NoError(t, err)
	require.Equal(t, DefaultSupportedTokens()["USDC"].Mint, out["USDC"].Mint,
		"a selected registry symbol carries the registry mint")
	require.Equal(t, "USD Coin", out["USDC"].Name)

	// devnet has its own column: the same selection resolves the devnet mint.
	out, err = ResolveDeclared("devnet", map[string]config.TokenConfig{"USDC": {}})
	require.NoError(t, err)
	require.Equal(t, DefaultDevnetTokens()["USDC"].Mint, out["USDC"].Mint)

	// name is display-only and may be overridden; the mint is not touched.
	out, err = ResolveDeclared("mainnet", map[string]config.TokenConfig{"USDC": {Name: "Dollars"}})
	require.NoError(t, err)
	require.Equal(t, "Dollars", out["USDC"].Name)
	require.Equal(t, DefaultSupportedTokens()["USDC"].Mint, out["USDC"].Mint)
}

func TestResolveDeclared_RegistrySymbolWithDeclaredMintFailsLoudly(t *testing.T) {
	t.Parallel()

	// Even the CORRECT address is refused: the value can only agree (redundant)
	// or disagree (accepting payment in a different token than was priced).
	_, err := ResolveDeclared("mainnet", map[string]config.TokenConfig{
		"USDC": {Mint: DefaultSupportedTokens()["USDC"].Mint},
	})
	require.ErrorContains(t, err, "USDC is a built-in token")
	require.ErrorContains(t, err, "remove tokens.USDC.mint")

	_, err = ResolveDeclared("mainnet", map[string]config.TokenConfig{
		"USDC": {Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1w"}, // one char off
	})
	require.ErrorContains(t, err, "built-in token")
}

func TestResolveDeclared_CustomSymbolRequiresMint(t *testing.T) {
	t.Parallel()

	_, err := ResolveDeclared("mainnet", map[string]config.TokenConfig{"MYTK": {Name: "My Token"}})
	require.ErrorContains(t, err, "MYTK requires mint")

	const customMint = "MyTk1111111111111111111111111111111111111111"
	out, err := ResolveDeclared("mainnet", map[string]config.TokenConfig{
		"mytk": {Name: "My Token", Mint: customMint},
	})
	require.NoError(t, err)
	require.Equal(t, customMint, out["MYTK"].Mint)

	// Additive: one custom token never costs the merchant the registry.
	for symbol, want := range DefaultSupportedTokens() {
		require.Equalf(t, want.Mint, out[symbol].Mint, "registry token %s was dropped or changed", symbol)
	}
}

// Test mode: the registry's devnet column is the built-in set there. A mainnet
// symbol with no devnet deployment is NOT built-in on devnet, so it is declared
// like any other ad-hoc devnet token — with an explicit mint.
func TestResolveDeclared_DevnetOnlyBuiltInWhereTheRegistryHasAMint(t *testing.T) {
	t.Parallel()

	require.NotContains(t, DefaultDevnetTokens(), "USDT", "no canonical devnet USDT")

	_, err := ResolveDeclared("devnet", map[string]config.TokenConfig{"USDT": {}})
	require.ErrorContains(t, err, "USDT requires mint")
	require.ErrorContains(t, err, "not a built-in token on devnet")

	const adHoc = "DevUsdt11111111111111111111111111111111111111"
	out, err := ResolveDeclared("devnet", map[string]config.TokenConfig{"USDT": {Mint: adHoc}})
	require.NoError(t, err)
	require.Equal(t, adHoc, out["USDT"].Mint)

	// ...and the same declaration on mainnet is refused, where it matters.
	_, err = ResolveDeclared("mainnet", map[string]config.TokenConfig{"USDT": {Mint: adHoc}})
	require.ErrorContains(t, err, "built-in token")
}

func TestResolveDeclared_NoDeclarationIsTheRegistry(t *testing.T) {
	t.Parallel()

	out, err := ResolveDeclared("mainnet", nil)
	require.NoError(t, err)
	require.Equal(t, DefaultSupportedTokens(), out)

	out, err = ResolveDeclared("devnet", map[string]config.TokenConfig{})
	require.NoError(t, err)
	require.Equal(t, DefaultDevnetTokens(), out)

	_, err = ResolveDeclared("mainnet", map[string]config.TokenConfig{"  ": {Mint: "x"}})
	require.ErrorContains(t, err, "empty symbol")
}
