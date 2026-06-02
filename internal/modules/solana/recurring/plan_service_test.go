package recurring

import (
	"context"
	"errors"
	"testing"

	solanago "github.com/doujins-org/solana-go"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

type fakeSubmitter struct {
	merchant   solanago.PublicKey
	lastInstrs []solanago.Instruction
	submits    int
	sig        solanago.Signature
	err        error
}

func (f *fakeSubmitter) MerchantAddress(context.Context, tenant.ID) (solanago.PublicKey, error) {
	return f.merchant, nil
}

func (f *fakeSubmitter) Submit(_ context.Context, _ tenant.ID, instrs []solanago.Instruction) (solanago.Signature, error) {
	f.submits++
	f.lastInstrs = instrs
	return f.sig, f.err
}

func newMerchant(t *testing.T) solanago.PublicKey {
	t.Helper()
	k, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return k.PublicKey()
}

func TestPublishPlanHappyPath(t *testing.T) {
	merchant := newMerchant(t)
	fs := &fakeSubmitter{merchant: merchant}
	svc := NewPlanService(fs, "mainnet")

	h, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		TenantID: tenant.DefaultID, PlanID: 42, TokenSymbol: "usdc",
		AmountBaseUnits: 10_000_000, PeriodHours: 720,
	})
	if err != nil {
		t.Fatalf("PublishPlan: %v", err)
	}
	if h.MintSymbol != "USDC" || h.AmountBaseUnits != 10_000_000 || h.PeriodHours != 720 {
		t.Fatalf("unexpected handle: %+v", h)
	}
	if h.MerchantAddress != merchant.String() {
		t.Errorf("merchant = %s, want %s", h.MerchantAddress, merchant)
	}
	// Plan PDA must match the deterministic derivation.
	wantPDA, _, _ := subscriptions.DerivePlanPDA(merchant, 42)
	if h.PlanPDA != wantPDA.String() {
		t.Errorf("plan pda = %s, want %s", h.PlanPDA, wantPDA)
	}
	// The submitted instruction is a create_plan against the program, merchant first.
	if fs.submits != 1 || len(fs.lastInstrs) != 1 {
		t.Fatalf("expected one submitted instruction, got %d submits", fs.submits)
	}
	ix := fs.lastInstrs[0]
	if !ix.ProgramID().Equals(subscriptions.ProgramID) {
		t.Errorf("instruction program = %s, want subscriptions program", ix.ProgramID())
	}
	if accs := ix.Accounts(); len(accs) == 0 || !accs[0].PublicKey.Equals(merchant) || !accs[0].IsSigner {
		t.Error("create_plan account[0] must be the merchant signer")
	}

	// Handle round-trips into a processor config map.
	cfg := h.ToProcessorConfig()
	if cfg["mint_symbol"] != "USDC" || cfg["plan_id"] != "42" || cfg["plan_pda"] != h.PlanPDA {
		t.Errorf("processor config wrong: %v", cfg)
	}
}

func TestPublishPlanRejectsIneligibleToken(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	svc := NewPlanService(fs, "mainnet")
	for _, sym := range []string{"PYUSD", "USDG", "SOL"} {
		_, err := svc.PublishPlan(context.Background(), PublishPlanInput{
			TenantID: tenant.DefaultID, PlanID: 1, TokenSymbol: sym, AmountBaseUnits: 1, PeriodHours: 1,
		})
		var typed ErrTokenNotRecurringEligible
		if !errors.As(err, &typed) {
			t.Errorf("token %s: expected ErrTokenNotRecurringEligible, got %v", sym, err)
		}
	}
	if fs.submits != 0 {
		t.Error("ineligible tokens must never reach submit (fail closed)")
	}
}

func TestPublishPlanValidatesTerms(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	svc := NewPlanService(fs, "mainnet")
	cases := []PublishPlanInput{
		{TokenSymbol: "USDC", AmountBaseUnits: 0, PeriodHours: 720},    // zero amount
		{TokenSymbol: "USDC", AmountBaseUnits: 1, PeriodHours: 0},      // zero period
		{TokenSymbol: "USDC", AmountBaseUnits: 1, PeriodHours: 10_000}, // > 1 year
	}
	for i, in := range cases {
		in.TenantID = tenant.DefaultID
		in.PlanID = uint64(i + 1)
		if _, err := svc.PublishPlan(context.Background(), in); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
	if fs.submits != 0 {
		t.Error("invalid terms must never reach submit")
	}
}
