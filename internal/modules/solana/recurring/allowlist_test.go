package recurring

import (
	"errors"
	"testing"

	"github.com/open-rails/openrails/config"
)

func TestIsRecurringStablecoinSymbol(t *testing.T) {
	cases := map[string]bool{
		"USDC": true, "usdc": true, " USDC ": true,
		"USD1":  true,  // plain SPL, verified eligible
		"PYUSD": false, // Token-2022 PermanentDelegate (devnet error 121)
		"USDG":  false, // Token-2022 PermanentDelegate+TransferFee+...
		"SOL":   false, // volatile
		"":      false,
	}
	for in, want := range cases {
		if got := IsRecurringStablecoinSymbol(in); got != want {
			t.Errorf("IsRecurringStablecoinSymbol(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveRecurringMint(t *testing.T) {
	// USDC resolves on both networks. Decimals are NOT returned (#817) — they
	// come from the mint on-chain.
	for _, network := range []string{"mainnet", "devnet"} {
		mint, err := ResolveRecurringMint("USDC", network)
		if err != nil {
			t.Fatalf("ResolveRecurringMint(USDC, %s): %v", network, err)
		}
		if mint == "" {
			t.Fatalf("ResolveRecurringMint(USDC, %s) returned empty mint", network)
		}
	}

	// USD1 resolves on mainnet (where its mint exists) but not devnet (mainnet-only).
	if mint, err := ResolveRecurringMint("USD1", "mainnet"); err != nil || mint == "" {
		t.Errorf("ResolveRecurringMint(USD1, mainnet) = (%q,%v), want a mint", mint, err)
	}
	if _, err := ResolveRecurringMint("USD1", "devnet"); err == nil {
		t.Error("ResolveRecurringMint(USD1, devnet) should fail — USD1 has no devnet mint")
	}

	// Off-allowlist tokens fail closed even though they're supported for one-off.
	for _, sym := range []string{"PYUSD", "USDG", "SOL"} {
		if _, err := ResolveRecurringMint(sym, "mainnet"); err == nil {
			t.Errorf("ResolveRecurringMint(%s) = nil error, want rejection", sym)
		}
	}

	// The eligibility error is typed.
	_, err := ResolveRecurringMint("PYUSD", "mainnet")
	var typed ErrTokenNotRecurringEligible
	if !errors.As(err, &typed) {
		t.Errorf("expected ErrTokenNotRecurringEligible for PYUSD, got %T", err)
	}
}

func TestResolveRecurringMintFromTokensUsesConfiguredMint(t *testing.T) {
	const configuredMint = "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ"
	mint, err := ResolveRecurringMintFromTokens("USDC", map[string]config.TokenConfig{
		"USDC": {Mint: configuredMint},
	})
	if err != nil {
		t.Fatalf("ResolveRecurringMintFromTokens: %v", err)
	}
	if mint != configuredMint {
		t.Fatalf("mint = %s, want configured mint %s", mint, configuredMint)
	}
}
