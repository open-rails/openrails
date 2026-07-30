package service

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
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

func TestSolanaAdapter_AttachRequiresRecurringSettlementToken(t *testing.T) {
	a := &solanaAdapter{}
	days := 30
	if _, err := a.Attach(
		context.Background(),
		map[string]string{"mint": "x"},
		autoCreateContext{BillingCycleDays: &days, Currency: "usd"},
	); err == nil {
		t.Error("Attach recurring plan without mint_symbol should error")
	}
	got, err := a.Attach(context.Background(), map[string]string{
		solanaKeyPlanPDA:         "PdA111",
		solanaKeyMintSymbol:      "USDC",
		solanaKeyAmountBaseUnits: "29000000",
		"junk":                   "", // empty values are dropped
	}, autoCreateContext{BillingCycleDays: &days, Currency: "usd"})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got[solanaKeyPlanPDA] != "PdA111" || got[solanaKeyMintSymbol] != "USDC" || got[solanaKeyAmountBaseUnits] != "29000000" {
		t.Errorf("Attach passthrough wrong: %v", got)
	}
	if _, ok := got["junk"]; ok {
		t.Errorf("empty link values should be dropped, got %v", got)
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
