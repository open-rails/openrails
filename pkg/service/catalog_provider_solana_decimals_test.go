package service

import (
	"context"
	"strconv"
	"testing"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/solana/recurring"
	"github.com/open-rails/openrails/pkg/merchant"
)

const testUSDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

// planSubmitterStub satisfies recurring.Submitter without touching a chain.
type planSubmitterStub struct{ merchantPub solanago.PublicKey }

func (s *planSubmitterStub) MerchantAddress(context.Context, merchant.ID) (solanago.PublicKey, error) {
	return s.merchantPub, nil
}

func (s *planSubmitterStub) Submit(context.Context, merchant.ID, []solanago.Instruction) (solanago.Signature, error) {
	return solanago.Signature{1}, nil
}

// mintReaderStub answers the mint address with a synthetic SPL mint account at
// `decimals` and every other address (the plan PDA) with an empty account, so
// PublishPlan proceeds past its re-publish guard.
type mintReaderStub struct {
	mint     string
	decimals uint8
	// absent makes the mint read return an empty account (mint not on chain).
	absent bool
}

func (r mintReaderStub) GetAccountData(_ context.Context, addr solanago.PublicKey) ([]byte, error) {
	if addr.String() != r.mint || r.absent {
		return nil, nil
	}
	blob := make([]byte, solanaint.MintAccountSize)
	blob[44] = r.decimals
	blob[45] = 1 // is_initialized
	return blob, nil
}

// TestSolanaAdapter_DeclarativeMintHonoursOnChainDecimals pins #817 on the
// plan-PUBLISH path: the catalog price is MICROS, the on-chain plan amount is
// token BASE UNITS, and the MINT's on-chain decimals is the only thing that
// converts between them. Shipping micros verbatim (the original bug) was a
// 1000x undercharge on a 9-decimal mint.
func TestSolanaAdapter_DeclarativeMintHonoursOnChainDecimals(t *testing.T) {
	hours := 30 * 24
	days := 30

	cases := []struct {
		decimals uint8
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
		t.Run(strconv.Itoa(int(tc.decimals))+"-decimals", func(t *testing.T) {
			plan := recurring.NewPlanServiceWithReader(
				&planSubmitterStub{merchantPub: key.PublicKey()},
				mintReaderStub{mint: testUSDCMint, decimals: tc.decimals},
				"mainnet",
				map[string]config.TokenConfig{"USDC": {Mint: testUSDCMint}},
			)
			a := &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
			ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))

			out, err := a.Attach(ctx, map[string]string{
				solanaKeyToken: "USDC",
			}, autoCreateContext{
				PriceID:             uuid.New(),
				ProductKey:          "premium",
				Currency:            "usd",
				UnitAmount:          tc.micros,
				AccessDurationHours: &hours,
				BillingCycleDays:    &days,
			})
			if err != nil {
				t.Fatalf("Attach: %v", err)
			}
			if got := out[solanaKeyAmountBaseUnits]; got != strconv.FormatUint(tc.want, 10) {
				t.Fatalf("%d micros @%d decimals: plan amount_base_units = %s, want %d", tc.micros, tc.decimals, got, tc.want)
			}
			if got := out[solanaKeyMintSymbol]; got != "USDC" {
				t.Fatalf("plan mint_symbol = %q, want USDC", got)
			}
		})
	}
}

// TestSolanaAdapter_DeclarativeMintRejectsUnreadableMint: an unreadable mint
// must fail the publish loudly. There is no default decimals to fall back to —
// a guessed 6 would write a permanently wrong on-chain plan amount (#817).
func TestSolanaAdapter_DeclarativeMintRejectsUnreadableMint(t *testing.T) {
	key, _ := solanago.NewRandomPrivateKey()
	hours := 30 * 24
	days := 30
	plan := recurring.NewPlanServiceWithReader(
		&planSubmitterStub{merchantPub: key.PublicKey()},
		mintReaderStub{mint: testUSDCMint, absent: true},
		"mainnet",
		map[string]config.TokenConfig{"USDC": {Mint: testUSDCMint}},
	)
	a := &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))

	if _, err := a.Attach(ctx, map[string]string{
		solanaKeyToken: "USDC",
	}, autoCreateContext{
		PriceID:             uuid.New(),
		ProductKey:          "premium",
		Currency:            "usd",
		UnitAmount:          10_000_000,
		AccessDurationHours: &hours,
		BillingCycleDays:    &days,
	}); err == nil {
		t.Fatal("expected Attach to reject a token whose mint cannot be read on-chain")
	}
}

// TestSolanaAdapter_AutoCreateRejectsUnarmedChainReader: with no chain reader
// there is no authoritative decimals source, so the publish must refuse rather
// than assume one.
func TestSolanaAdapter_AutoCreateRejectsUnarmedChainReader(t *testing.T) {
	key, _ := solanago.NewRandomPrivateKey()
	hours := 30 * 24
	days := 30
	plan := recurring.NewPlanServiceWithReader(
		&planSubmitterStub{merchantPub: key.PublicKey()}, nil, "mainnet",
		map[string]config.TokenConfig{"USDC": {Mint: testUSDCMint}},
	)
	a := &solanaAdapter{svc: &Service{rt: &app.Runtime{SolanaPlanService: plan}}}
	ctx := merchant.WithID(context.Background(), merchant.ID(uuid.New()))

	if _, err := a.AutoCreate(ctx, autoCreateContext{
		PriceID:             uuid.New(),
		ProductKey:          "premium",
		Currency:            "usd",
		UnitAmount:          10_000_000,
		AccessDurationHours: &hours,
		BillingCycleDays:    &days,
	}); err == nil {
		t.Fatal("expected AutoCreate to refuse publishing with no chain reader armed")
	}
}
