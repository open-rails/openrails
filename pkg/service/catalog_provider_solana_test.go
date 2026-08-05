package service

import (
	"context"
	"errors"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/merchant"
)

// The solana adapter's on-chain paths (PublishPlan / Verify-with-RPC) are covered
// by the decoder test and the devnet suite. These unit tests pin the adapter's own
// branch logic: unconfigured -> pending, attach validation, sync-disabled, and the
// deterministic plan-id derivation.

func TestSolanaAdapter_AutoCreateUnconfiguredIsPending(t *testing.T) {
	a := &solanaAdapter{svc: &Service{}} // no runtime -> SolanaPlanService nil
	hours := 30 * 24
	days := 30
	_, err := a.AutoCreate(context.Background(), autoCreateContext{
		PriceID:             uuid.New(),
		Currency:            "usd",
		UnitAmount:          29_000_000,
		AccessDurationHours: &hours,
		BillingCycleDays:    &days,
	})
	if !errors.Is(err, errPendingManualLink) {
		t.Fatalf("unconfigured Solana AutoCreate = %v, want errPendingManualLink", err)
	}
}

func TestSolanaAdapter_AutoCreateRecurringDefaultsToUSDC(t *testing.T) {
	const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	hours := 30 * 24
	days := 30
	plan := recurring.NewPlanServiceWithReader(
		&planSubmitterStub{merchantPub: solanago.NewWallet().PublicKey()},
		mintReaderStub{mint: usdcMint, decimals: 6},
		"mainnet",
		map[string]config.TokenConfig{"USDC": {Mint: usdcMint}},
	)
	a := &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))

	got, err := a.AutoCreate(ctx, autoCreateContext{
		ProductKey:          "premium",
		Currency:            "usd",
		UnitAmount:          29_000_000,
		AccessDurationHours: &hours,
		BillingCycleDays:    &days,
	})
	if err != nil {
		t.Fatalf("AutoCreate recurring: %v", err)
	}
	if got[solanaKeyMintSymbol] != "USDC" {
		t.Fatalf("AutoCreate recurring mint_symbol = %q, want USDC", got[solanaKeyMintSymbol])
	}
}

func TestSolanaAdapter_AutoCreateOneOffNeedsNoPlan(t *testing.T) {
	a := &solanaAdapter{}
	hours := 30 * 24
	got, err := a.AutoCreate(context.Background(), autoCreateContext{
		Currency:            "eur",
		UnitAmount:          29_000_000,
		AccessDurationHours: &hours,
	})
	if err != nil {
		t.Fatalf("AutoCreate one-off: %v", err)
	}
	if got["provider"] != "solana" {
		t.Fatalf("AutoCreate one-off = %v, want Solana provider marker", got)
	}
}

func TestSolanaAdapter_AttachUsesTokenInput(t *testing.T) {
	a := &solanaAdapter{}
	days := 30
	if _, err := a.Attach(
		context.Background(),
		map[string]string{solanaKeyMintSymbol: "USDC"},
		autoCreateContext{BillingCycleDays: &days, Currency: "usd"},
	); err == nil {
		t.Error("Attach should reject mint_symbol as input")
	}
	if _, err := a.Attach(
		context.Background(),
		map[string]string{solanaKeyToken: "SOL"},
		autoCreateContext{BillingCycleDays: &days, Currency: "usd"},
	); err == nil {
		t.Error("Attach should reject a non-recurring token")
	}
	if _, err := a.Attach(
		context.Background(),
		map[string]string{solanaKeyPlanPDA: "PdA111", solanaKeyToken: "USDC"},
		autoCreateContext{BillingCycleDays: &days, Currency: "usd"},
	); err == nil {
		t.Error("Attach should reject token beside plan_pda")
	}
	if _, err := a.Attach(context.Background(), map[string]string{
		solanaKeyPlanPDA:         "PdA111",
		solanaKeyAmountBaseUnits: "29000000",
		"junk":                   "", // empty values are dropped
	}, autoCreateContext{BillingCycleDays: &days, Currency: "usdc"}); err == nil {
		t.Error("Attach should not accept a legacy plan_pda without on-chain verification")
	}

	if _, err := a.Attach(context.Background(), map[string]string{
		solanaKeyPlanPDA: "PdAWithoutToken111",
	}, autoCreateContext{BillingCycleDays: &days, Currency: "usd"}); err == nil {
		t.Fatal("existing-plan link without RPC should error")
	}
}

func TestExistingSolanaSettlementToken(t *testing.T) {
	tests := []struct {
		name     string
		currency string
		want     string
		wantErr  bool
	}{
		{name: "new USD price may infer token", currency: "usd"},
		{name: "legacy USDC derives symbol", currency: " USDC ", want: "USDC"},
		{name: "unsupported legacy currency fails", currency: "usdg", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := existingSolanaSettlementToken(tt.currency)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("existingSolanaSettlementToken(%q) = %q, want error", tt.currency, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("existingSolanaSettlementToken(%q): %v", tt.currency, err)
			}
			if got != tt.want {
				t.Fatalf("existingSolanaSettlementToken(%q) = %q, want %q", tt.currency, got, tt.want)
			}
		})
	}
}

func TestResolveSolanaTokenFromMint(t *testing.T) {
	const (
		usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
		usd1Mint = "USD1USD1USD1USD1USD1USD1USD1USD1USD1USD1USD"
	)
	plan := recurring.NewPlanServiceWithReader(
		&planSubmitterStub{merchantPub: solanago.NewWallet().PublicKey()},
		nil,
		"mainnet",
		map[string]config.TokenConfig{
			"USDC": {Mint: usdcMint},
			"USD1": {Mint: usd1Mint},
		},
	)

	got, err := resolveSolanaTokenFromMint(plan, usd1Mint)
	if err != nil {
		t.Fatalf("resolveSolanaTokenFromMint: %v", err)
	}
	if got != "USD1" {
		t.Fatalf("resolveSolanaTokenFromMint = %q, want USD1", got)
	}
	if _, err := resolveSolanaTokenFromMint(plan, solanago.NewWallet().PublicKey().String()); err == nil {
		t.Fatal("unconfigured recurring mint should fail")
	}
}

func TestSolanaAdapter_RemoteWritesDisabledDefersPublish(t *testing.T) {
	const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	hours := 30 * 24
	days := 30
	plan := recurring.NewPlanServiceWithReader(
		&planSubmitterStub{merchantPub: solanago.NewWallet().PublicKey()},
		mintReaderStub{mint: usdcMint, decimals: 6},
		"mainnet",
		map[string]config.TokenConfig{"USDC": {Mint: usdcMint}},
	)
	a := &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))

	_, err := a.Attach(ctx, map[string]string{
		solanaKeyToken: "USDC",
	}, autoCreateContext{
		ProductKey:           "premium",
		Currency:             "usd",
		UnitAmount:           23_000_000,
		AccessDurationHours:  &hours,
		BillingCycleDays:     &days,
		RemoteWritesDisabled: true,
	})
	if !errors.Is(err, errRemoteWritesDisabled) {
		t.Fatalf("Attach with remote writes disabled = %v, want errRemoteWritesDisabled", err)
	}
}

func TestSolanaAdapter_VerifySyncDisabledWithoutRPC(t *testing.T) {
	a := &solanaAdapter{svc: &Service{}} // no runtime -> SolanaRPC nil
	drift, missing, err := a.Verify(context.Background(), map[string]string{solanaKeyPlanPDA: "x"}, nil)
	if err != nil || drift != nil || missing {
		t.Fatalf("Verify without RPC = (%v,%v,%v), want sync_disabled (nil,false,nil)", drift, missing, err)
	}
}

func TestSolanaAdapter_UpdateIsNoOp(t *testing.T) {
	a := &solanaAdapter{}
	if err := a.Update(context.Background(), map[string]string{}, mutableUpdate{}); err != nil {
		t.Errorf("Update should be a no-op, got %v", err)
	}
}

func TestSolanaPlanID_Deterministic(t *testing.T) {
	base := solanaPlanID("premium", "usd", 2300, intPtr(30), "mint-a")
	// Content-addressed: deterministic for identical content, and with NO price-UUID
	// input it is stable across a fresh OpenRails DB (a rebuilt catalog derives the
	// same on-chain plan PDA and reattaches instead of republishing).
	if base != solanaPlanID("premium", "usd", 2300, intPtr(30), "mint-a") {
		t.Error("solanaPlanID must be deterministic for identical content")
	}
	if base == solanaPlanID("premium", "usd", 2900, intPtr(30), "mint-a") {
		t.Error("a different amount must yield a different plan id")
	}
	if base == solanaPlanID("premium", "usd", 2300, intPtr(365), "mint-a") {
		t.Error("a different cycle must yield a different plan id")
	}
	if base == solanaPlanID("basic", "usd", 2300, intPtr(30), "mint-a") {
		t.Error("a different product key must yield a different plan id")
	}
	if base == solanaPlanID("premium", "usd", 2300, intPtr(30), "mint-b") {
		t.Error("a different mint must yield a different plan id")
	}
}
