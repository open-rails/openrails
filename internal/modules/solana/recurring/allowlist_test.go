package recurring

import (
	"errors"
	"testing"
)

func TestIsRecurringStablecoinSymbol(t *testing.T) {
	cases := map[string]bool{
		"USDC": true, "usdc": true, " USDC ": true,
		"USD1":  true,  // plain SPL, verified eligible
		"PYUSD": false, // Token-2022 PermanentDelegate (devnet error 121)
		"USDG":  false, // Token-2022 PermanentDelegate+TransferFee+...
		"BUIDL": false, // Token-2022 PermanentDelegate+TransferHook (permissioned)
		"USDT":  false, // permanently excluded
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
	// USDC resolves on both networks with 6 decimals.
	for _, network := range []string{"mainnet", "devnet"} {
		mint, decimals, err := ResolveRecurringMint("USDC", network)
		if err != nil {
			t.Fatalf("ResolveRecurringMint(USDC, %s): %v", network, err)
		}
		if mint == "" {
			t.Fatalf("ResolveRecurringMint(USDC, %s) returned empty mint", network)
		}
		if decimals != 6 {
			t.Errorf("USDC decimals on %s = %d, want 6", network, decimals)
		}
	}

	// USD1 resolves on mainnet (where its mint exists) but not devnet (mainnet-only).
	if mint, dec, err := ResolveRecurringMint("USD1", "mainnet"); err != nil || mint == "" || dec != 6 {
		t.Errorf("ResolveRecurringMint(USD1, mainnet) = (%q,%d,%v), want a 6-decimal mint", mint, dec, err)
	}
	if _, _, err := ResolveRecurringMint("USD1", "devnet"); err == nil {
		t.Error("ResolveRecurringMint(USD1, devnet) should fail — USD1 has no devnet mint")
	}

	// Off-allowlist tokens fail closed even though they're otherwise supported.
	for _, sym := range []string{"PYUSD", "USDG", "BUIDL", "USDT", "SOL", "NOPE"} {
		if _, _, err := ResolveRecurringMint(sym, "mainnet"); err == nil {
			t.Errorf("ResolveRecurringMint(%s) = nil error, want rejection", sym)
		}
	}

	// The eligibility error is typed.
	_, _, err := ResolveRecurringMint("USDT", "mainnet")
	var typed ErrTokenNotRecurringEligible
	if !errors.As(err, &typed) {
		t.Errorf("expected ErrTokenNotRecurringEligible for USDT, got %T", err)
	}
}

func TestValidateRecurringMint(t *testing.T) {
	goodMint, _, err := ResolveRecurringMint("USDC", "mainnet")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := ValidateRecurringMint("USDC", goodMint, "mainnet"); err != nil {
		t.Errorf("matching mint should pass: %v", err)
	}
	if err := ValidateRecurringMint("USDC", "SomeOtherMint1111111111111111111111111111111", "mainnet"); err == nil {
		t.Error("mismatched mint should fail")
	}
	if err := ValidateRecurringMint("USDT", goodMint, "mainnet"); err == nil {
		t.Error("off-allowlist symbol should fail regardless of mint")
	}
}
