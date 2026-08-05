package recurring

import (
	"context"
	"strings"
	"testing"

	solanago "github.com/gagliardetto/solana-go"

	"github.com/open-rails/openrails/pkg/merchant"
)

// TestPublishPlanRequiresOnChainDecimals pins #817 at the boundary where it
// matters most: a plan's amount is written IMMUTABLY on-chain, so a caller that
// converted micros at the wrong shift must be refused before submit, not
// discovered afterwards. The mint is the source of truth.
func TestPublishPlanRequiresOnChainDecimals(t *testing.T) {
	key, _ := solanago.NewRandomPrivateKey()
	ctx := context.Background()

	// The mint really has 9 decimals; the caller converted at 6. That is a
	// 1000x undercharge — refuse it.
	t.Run("caller shift disagrees with the mint", func(t *testing.T) {
		sub := &fakeSubmitter{merchantPub: key.PublicKey()}
		svc := NewPlanServiceWithReader(sub, readerWithMint(9), "devnet", testSolanaTokens())

		_, err := svc.PublishPlan(ctx, PublishPlanInput{
			MerchantID:      merchant.ID{},
			PlanID:          1,
			TokenSymbol:     "USDC",
			AmountBaseUnits: 10_000_000, // $10 at 6 decimals — wrong for this mint
			AmountDecimals:  6,
			PeriodHours:     720,
		})
		if err == nil {
			t.Fatal("expected a refusal: 6-decimal amount against a 9-decimal mint")
		}
		if !strings.Contains(err.Error(), "9 on-chain") {
			t.Fatalf("error should name the on-chain precision, got: %v", err)
		}
		if len(sub.submits) != 0 {
			t.Fatal("nothing may be submitted on a decimals mismatch")
		}
	})

	// Matching shift publishes the exact base-unit integer the caller computed.
	t.Run("matching shift publishes verbatim", func(t *testing.T) {
		for _, tc := range []struct {
			decimals uint8
			amount   uint64
		}{
			{decimals: 6, amount: 10_000_000},
			{decimals: 9, amount: 10_000_000_000},
			{decimals: 8, amount: 1_000_000_000},
		} {
			sub := &fakeSubmitter{merchantPub: key.PublicKey()}
			svc := NewPlanServiceWithReader(sub, readerWithMint(tc.decimals), "devnet", testSolanaTokens())

			h, err := svc.PublishPlan(ctx, PublishPlanInput{
				MerchantID:      merchant.ID{},
				PlanID:          2,
				TokenSymbol:     "USDC",
				AmountBaseUnits: tc.amount,
				AmountDecimals:  int(tc.decimals),
				PeriodHours:     720,
			})
			if err != nil {
				t.Fatalf("publish at %d decimals: %v", tc.decimals, err)
			}
			if h.AmountBaseUnits != tc.amount {
				t.Fatalf("published amount = %d, want %d", h.AmountBaseUnits, tc.amount)
			}
		}
	})

	// No decimals declared at all (zero value) is a refusal, not an implicit 6.
	t.Run("missing caller decimals is refused", func(t *testing.T) {
		sub := &fakeSubmitter{merchantPub: key.PublicKey()}
		svc := NewPlanServiceWithReader(sub, readerWithMint(6), "devnet", testSolanaTokens())

		if _, err := svc.PublishPlan(ctx, PublishPlanInput{
			MerchantID:      merchant.ID{},
			PlanID:          3,
			TokenSymbol:     "USDC",
			AmountBaseUnits: 10_000_000,
			PeriodHours:     720,
		}); err == nil {
			t.Fatal("an omitted AmountDecimals must be refused, never treated as the mint's")
		}
	})

	// An unreadable mint stops the publish: there is no fallback precision.
	t.Run("unreadable mint blocks the publish", func(t *testing.T) {
		sub := &fakeSubmitter{merchantPub: key.PublicKey()}
		svc := NewPlanServiceWithReader(sub, fakePlanReader{mintMissing: true}, "devnet", testSolanaTokens())

		if _, err := svc.PublishPlan(ctx, PublishPlanInput{
			MerchantID:      merchant.ID{},
			PlanID:          4,
			TokenSymbol:     "USDC",
			AmountBaseUnits: 10_000_000,
			AmountDecimals:  6,
			PeriodHours:     720,
		}); err == nil {
			t.Fatal("an unreadable mint must block the publish")
		}
	})

	// A mint reporting an unpayable precision is refused too.
	t.Run("unpayable on-chain precision is refused", func(t *testing.T) {
		sub := &fakeSubmitter{merchantPub: key.PublicKey()}
		svc := NewPlanServiceWithReader(sub, fakePlanReader{mintZeroDecimals: true}, "devnet", testSolanaTokens())

		if _, err := svc.PublishPlan(ctx, PublishPlanInput{
			MerchantID:      merchant.ID{},
			PlanID:          5,
			TokenSymbol:     "USDC",
			AmountBaseUnits: 10,
			AmountDecimals:  0,
			PeriodHours:     720,
		}); err == nil {
			t.Fatal("a 0-decimal mint must be refused as a payment token")
		}
	})
}
