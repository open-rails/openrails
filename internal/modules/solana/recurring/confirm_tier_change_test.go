package recurring

import (
	"context"
	"errors"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
)

// The failure tests observe attempted writes; successful persistence is tested
// by the SQL rollback/retry workflow.
type fakeTierLifecycle struct {
	createCalls int
	cancelCalls int
}

func (f *fakeTierLifecycle) CreateMembershipTx(context.Context, *db.DB, *submod.CreateMembershipParams) (*models.Subscription, []*models.NotificationQueue, error) {
	f.createCalls++
	return &models.Subscription{ID: uuid.New()}, nil, nil
}

func (f *fakeTierLifecycle) CancelMembershipTx(context.Context, *db.DB, *submod.CancelMembershipParams) (*submod.CancelMembershipTxResult, error) {
	f.cancelCalls++
	return &submod.CancelMembershipTxResult{}, nil
}

func (*fakeTierLifecycle) DispatchNotifications(context.Context, []*models.NotificationQueue) {}

type directTierTransactor struct{}

func (directTierTransactor) MerchantTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	return fn(ctx, nil)
}

// fakeTierStore captures scheduling and attempted writes on failure paths.
type fakeTierStore struct {
	oldRow      *models.SolanaSubscription
	upserted    *models.SolanaSubscription
	upsertCalls int
	statusErr   error
}

func (f *fakeTierStore) GetBySubscriptionID(_ context.Context, _ uuid.UUID) (*models.SolanaSubscription, error) {
	return f.oldRow, nil
}
func (f *fakeTierStore) GetBySubscriptionPDA(_ context.Context, _ string) (*models.SolanaSubscription, error) {
	return nil, nil
}
func (f *fakeTierStore) UpsertTx(_ context.Context, _ *db.DB, s *models.SolanaSubscription) error {
	f.upsertCalls++
	f.upserted = s
	return nil
}
func (f *fakeTierStore) SetStatusTx(context.Context, *db.DB, uuid.UUID, string) error {
	return f.statusErr
}

func TestConfirmTierChange_OldMirrorStatusFailureIsReturned(t *testing.T) {
	oldRow := newOldRow()
	injected := errors.New("old mirror status failed")
	store := &fakeTierStore{oldRow: oldRow, statusErr: injected}
	life := &fakeTierLifecycle{}
	svc := NewConfirmTierChangeService(&fakeConfirmRPC{outcome: okOutcome()}, life, store, directTierTransactor{}, "mainnet")
	in := baseConfirmInput(oldRow.SubscriptionID)
	in.IsUpgrade = true

	_, err := svc.Confirm(context.Background(), in)
	if !errors.Is(err, injected) {
		t.Fatalf("Confirm error = %v, want injected SetStatus failure", err)
	}
	if life.createCalls != 0 {
		t.Fatal("new membership must not be created after the old mirror status write fails")
	}
}

func newOldRow() *models.SolanaSubscription {
	merchant, _ := solanago.NewRandomPrivateKey()
	subscriber, _ := solanago.NewRandomPrivateKey()
	return &models.SolanaSubscription{
		ID:               uuid.New(),
		MerchantID:       uuid.New(),
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

func TestConfirmTierChange_Downgrade_DefersFirstPullToOldPeriodEnd(t *testing.T) {
	oldRow := newOldRow()
	store := &fakeTierStore{oldRow: oldRow}
	life := &fakeTierLifecycle{}
	svc := NewConfirmTierChangeService(&fakeConfirmRPC{outcome: okOutcome()}, life, store, directTierTransactor{}, "mainnet")

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
		life, store, directTierTransactor{}, "mainnet",
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
		life, store, directTierTransactor{}, "mainnet",
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

func TestConfirmTierChange_Downgrade_RequiresOldPeriodEnd(t *testing.T) {
	oldRow := newOldRow()
	store := &fakeTierStore{oldRow: oldRow}
	svc := NewConfirmTierChangeService(&fakeConfirmRPC{outcome: okOutcome()}, &fakeTierLifecycle{}, store, directTierTransactor{}, "mainnet")
	in := baseConfirmInput(oldRow.SubscriptionID)
	in.IsUpgrade = false // no OldPeriodEndsAt
	if _, err := svc.Confirm(context.Background(), in); err == nil {
		t.Fatal("downgrade without an old period end must error (deferred pull has no target time)")
	}
}
