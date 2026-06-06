package recurring

import (
	"context"
	"errors"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
)

// fakeTierLifecycle records the CreateMembership + CancelMembership calls.
type fakeTierLifecycle struct {
	created     *submod.CreateMembershipParams
	cancelled   *submod.CancelMembershipParams
	createErr   error
	cancelErr   error
	newSubID    uuid.UUID
	createCalls int
	cancelCalls int
}

func (f *fakeTierLifecycle) CreateMembership(_ context.Context, p *submod.CreateMembershipParams) (*models.Subscription, error) {
	f.createCalls++
	f.created = p
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := f.newSubID
	if id == uuid.Nil {
		id = uuid.New()
		f.newSubID = id
	}
	return &models.Subscription{ID: id}, nil
}

func (f *fakeTierLifecycle) CancelMembership(_ context.Context, p *submod.CancelMembershipParams) error {
	f.cancelCalls++
	f.cancelled = p
	return f.cancelErr
}

// fakeTierStore stubs the on-chain state store. existingByPDA powers the
// idempotency guard; upserted captures the new row; statusSet captures SetStatus.
type fakeTierStore struct {
	oldRow        *models.SolanaSubscription
	existingByPDA *models.SolanaSubscription
	upserted      *models.SolanaSubscription
	statusSet     map[uuid.UUID]string
	upsertCalls   int
}

func (f *fakeTierStore) GetBySubscriptionID(_ context.Context, _ uuid.UUID) (*models.SolanaSubscription, error) {
	return f.oldRow, nil
}
func (f *fakeTierStore) GetBySubscriptionPDA(_ context.Context, _ string) (*models.SolanaSubscription, error) {
	return f.existingByPDA, nil
}
func (f *fakeTierStore) Upsert(_ context.Context, s *models.SolanaSubscription) error {
	f.upsertCalls++
	f.upserted = s
	return nil
}
func (f *fakeTierStore) SetStatus(_ context.Context, id uuid.UUID, status string) error {
	if f.statusSet == nil {
		f.statusSet = map[uuid.UUID]string{}
	}
	f.statusSet[id] = status
	return nil
}

func newOldRow() *models.SolanaSubscription {
	merchant, _ := solanago.NewRandomPrivateKey()
	subscriber, _ := solanago.NewRandomPrivateKey()
	return &models.SolanaSubscription{
		ID:               uuid.New(),
		TenantID:         uuid.New(),
		SubscriptionID:   uuid.New(),
		SubscriberWallet: subscriber.PublicKey().String(),
		AuthorityPDA:     subscriber.PublicKey().String(),
		SubscriptionPDA:  "OLDPDA",
		PlanPDA:          "OLDPLAN",
		MerchantAddress:  merchant.PublicKey().String(),
		Mint:             "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
		Status:           models.SolanaSubscriptionActive,
	}
}

func okOutcome() *solanaint.TransactionOutcome { return &solanaint.TransactionOutcome{Err: nil} }

func baseConfirmInput(oldSubID uuid.UUID) ConfirmTierChangeInput {
	return ConfirmTierChangeInput{
		Signature:          solanago.Signature{}.String(),
		OldSubscriptionID:  oldSubID,
		UserID:             "user-1",
		NewPriceID:         uuid.New(),
		NewSubscriptionPDA: "NEWPDA",
		NewPlanID:          99,
		NewMintSymbol:      "USDC",
		NewAmountBaseUnits: 50_000_000,
		NewPeriodHours:     720,
		NewPlanCreatedAt:   1_700_000_000,
		NewFiatAmount:      5000,
		NewCurrency:        "USD",
	}
}

func TestConfirmTierChange_Upgrade_MirrorsNewAndCancelsOld(t *testing.T) {
	oldRow := newOldRow()
	store := &fakeTierStore{oldRow: oldRow}
	life := &fakeTierLifecycle{}
	frozen := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	svc := NewConfirmTierChangeService(&fakeConfirmRPC{outcome: okOutcome()}, life, store, "mainnet")
	svc.now = func() time.Time { return frozen }

	in := baseConfirmInput(oldRow.SubscriptionID)
	in.IsUpgrade = true
	in.FirstChargeBaseUnits = 31_330_000

	res, err := svc.Confirm(context.Background(), in)
	if err != nil {
		t.Fatalf("Confirm upgrade: %v", err)
	}
	if res.AlreadyConfirmed {
		t.Fatal("fresh confirm should not be AlreadyConfirmed")
	}
	if life.createCalls != 1 {
		t.Fatalf("expected one CreateMembership, got %d", life.createCalls)
	}
	if life.cancelCalls != 1 {
		t.Fatalf("expected one CancelMembership (old), got %d", life.cancelCalls)
	}
	// New membership: processor solana, processor_subscription_id = new PDA.
	if life.created.Processor != models.ProcessorSolana {
		t.Fatalf("new membership processor = %v, want solana", life.created.Processor)
	}
	if life.created.ProcessorSubscriptionID == nil || *life.created.ProcessorSubscriptionID != "NEWPDA" {
		t.Fatalf("new membership processor_subscription_id = %v, want NEWPDA", life.created.ProcessorSubscriptionID)
	}
	// Old membership cancelled immediately.
	if life.cancelled.SubscriptionID == nil || *life.cancelled.SubscriptionID != oldRow.SubscriptionID {
		t.Fatalf("cancelled wrong subscription: %+v", life.cancelled)
	}
	if !life.cancelled.RevokeAccess {
		t.Fatal("old cancel must be immediate (RevokeAccess=true)")
	}
	// UPGRADE next_pull_at = now + new_period (period 1 pulled atomically).
	wantNext := frozen.Add(720 * time.Hour)
	if !store.upserted.NextPullAt.Equal(wantNext) {
		t.Fatalf("upgrade next_pull_at = %v, want %v (now + new period)", store.upserted.NextPullAt, wantNext)
	}
	if store.upserted.Status != models.SolanaSubscriptionActive {
		t.Fatalf("new row status = %q, want active", store.upserted.Status)
	}
	if store.upserted.SubscriptionID != res.NewSubscription.ID {
		t.Fatal("new row must link to the new membership")
	}
	// Old on-chain row flipped cancelled.
	if store.statusSet[oldRow.ID] != models.SolanaSubscriptionCancelled {
		t.Fatalf("old row status = %q, want cancelled", store.statusSet[oldRow.ID])
	}
}

func TestConfirmTierChange_Downgrade_DefersFirstPullToOldPeriodEnd(t *testing.T) {
	oldRow := newOldRow()
	store := &fakeTierStore{oldRow: oldRow}
	life := &fakeTierLifecycle{}
	svc := NewConfirmTierChangeService(&fakeConfirmRPC{outcome: okOutcome()}, life, store, "mainnet")

	oldPeriodEnd := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	in := baseConfirmInput(oldRow.SubscriptionID)
	in.IsUpgrade = false
	in.OldPeriodEndsAt = &oldPeriodEnd

	if _, err := svc.Confirm(context.Background(), in); err != nil {
		t.Fatalf("Confirm downgrade: %v", err)
	}
	// DOWNGRADE next_pull_at = the OLD subscription's period end (deferred).
	if !store.upserted.NextPullAt.Equal(oldPeriodEnd) {
		t.Fatalf("downgrade next_pull_at = %v, want old period end %v", store.upserted.NextPullAt, oldPeriodEnd)
	}
	if life.cancelCalls != 1 {
		t.Fatal("downgrade must still cancel the old membership (atomic switch already happened on-chain)")
	}
}

func TestConfirmTierChange_OnChainFailure_NoMirror(t *testing.T) {
	oldRow := newOldRow()
	store := &fakeTierStore{oldRow: oldRow}
	life := &fakeTierLifecycle{}
	// Outcome with a non-nil Err -> Succeeded() == false.
	svc := NewConfirmTierChangeService(
		&fakeConfirmRPC{outcome: &solanaint.TransactionOutcome{Err: errors.New("reverted")}},
		life, store, "mainnet",
	)
	in := baseConfirmInput(oldRow.SubscriptionID)
	in.IsUpgrade = true
	in.FirstChargeBaseUnits = 1_000_000

	if _, err := svc.Confirm(context.Background(), in); err == nil {
		t.Fatal("a reverted on-chain tx must NOT mirror (expected error)")
	}
	if life.createCalls != 0 || life.cancelCalls != 0 || store.upsertCalls != 0 {
		t.Fatal("no DB mutation may happen when the on-chain switch did not succeed")
	}
}

func TestConfirmTierChange_NeverConfirmed_NoMirror(t *testing.T) {
	oldRow := newOldRow()
	store := &fakeTierStore{oldRow: oldRow}
	life := &fakeTierLifecycle{}
	svc := NewConfirmTierChangeService(
		&fakeConfirmRPC{err: errors.New("timed out")},
		life, store, "mainnet",
	)
	in := baseConfirmInput(oldRow.SubscriptionID)
	in.IsUpgrade = true
	in.FirstChargeBaseUnits = 1_000_000

	if _, err := svc.Confirm(context.Background(), in); err == nil {
		t.Fatal("an unconfirmed signature must NOT mirror (expected error)")
	}
	if life.createCalls != 0 || life.cancelCalls != 0 {
		t.Fatal("no DB mutation may happen when the signature never confirmed")
	}
}

func TestConfirmTierChange_Idempotent_ReturnsExisting(t *testing.T) {
	oldRow := newOldRow()
	existingNewSubID := uuid.New()
	store := &fakeTierStore{
		oldRow:        oldRow,
		existingByPDA: &models.SolanaSubscription{SubscriptionID: existingNewSubID, SubscriptionPDA: "NEWPDA"},
	}
	life := &fakeTierLifecycle{}
	rpcStub := &fakeConfirmRPC{outcome: okOutcome()}
	svc := NewConfirmTierChangeService(rpcStub, life, store, "mainnet")

	in := baseConfirmInput(oldRow.SubscriptionID)
	in.IsUpgrade = true
	in.FirstChargeBaseUnits = 1_000_000

	res, err := svc.Confirm(context.Background(), in)
	if err != nil {
		t.Fatalf("idempotent re-confirm: %v", err)
	}
	if !res.AlreadyConfirmed {
		t.Fatal("re-confirm of an already-mirrored tier change must report AlreadyConfirmed")
	}
	if res.NewSubscription.ID != existingNewSubID {
		t.Fatalf("re-confirm should return the existing new subscription %s, got %s", existingNewSubID, res.NewSubscription.ID)
	}
	if life.createCalls != 0 || life.cancelCalls != 0 || rpcStub.called {
		t.Fatal("a completed tier change must short-circuit before re-watching or re-mirroring")
	}
}

func TestConfirmTierChange_Downgrade_RequiresOldPeriodEnd(t *testing.T) {
	oldRow := newOldRow()
	store := &fakeTierStore{oldRow: oldRow}
	svc := NewConfirmTierChangeService(&fakeConfirmRPC{outcome: okOutcome()}, &fakeTierLifecycle{}, store, "mainnet")
	in := baseConfirmInput(oldRow.SubscriptionID)
	in.IsUpgrade = false // no OldPeriodEndsAt
	if _, err := svc.Confirm(context.Background(), in); err == nil {
		t.Fatal("downgrade without an old period end must error (deferred pull has no target time)")
	}
}
