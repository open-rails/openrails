package recurring

import (
	"context"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

const testDevnetUSDCMint = "5CVTPbcqPuzQd9bMCViire6zQVSr7TUTWTjM21aE4TZ"

func testSolanaTokens() map[string]config.TokenConfig {
	return map[string]config.TokenConfig{
		"USDC": {Mint: testDevnetUSDCMint},
	}
}

// fakeSubmitter captures every submitted instruction set so tests can assert on
// the on-chain effects PublishPlan produces without a live signer/RPC.
type fakeSubmitter struct {
	merchantPub solanago.PublicKey
	submits     [][]solanago.Instruction
}

func (f *fakeSubmitter) MerchantAddress(_ context.Context, _ merchant.ID) (solanago.PublicKey, error) {
	return f.merchantPub, nil
}

func (f *fakeSubmitter) Submit(_ context.Context, _ merchant.ID, ins []solanago.Instruction) (solanago.Signature, error) {
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
	merchantPub := merchantKey.PublicKey()
	sub := &fakeSubmitter{merchantPub: merchantPub}
	svc := NewPlanServiceWithReader(sub, readerWithMint(6), "devnet", testSolanaTokens())

	h, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		MerchantID:      merchant.ID{},
		PlanID:          1,
		TokenSymbol:     "USDC",
		AmountBaseUnits: 10_000_000,
		AmountDecimals:  6,
		PeriodHours:     720,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	mint, err := solanago.PublicKeyFromBase58(h.Mint)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	ix := findEnsureATA(t, sub, merchantPub, mint)

	accs := ix.Accounts()
	// payer is the cranker (merchant), signer+writable.
	if !accs[0].PublicKey.Equals(merchantPub) || !accs[0].IsSigner || !accs[0].IsWritable {
		t.Fatal("ensure-ATA payer must be the merchant (cranker), signer+writable")
	}
	if !accs[2].PublicKey.Equals(merchantPub) {
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
	merchantPub := merchantKey.PublicKey()
	cold := coldKey.PublicKey()

	sub := &fakeSubmitter{merchantPub: merchantPub}
	svc := NewPlanServiceWithReader(sub, readerWithMint(6), "devnet", testSolanaTokens())

	h, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		MerchantID:      merchant.ID{},
		PlanID:          2,
		TokenSymbol:     "USDC",
		AmountBaseUnits: 5_000_000,
		AmountDecimals:  6,
		PeriodHours:     720,
		ReceivingWallet: cold.String(),
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	mint, _ := solanago.PublicKeyFromBase58(h.Mint)

	// Both the merchant ATA and the cold-wallet ATA must be ensured.
	findEnsureATA(t, sub, merchantPub, mint)
	ix := findEnsureATA(t, sub, cold, mint)
	if !ix.Accounts()[2].PublicKey.Equals(cold) {
		t.Fatal("cold-wallet ensure-ATA owner must be the cold wallet")
	}
	// Payer for the cold-wallet ATA is still the cranker (merchant pays gas).
	if !ix.Accounts()[0].PublicKey.Equals(merchantPub) {
		t.Fatal("cold-wallet ensure-ATA must still be paid by the cranker")
	}
}
