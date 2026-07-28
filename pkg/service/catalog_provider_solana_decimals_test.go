package service

import (
	"context"
	"strconv"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/merchant"
)

// planSubmitterStub satisfies recurring.Submitter without touching a chain.
type planSubmitterStub struct{ merchantPub solanago.PublicKey }

func (s *planSubmitterStub) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return s.merchantPub, nil
}

func (s *planSubmitterStub) Submit(context.Context, merchant.ID, []solanago.Instruction) (solanago.Signature, error) {
	return solanago.Signature{1}, nil
}

// TestSolanaAdapter_AutoCreateHonoursTokenDecimals pins #817 on the plan-PUBLISH
// path: the catalog price is MICROS, the on-chain plan amount is token BASE
// UNITS, and the merchant's configured decimals is the only thing that converts
// between them. Shipping micros verbatim (the old bug) was a 1000x undercharge
// on a 9-decimal mint.
func TestSolanaAdapter_AutoCreateHonoursTokenDecimals(t *testing.T) {
	const usdcMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	hours := 30 * 24

	cases := []struct {
		decimals int
		micros   int64
		want     uint64
	}{
		{decimals: 6, micros: 10_000_000, want: 10_000_000},
		{decimals: 9, micros: 10_000_000, want: 10_000_000_000},
		{decimals: 8, micros: 10_000_000, want: 1_000_000_000},
		{decimals: 6, micros: 4_030_000, want: 4_030_000},       // #818 float off-by-one amounts
		{decimals: 9, micros: 8_050_000, want: 8_050_000_000},   //
		{decimals: 8, micros: 8_130_000, want: 813_000_000},     //
		{decimals: 9, micros: 19_990_000, want: 19_990_000_000}, // $19.99
	}

	key, err := solanago.NewRandomPrivateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}

	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.decimals)+"-decimals", func(t *testing.T) {
			plan := recurring.NewPlanServiceWithReader(
				&planSubmitterStub{merchantPub: key.PublicKey()}, nil, "mainnet",
				map[string]config.TokenConfig{"USDC": {Mint: usdcMint, Decimals: tc.decimals}},
			)
			a := &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
			ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))

			out, err := a.AutoCreate(ctx, autoCreateContext{
				PriceID:             uuid.New(),
				ProductKey:          "premium",
				Currency:            "USDC",
				UnitAmount:          tc.micros,
				AccessDurationHours: &hours,
			})
			if err != nil {
				t.Fatalf("AutoCreate: %v", err)
			}
			if got := out[solanaKeyAmountBaseUnits]; got != strconv.FormatUint(tc.want, 10) {
				t.Fatalf("%d micros @%d decimals: plan amount_base_units = %s, want %d", tc.micros, tc.decimals, got, tc.want)
			}
		})
	}
}

// TestSolanaAdapter_AutoCreateRejectsMissingDecimals: an omitted `decimals`
// decodes as 0 and must fail loudly rather than publish a 10^6-off plan.
func TestSolanaAdapter_AutoCreateRejectsMissingDecimals(t *testing.T) {
	key, _ := solanago.NewRandomPrivateKey()
	hours := 30 * 24
	plan := recurring.NewPlanServiceWithReader(
		&planSubmitterStub{merchantPub: key.PublicKey()}, nil, "mainnet",
		map[string]config.TokenConfig{"USDC": {Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"}},
	)
	a := &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))

	if _, err := a.AutoCreate(ctx, autoCreateContext{
		PriceID:             uuid.New(),
		ProductKey:          "premium",
		Currency:            "USDC",
		UnitAmount:          10_000_000,
		AccessDurationHours: &hours,
	}); err == nil {
		t.Fatal("expected AutoCreate to reject a token with no configured decimals")
	}
}
