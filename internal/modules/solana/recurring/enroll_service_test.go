package recurring

import (
	"context"
	"encoding/binary"
	"testing"

	solanago "github.com/doujins-org/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

type fakeLifecycle struct {
	created *submod.CreateMembershipParams
	sub     *models.Subscription
	err     error
}

func (f *fakeLifecycle) CreateMembership(_ context.Context, p *submod.CreateMembershipParams) (*models.Subscription, error) {
	f.created = p
	return f.sub, f.err
}

type fakeBalance struct{ lamports uint64 }

func (f fakeBalance) GetBalance(context.Context, solanago.PublicKey) (uint64, error) {
	return f.lamports, nil
}

type fakeStore struct{ upserted *models.SolanaSubscription }

func (f *fakeStore) Upsert(_ context.Context, s *models.SolanaSubscription) error {
	f.upserted = s
	return nil
}

func newEnrollService(t *testing.T, balance uint64) (*EnrollService, *fakeLifecycle, *fakeStore) {
	t.Helper()
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	subID := uuid.New()
	lc := &fakeLifecycle{sub: &models.Subscription{ID: subID}}
	store := &fakeStore{}
	svc := NewEnrollService(NewCrankService(fs), lc, store, fakeBalance{lamports: balance}, fs, "mainnet")
	return svc, lc, store
}

func TestConfirmEnrollmentHappyPath(t *testing.T) {
	svc, lc, store := newEnrollService(t, 2_000_000) // PDA funded -> exists
	subscriber := newMerchant(t).String()
	sub, err := svc.ConfirmEnrollment(context.Background(), EnrollInput{
		TenantID: tenant.DefaultID, UserID: "user-1", PriceID: uuid.New(),
		SubscriberWallet: subscriber, PlanID: 7, MintSymbol: "USDC",
		AmountBaseUnits: 10_000_000, PeriodHours: 720, PlanCreatedAt: 1_700_000_000,
		FiatAmount: 1000, Currency: "usd",
	})
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if sub == nil {
		t.Fatal("expected a subscription")
	}
	// Membership created for Solana with the crank signature + period.
	if lc.created == nil || lc.created.Processor != models.ProcessorSolana || lc.created.TransactionID == "" {
		t.Fatalf("CreateMembership not called correctly: %+v", lc.created)
	}
	if lc.created.Amount != 1000 || lc.created.Currency != "usd" {
		t.Errorf("membership fiat amount/currency wrong: %+v", lc.created)
	}
	// Row persisted, linked to the lifecycle subscription + on-chain state.
	if store.upserted == nil || store.upserted.SubscriptionID != sub.ID ||
		store.upserted.Status != models.SolanaSubscriptionActive || store.upserted.SubscriptionPDA == "" {
		t.Fatalf("solana subscription row not persisted correctly: %+v", store.upserted)
	}
	if store.upserted.LastSignature == nil || *store.upserted.LastSignature != lc.created.TransactionID {
		t.Error("row last_signature should equal the first-crank signature")
	}
}

func TestConfirmEnrollmentRejectsMissingOnChainSubscription(t *testing.T) {
	svc, lc, store := newEnrollService(t, 0) // PDA not funded -> not subscribed
	_, err := svc.ConfirmEnrollment(context.Background(), EnrollInput{
		TenantID: tenant.DefaultID, UserID: "user-1", PriceID: uuid.New(),
		SubscriberWallet: newMerchant(t).String(), PlanID: 1, MintSymbol: "USDC",
		AmountBaseUnits: 10_000_000, PeriodHours: 720,
	})
	if err == nil {
		t.Fatal("expected error when on-chain subscription is absent")
	}
	if lc.created != nil || store.upserted != nil {
		t.Error("must not create membership or persist when subscription is absent")
	}
}

// crankAmountFromSubmitter decodes the u64 little-endian amount from the
// transfer_subscription instruction the cranker submitted (data bytes [1:9]).
func crankAmountFromSubmitter(t *testing.T, fs *fakeSubmitter) uint64 {
	t.Helper()
	if len(fs.lastInstrs) != 1 {
		t.Fatalf("expected exactly one submitted instruction, got %d", len(fs.lastInstrs))
	}
	data, derr := fs.lastInstrs[0].Data()
	if derr != nil {
		t.Fatalf("instruction data: %v", derr)
	}
	if len(data) < 9 {
		t.Fatalf("instruction data too short: %d", len(data))
	}
	return binary.LittleEndian.Uint64(data[1:9])
}

// TestConfirmEnrollmentFirstChargeOverride pins the #267 Model-B behavior: when
// FirstChargeBaseUnits is non-zero the FIRST crank pulls THAT amount (the reduced
// upgrade pull), while the persisted row keeps the full per-cycle AmountBaseUnits
// so every subsequent cranker pull is the full amount.
func TestConfirmEnrollmentFirstChargeOverride(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	subID := uuid.New()
	lc := &fakeLifecycle{sub: &models.Subscription{ID: subID}}
	store := &fakeStore{}
	svc := NewEnrollService(NewCrankService(fs), lc, store, fakeBalance{lamports: 2_000_000}, fs, "mainnet")

	const fullAmount = uint64(10_000_000) // 10 USDC full cycle
	const reducedFirst = uint64(3_134_000) // reduced Model-B first pull

	sub, err := svc.ConfirmEnrollment(context.Background(), EnrollInput{
		TenantID: tenant.DefaultID, UserID: "user-1", PriceID: uuid.New(),
		SubscriberWallet: newMerchant(t).String(), PlanID: 7, MintSymbol: "USDC",
		AmountBaseUnits: fullAmount, PeriodHours: 720, PlanCreatedAt: 1_700_000_000,
		FiatAmount: 5000, Currency: "usd",
		FirstChargeBaseUnits: reducedFirst,
	})
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if sub == nil {
		t.Fatal("expected a subscription")
	}
	// First crank pulled the REDUCED amount, not the full per-cycle amount.
	if got := crankAmountFromSubmitter(t, fs); got != reducedFirst {
		t.Fatalf("first crank amount: expected reduced %d, got %d", reducedFirst, got)
	}
	// Membership fiat amount is still the new full price (cents) — only the on-chain
	// first pull is reduced.
	if lc.created == nil || lc.created.Amount != 5000 {
		t.Fatalf("membership fiat amount should be the full new price: %+v", lc.created)
	}
}

// TestConfirmEnrollmentFirstChargeZeroUsesFull confirms a zero override falls
// back to the full AmountBaseUnits (a normal, non-upgrade subscribe).
func TestConfirmEnrollmentFirstChargeZeroUsesFull(t *testing.T) {
	fs := &fakeSubmitter{merchant: newMerchant(t)}
	lc := &fakeLifecycle{sub: &models.Subscription{ID: uuid.New()}}
	store := &fakeStore{}
	svc := NewEnrollService(NewCrankService(fs), lc, store, fakeBalance{lamports: 2_000_000}, fs, "mainnet")

	const fullAmount = uint64(10_000_000)
	_, err := svc.ConfirmEnrollment(context.Background(), EnrollInput{
		TenantID: tenant.DefaultID, UserID: "user-1", PriceID: uuid.New(),
		SubscriberWallet: newMerchant(t).String(), PlanID: 7, MintSymbol: "USDC",
		AmountBaseUnits: fullAmount, PeriodHours: 720,
		FiatAmount: 1000, Currency: "usd",
		FirstChargeBaseUnits: 0, // no override
	})
	if err != nil {
		t.Fatalf("ConfirmEnrollment: %v", err)
	}
	if got := crankAmountFromSubmitter(t, fs); got != fullAmount {
		t.Fatalf("first crank amount: expected full %d, got %d", fullAmount, got)
	}
}

func TestConfirmEnrollmentRejectsIneligibleToken(t *testing.T) {
	svc, _, _ := newEnrollService(t, 2_000_000)
	_, err := svc.ConfirmEnrollment(context.Background(), EnrollInput{
		TenantID: tenant.DefaultID, UserID: "u", PriceID: uuid.New(),
		SubscriberWallet: newMerchant(t).String(), PlanID: 1, MintSymbol: "PYUSD",
		AmountBaseUnits: 1, PeriodHours: 1,
	})
	if err == nil {
		t.Fatal("PYUSD must be rejected (not recurring-eligible)")
	}
}
