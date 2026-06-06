package recurring

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

const testDevnetUSDCMint = "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ"

func testSolanaTokens() map[string]config.SolanaToken {
	return map[string]config.SolanaToken{
		"USDC": {Mint: testDevnetUSDCMint, Decimals: 6},
	}
}

// fakeSubmitter captures every submitted instruction set so tests can assert on
// the on-chain effects PublishPlan produces without a live signer/RPC.
type fakeSubmitter struct {
	merchant solanago.PublicKey
	submits  [][]solanago.Instruction
}

func (f *fakeSubmitter) MerchantAddress(_ context.Context, _ tenant.ID) (solanago.PublicKey, error) {
	return f.merchant, nil
}

func (f *fakeSubmitter) Submit(_ context.Context, _ tenant.ID, ins []solanago.Instruction) (solanago.Signature, error) {
	f.submits = append(f.submits, ins)
	return solanago.Signature{byte(len(f.submits))}, nil
}

// findEnsureATA returns the lone CreateIdempotent ATA instruction whose derived
// ATA matches owner+mint, or fails the test if none was submitted.
func findEnsureATA(t *testing.T, sub *fakeSubmitter, owner, mint solanago.PublicKey) solanago.Instruction {
	t.Helper()
	wantATA, _, err := subscriptions.DeriveATA(owner, mint, solanago.TokenProgramID)
	if err != nil {
		t.Fatalf("derive expected ata: %v", err)
	}
	for _, set := range sub.submits {
		for _, ix := range set {
			if !ix.ProgramID().Equals(subscriptions.AssociatedTokenProgramID) {
				continue
			}
			accs := ix.Accounts()
			if len(accs) == 6 && accs[1].PublicKey.Equals(wantATA) {
				return ix
			}
		}
	}
	t.Fatalf("no ensure-ATA instruction for owner %s", owner)
	return nil
}

func TestPublishPlanEnsuresMerchantReceivingATA(t *testing.T) {
	merchantKey, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	merchant := merchantKey.PublicKey()
	sub := &fakeSubmitter{merchant: merchant}
	svc := NewPlanService(sub, "devnet", testSolanaTokens())

	h, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		TenantID:        tenant.ID{},
		PlanID:          1,
		TokenSymbol:     "USDC",
		AmountBaseUnits: 10_000_000,
		PeriodHours:     720,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	mint, err := solanago.PublicKeyFromBase58(h.Mint)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ix := findEnsureATA(t, sub, merchant, mint)

	accs := ix.Accounts()
	// payer is the cranker (merchant), signer+writable.
	if !accs[0].PublicKey.Equals(merchant) || !accs[0].IsSigner || !accs[0].IsWritable {
		t.Fatal("ensure-ATA payer must be the merchant (cranker), signer+writable")
	}
	if !accs[2].PublicKey.Equals(merchant) {
		t.Fatal("ensure-ATA owner must be the merchant")
	}
	if !accs[3].PublicKey.Equals(mint) {
		t.Fatal("ensure-ATA mint mismatch")
	}
	d, _ := ix.Data()
	if len(d) != 1 || d[0] != 1 {
		t.Fatalf("ensure-ATA data = %v, want [1] (CreateIdempotent)", d)
	}
}

func TestPublishPlanEnsuresColdReceivingWalletATA(t *testing.T) {
	merchantKey, _ := solanago.NewRandomPrivateKey()
	coldKey, _ := solanago.NewRandomPrivateKey()
	merchant := merchantKey.PublicKey()
	cold := coldKey.PublicKey()

	sub := &fakeSubmitter{merchant: merchant}
	svc := NewPlanService(sub, "devnet", testSolanaTokens())

	h, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		TenantID:        tenant.ID{},
		PlanID:          2,
		TokenSymbol:     "USDC",
		AmountBaseUnits: 5_000_000,
		PeriodHours:     720,
		ReceivingWallet: cold.String(),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	mint, _ := solanago.PublicKeyFromBase58(h.Mint)

	// Both the merchant ATA and the cold-wallet ATA must be ensured.
	findEnsureATA(t, sub, merchant, mint)
	ix := findEnsureATA(t, sub, cold, mint)
	if !ix.Accounts()[2].PublicKey.Equals(cold) {
		t.Fatal("cold-wallet ensure-ATA owner must be the cold wallet")
	}
	// Payer for the cold-wallet ATA is still the cranker (merchant pays gas).
	if !ix.Accounts()[0].PublicKey.Equals(merchant) {
		t.Fatal("cold-wallet ensure-ATA must still be paid by the cranker")
	}
}
