package recurring

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// fakePlanReader answers the plan-PDA read with a canned (data, err) and every
// OTHER address with a synthetic SPL mint account. PublishPlan reads both the
// plan PDA (re-publish guard) and the mint (on-chain decimals, #817) through
// this one interface.
type fakePlanReader struct {
	data []byte
	err  error
	// mintDecimals is the precision the synthetic mint account reports.
	mintDecimals uint8
	// mintMissing makes the mint read return an empty account (fail closed).
	mintMissing bool
}

func (r fakePlanReader) GetAccountData(_ context.Context, addr solanago.PublicKey) ([]byte, error) {
	if isTestMintAddress(addr) {
		if r.mintMissing {
			return nil, nil
		}
		d := r.mintDecimals
		if d == 0 {
			d = 6 // devnet/mainnet USDC
		}
		return buildMintBlob(d), nil
	}
	return r.data, r.err
}

// testMintAddresses are the mints the recurring token maps point at; the fake
// reader recognises them so a mint read never gets a plan blob.
func isTestMintAddress(addr solanago.PublicKey) bool {
	for _, tok := range testSolanaTokens() {
		if tok.Mint == addr.String() {
			return true
		}
	}
	return false
}

// buildMintBlob assembles a synthetic, initialized SPL Token mint account with
// the given decimals — the exact 82-byte base layout DecodeMintDecimals reads.
func buildMintBlob(decimals uint8) []byte {
	blob := make([]byte, solanaint.MintAccountSize)
	blob[44] = decimals
	blob[45] = 1 // is_initialized
	return blob
}

// readerWithMint is a fake reader whose plan PDA is empty (publish proceeds) and
// whose mint reports `decimals`.
func readerWithMint(decimals uint8) fakePlanReader {
	return fakePlanReader{mintDecimals: decimals}
}

// buildPlanBlob assembles a synthetic on-chain Plan account in the exact byte
// order DecodePlanAccount expects, for the given immutable terms. Only the fields
// the re-publish guard inspects (mint/amount/periodHours) need to be meaningful.
func buildPlanBlob(owner, mint solanago.PublicKey, amount, periodHours uint64, createdAt int64) []byte {
	blob := make([]byte, 0, subscriptions.PlanAccountSize)
	u64 := func(v uint64) { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, v); blob = append(blob, b...) }
	blob = append(blob, 1) // discriminator (planAccountDiscriminator)
	blob = append(blob, owner.Bytes()...)
	blob = append(blob, 254) // bump
	blob = append(blob, 0)   // status
	u64(7)                   // planId
	blob = append(blob, mint.Bytes()...)
	u64(amount)
	u64(periodHours)
	u64(uint64(createdAt)) // createdAt (i64 LE)
	u64(0)                 // endTs
	var zero solanago.PublicKey
	for i := 0; i < 4; i++ { // destinations
		blob = append(blob, zero.Bytes()...)
	}
	for i := 0; i < 4; i++ { // pullers
		blob = append(blob, zero.Bytes()...)
	}
	blob = append(blob, make([]byte, 128)...) // metadataUri
	return blob
}

func devnetUSDCMint(t *testing.T) solanago.PublicKey {
	t.Helper()
	mintStr, err := ResolveRecurringMintFromTokens("USDC", testSolanaTokens())
	if err != nil {
		t.Fatalf("resolve devnet USDC mint: %v", err)
	}
	mint, err := solanago.PublicKeyFromBase58(mintStr)
	if err != nil {
		t.Fatalf("parse mint: %v", err)
	}
	return mint
}

// TestPublishPlanIdempotentRepublish: when the plan PDA already holds a plan with
// MATCHING terms, PublishPlan returns the existing handle WITHOUT a second submit.
func TestPublishPlanIdempotentRepublish(t *testing.T) {
	merchantKey, _ := solanago.NewRandomPrivateKey()
	merchantPub := merchantKey.PublicKey()
	mint := devnetUSDCMint(t)

	const amount = uint64(10_000_000)
	const periodHours = uint64(720)
	const createdAt = int64(1_717_200_000)

	planPDA, _, err := subscriptions.DerivePlanPDA(merchantPub, 7)
	if err != nil {
		t.Fatalf("derive pda: %v", err)
	}

	sub := &fakeSubmitter{merchantPub: merchantPub}
	reader := fakePlanReader{data: buildPlanBlob(merchantPub, mint, amount, periodHours, createdAt)}
	svc := NewPlanServiceWithReader(sub, reader, "devnet", testSolanaTokens())

	h, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		MerchantID:      merchant.ID{},
		PlanID:          7,
		TokenSymbol:     "USDC",
		AmountBaseUnits: amount,
		PeriodHours:     periodHours,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(sub.submits) != 0 {
		t.Fatalf("expected NO submit on idempotent re-publish, got %d submit sets", len(sub.submits))
	}
	if h.PlanPDA != planPDA.String() {
		t.Fatalf("handle PDA = %s, want %s", h.PlanPDA, planPDA.String())
	}
	if h.CreatedAt != createdAt || h.AmountBaseUnits != amount || h.PeriodHours != periodHours {
		t.Fatalf("handle did not echo existing on-chain terms: %+v", h)
	}
	if h.Signature != "" {
		t.Fatalf("idempotent no-op should carry no fresh signature, got %q", h.Signature)
	}
}

// TestPublishPlanRepublishDifferingTermsRejected: an existing plan with different
// IMMUTABLE terms must be rejected (publish a new plan_id instead of mutating).
func TestPublishPlanRepublishDifferingTermsRejected(t *testing.T) {
	merchantKey, _ := solanago.NewRandomPrivateKey()
	merchantPub := merchantKey.PublicKey()
	mint := devnetUSDCMint(t)

	// On-chain plan has amount 5_000_000; the re-publish requests 10_000_000.
	reader := fakePlanReader{data: buildPlanBlob(merchantPub, mint, 5_000_000, 720, 1_717_200_000)}
	sub := &fakeSubmitter{merchantPub: merchantPub}
	svc := NewPlanServiceWithReader(sub, reader, "devnet", testSolanaTokens())

	_, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		MerchantID:      merchant.ID{},
		PlanID:          7,
		TokenSymbol:     "USDC",
		AmountBaseUnits: 10_000_000,
		PeriodHours:     720,
	})
	if err == nil {
		t.Fatal("expected immutability rejection for differing terms, got nil")
	}
	if !strings.Contains(err.Error(), "immutable") && !strings.Contains(err.Error(), "IMMUTABLE") {
		t.Fatalf("error should explain plans are immutable, got: %v", err)
	}
	if len(sub.submits) != 0 {
		t.Fatalf("must not submit create_plan when rejecting differing terms, got %d", len(sub.submits))
	}
}

// TestPublishPlanAbsentPDAProceeds: when the reader reports the PDA empty, the
// normal create_plan path runs (a submit happens and a fresh handle is returned).
func TestPublishPlanAbsentPDAProceeds(t *testing.T) {
	merchantKey, _ := solanago.NewRandomPrivateKey()
	merchantPub := merchantKey.PublicKey()

	reader := fakePlanReader{data: nil} // PDA does not exist
	sub := &fakeSubmitter{merchantPub: merchantPub}
	svc := NewPlanServiceWithReader(sub, reader, "devnet", testSolanaTokens())

	h, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		MerchantID:      merchant.ID{},
		PlanID:          7,
		TokenSymbol:     "USDC",
		AmountBaseUnits: 10_000_000,
		PeriodHours:     720,
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(sub.submits) == 0 {
		t.Fatal("expected create_plan submit when PDA is absent")
	}
	if h.Signature == "" {
		t.Fatal("a fresh publish must carry a submit signature")
	}
}

// TestPublishPlanPeriodBoundValidation: period_hours must be in (0, 8760].
func TestPublishPlanPeriodBoundValidation(t *testing.T) {
	merchantKey, _ := solanago.NewRandomPrivateKey()
	merchantPub := merchantKey.PublicKey()
	sub := &fakeSubmitter{merchantPub: merchantPub}
	svc := NewPlanServiceWithReader(sub, nil, "devnet", testSolanaTokens())

	for _, period := range []uint64{0, maxPeriodHours + 1} {
		_, err := svc.PublishPlan(context.Background(), PublishPlanInput{
			PlanID:          7,
			TokenSymbol:     "USDC",
			AmountBaseUnits: 10_000_000,
			PeriodHours:     period,
		})
		if err == nil {
			t.Fatalf("period_hours %d should be rejected", period)
		}
		if !strings.Contains(err.Error(), "period_hours") {
			t.Fatalf("period %d: error should mention period_hours, got: %v", period, err)
		}
	}
	if len(sub.submits) != 0 {
		t.Fatalf("no submit on validation failure, got %d", len(sub.submits))
	}
}

// TestPublishPlanBillingCycleConsistency: when BillingCycleHours is threaded in,
// period_hours must match it.
func TestPublishPlanBillingCycleConsistency(t *testing.T) {
	merchantKey, _ := solanago.NewRandomPrivateKey()
	merchantPub := merchantKey.PublicKey()
	sub := &fakeSubmitter{merchantPub: merchantPub}
	svc := NewPlanServiceWithReader(sub, nil, "devnet")

	_, err := svc.PublishPlan(context.Background(), PublishPlanInput{
		PlanID:            7,
		TokenSymbol:       "USDC",
		AmountBaseUnits:   10_000_000,
		PeriodHours:       720,
		BillingCycleHours: 744,
	})
	if err == nil {
		t.Fatal("mismatched period_hours vs billing_cycle_hours should be rejected")
	}
	if !strings.Contains(err.Error(), "billing_cycle_hours") {
		t.Fatalf("error should mention billing_cycle_hours, got: %v", err)
	}
	if len(sub.submits) != 0 {
		t.Fatalf("no submit on consistency failure, got %d", len(sub.submits))
	}
}
