package recurring

import (
	"context"
	"errors"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
)

var zeroSig = solanago.Signature{}.String()

// watcher fakes the signature watch: landed-and-succeeded, landed-and-reverted,
// or never observed.
type watcher struct {
	outcome *solanaint.TransactionOutcome
	err     error
	calls   int
	comm    rpc.CommitmentType
}

func (w *watcher) WatchTransaction(_ context.Context, _ solanago.Signature, comm rpc.CommitmentType, _ solanaint.ChainTerminal) (*solanaint.TransactionOutcome, error) {
	w.calls++
	w.comm = comm
	return w.outcome, w.err
}

func landed() *watcher { return &watcher{outcome: &solanaint.TransactionOutcome{}} }
func reverted() *watcher {
	return &watcher{outcome: &solanaint.TransactionOutcome{Err: map[string]any{"InstructionError": []any{0, "Custom"}}}}
}
func unconfirmed() *watcher { return &watcher{err: errors.New("context deadline exceeded")} }

type canceller struct {
	params []*submod.CancelMembershipParams
}

func (c *canceller) CancelMembership(_ context.Context, p *submod.CancelMembershipParams) error {
	c.params = append(c.params, p)
	return nil
}

// The chain is the source of truth: only a confirmed-and-succeeded cancel
// mirrors, and it mirrors as a SCHEDULED period-end cancel, not a revoke.
func TestConfirmCancel(t *testing.T) {
	subID := uuid.New()
	for _, tc := range []struct {
		name   string
		w      *watcher
		subID  uuid.UUID
		sig    string
		mirror bool
	}{
		{"succeeded", landed(), subID, zeroSig, true},
		{"reverted", reverted(), subID, zeroSig, false},
		{"never confirmed", unconfirmed(), subID, zeroSig, false},
		{"invalid signature", landed(), subID, "not-base58!!", false},
		{"empty signature", landed(), subID, "", false},
		{"nil subscription", landed(), uuid.Nil, zeroSig, false},
	} {
		c := &canceller{}
		err := NewConfirmCancelService(tc.w, c).Confirm(context.Background(), tc.subID, tc.sig)
		if !tc.mirror {
			require.Error(t, err, tc.name)
			require.Empty(t, c.params, tc.name)
			continue
		}
		require.NoError(t, err)
		require.Equal(t, rpc.CommitmentConfirmed, tc.w.comm)
		require.Len(t, c.params, 1)
		require.Equal(t, subID, *c.params[0].SubscriptionID)
		require.False(t, c.params[0].RevokeAccess, "cancel at period end")
		require.Equal(t, models.CancelTypeUser, c.params[0].CancelType)
	}
}

type tierLifecycle struct {
	creates []*submod.CreateMembershipParams
	cancels []*submod.CancelMembershipParams
	newID   uuid.UUID
}

func (l *tierLifecycle) CreateMembershipTx(_ context.Context, _ *db.DB, p *submod.CreateMembershipParams) (*models.Subscription, []*models.NotificationQueue, error) {
	l.creates = append(l.creates, p)
	return &models.Subscription{ID: l.newID}, nil, nil
}

func (l *tierLifecycle) CancelMembershipTx(_ context.Context, _ *db.DB, p *submod.CancelMembershipParams) (*submod.CancelMembershipTxResult, error) {
	l.cancels = append(l.cancels, p)
	return &submod.CancelMembershipTxResult{}, nil
}

func (*tierLifecycle) DispatchNotifications(context.Context, []*models.NotificationQueue) {}

type inlineTx struct{}

func (inlineTx) MerchantTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
	return fn(ctx, nil)
}

type tierStore struct {
	oldRow    *models.SolanaSubscription
	byNewPDA  *models.SolanaSubscription
	statusErr error
	statuses  []string
	upserted  []*models.SolanaSubscription
}

func (s *tierStore) GetBySubscriptionID(context.Context, uuid.UUID) (*models.SolanaSubscription, error) {
	return s.oldRow, nil
}

func (s *tierStore) GetBySubscriptionPDA(context.Context, string) (*models.SolanaSubscription, error) {
	return s.byNewPDA, nil
}

func (s *tierStore) UpsertTx(_ context.Context, _ *db.DB, row *models.SolanaSubscription) error {
	s.upserted = append(s.upserted, row)
	return nil
}

func (s *tierStore) SetStatusTx(_ context.Context, _ *db.DB, _ uuid.UUID, status string) error {
	s.statuses = append(s.statuses, status)
	return s.statusErr
}

func oldTierRow(t *testing.T) *models.SolanaSubscription {
	return &models.SolanaSubscription{
		ID:               uuid.New(),
		MerchantID:       uuid.New(),
		SubscriptionID:   uuid.New(),
		SubscriberWallet: randAddr(t),
		AuthorityPDA:     randAddr(t),
		SubscriptionPDA:  randAddr(t),
		PlanPDA:          randAddr(t),
		MerchantAddress:  randAddr(t),
		Mint:             "OldMint",
		Status:           models.SolanaSubscriptionActive,
	}
}

func tierConfirmInput(old *models.SolanaSubscription) ConfirmTierChangeInput {
	return ConfirmTierChangeInput{
		Signature:            zeroSig,
		OldSubscriptionID:    old.SubscriptionID,
		UserID:               "user-1",
		NewPriceID:           uuid.New(),
		NewSubscriptionPDA:   "NEWPDA",
		NewPlanID:            99,
		NewMintSymbol:        "USDC",
		NewAmountBaseUnits:   50_000_000,
		NewPeriodHours:       720,
		NewPlanCreatedAt:     1_700_000_000,
		NewFiatAmount:        50_000_000,
		NewCurrency:          "USD",
		FirstChargeBaseUnits: 31_330_000,
	}
}

// #272: after the atomic switch lands, the old membership+row are cancelled
// and the new ones created in one DB tx. Upgrade: period 1 was pulled in the
// tx, so the next pull is now+period. Downgrade: no charge; the first pull is
// deferred to the OLD period end.
func TestConfirmTierChangeMirrorsSwitch(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	oldEnd := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		upgrade  bool
		wantNext time.Time
		wantMeta map[string]any
	}{
		{"upgrade", true, now.Add(720 * time.Hour), map[string]any{"solana_tier_change": "upgrade", "solana_first_charge_base_units": uint64(31_330_000)}},
		{"downgrade", false, oldEnd, map[string]any{"solana_tier_change": "downgrade"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := oldTierRow(t)
			store := &tierStore{oldRow: old}
			life := &tierLifecycle{newID: uuid.New()}
			svc := NewConfirmTierChangeService(landed(), life, store, inlineTx{}, "devnet", testTokens())
			svc.now = func() time.Time { return now }
			in := tierConfirmInput(old)
			in.IsUpgrade = tc.upgrade
			in.OldPeriodEndsAt = &oldEnd

			res, err := svc.Confirm(context.Background(), in)
			require.NoError(t, err)
			require.Equal(t, life.newID, res.NewSubscription.ID)
			require.False(t, res.AlreadyConfirmed)

			require.Len(t, life.cancels, 1)
			require.Equal(t, old.SubscriptionID, *life.cancels[0].SubscriptionID)
			require.True(t, life.cancels[0].RevokeAccess)
			require.Equal(t, []string{models.SolanaSubscriptionCancelled}, store.statuses)

			require.Len(t, life.creates, 1)
			created := life.creates[0]
			require.Equal(t, models.RailSolana, created.Rail)
			require.Equal(t, "NEWPDA", *created.RailSubscriptionID)
			require.Equal(t, zeroSig, created.TransactionID)
			require.Equal(t, tc.wantNext, *created.CurrentPeriodEndsAt)
			require.Equal(t, tc.wantMeta, created.PaymentMetadata)

			require.Len(t, store.upserted, 1)
			row := store.upserted[0]
			merchantPub := solanago.MustPublicKeyFromBase58(old.MerchantAddress)
			newPlan, _, _ := subscriptions.DerivePlanPDA(merchantPub, 99)
			require.Equal(t, tc.wantNext, row.NextPullAt)
			require.Equal(t, life.newID, row.SubscriptionID)
			require.Equal(t, "NEWPDA", row.SubscriptionPDA)
			require.Equal(t, newPlan.String(), row.PlanPDA)
			require.Equal(t, testDevnetUSDCMint, row.Mint)
			require.Equal(t, old.SubscriberWallet, row.SubscriberWallet)
			require.Equal(t, old.AuthorityPDA, row.AuthorityPDA)
			require.Equal(t, int64(1_700_000_000), row.PlanCreatedAtFingerprint)
			require.Equal(t, models.SolanaSubscriptionActive, row.Status)
		})
	}
}

func TestConfirmTierChangeNeverMirrorsUnprovenSwitch(t *testing.T) {
	statusErr := errors.New("old mirror status failed")
	for _, tc := range []struct {
		name    string
		w       *watcher
		mutate  func(*ConfirmTierChangeInput)
		store   func(*tierStore)
		wantErr error
	}{
		{"reverted", reverted(), nil, nil, nil},
		{"never confirmed", unconfirmed(), nil, nil, nil},
		{"downgrade without old period end", landed(), func(in *ConfirmTierChangeInput) { in.IsUpgrade = false }, nil, nil},
		{"missing new pda", landed(), func(in *ConfirmTierChangeInput) { in.NewSubscriptionPDA = "" }, nil, nil},
		{"old row missing", landed(), nil, func(s *tierStore) { s.oldRow = nil }, nil},
		{"old status write fails", landed(), nil, func(s *tierStore) { s.statusErr = statusErr }, statusErr},
	} {
		old := oldTierRow(t)
		store := &tierStore{oldRow: old}
		if tc.store != nil {
			tc.store(store)
		}
		life := &tierLifecycle{newID: uuid.New()}
		in := tierConfirmInput(old)
		in.IsUpgrade = true
		if tc.mutate != nil {
			tc.mutate(&in)
		}
		_, err := NewConfirmTierChangeService(tc.w, life, store, inlineTx{}, "devnet", testTokens()).Confirm(context.Background(), in)
		require.Error(t, err, tc.name)
		if tc.wantErr != nil {
			require.ErrorIs(t, err, tc.wantErr, tc.name)
		}
		require.Empty(t, life.creates, tc.name)
		require.Empty(t, store.upserted, tc.name)
	}
}

// Re-confirming a tier change already mirrored (new row exists) returns the
// existing subscription without touching the chain or the DB.
func TestConfirmTierChangeIsIdempotent(t *testing.T) {
	old := oldTierRow(t)
	existing := uuid.New()
	w := landed()
	store := &tierStore{oldRow: old, byNewPDA: &models.SolanaSubscription{SubscriptionID: existing}}
	life := &tierLifecycle{}
	in := tierConfirmInput(old)
	in.IsUpgrade = true

	res, err := NewConfirmTierChangeService(w, life, store, inlineTx{}, "devnet", testTokens()).Confirm(context.Background(), in)
	require.NoError(t, err)
	require.True(t, res.AlreadyConfirmed)
	require.Equal(t, existing, res.NewSubscription.ID)
	require.Zero(t, w.calls)
	require.Empty(t, life.cancels)
	require.Empty(t, life.creates)
}

type membershipRecorder struct {
	params *submod.CreateMembershipParams
	id     uuid.UUID
}

func (m *membershipRecorder) CreateMembership(_ context.Context, p *submod.CreateMembershipParams) (*models.Subscription, error) {
	m.params = p
	return &models.Subscription{ID: m.id}, nil
}

type rowRecorder struct{ rows []*models.SolanaSubscription }

func (r *rowRecorder) Upsert(_ context.Context, s *models.SolanaSubscription) error {
	r.rows = append(r.rows, s)
	return nil
}

type pdaPresent struct{}

func (pdaPresent) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	return []byte{1}, nil
}

// #286: the first period is pulled INSIDE the atomic subscribe tx, so a present
// subscription PDA proves payment; confirm creates the membership and the
// active row with no separate crank, keyed by the bundle signature (or the
// PDA when none was supplied).
func TestConfirmEnrollment(t *testing.T) {
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	for _, sig := range []string{"atomic-sig-123", ""} {
		_, submitter, merchantPub := newCranker(t)
		members := &membershipRecorder{id: uuid.New()}
		rows := &rowRecorder{}
		svc := NewEnrollService(members, rows, pdaPresent{}, submitter, "devnet", testTokens())
		svc.now = func() time.Time { return now }
		wallet := randAddr(t)

		sub, err := svc.ConfirmEnrollment(context.Background(), EnrollInput{
			MerchantID:       testMerchantID,
			UserID:           "user-1",
			PriceID:          uuid.New(),
			SubscriberWallet: wallet,
			PlanID:           7,
			MintSymbol:       "USDC",
			AmountBaseUnits:  10_000_000,
			PeriodHours:      720,
			PlanCreatedAt:    1_700_000_000,
			FiatAmount:       9_990_000,
			Currency:         "USD",
			Signature:        sig,
		})
		require.NoError(t, err)
		require.Equal(t, members.id, sub.ID)

		planPDA, _, _ := subscriptions.DerivePlanPDA(merchantPub, 7)
		subPDA, _, _ := subscriptions.DeriveSubscriptionPDA(planPDA, solanago.MustPublicKeyFromBase58(wallet))
		wantSig := sig
		if sig == "" {
			wantSig = subPDA.String()
		}
		periodEnd := now.Add(720 * time.Hour)
		require.Equal(t, wantSig, members.params.TransactionID)
		require.Equal(t, subPDA.String(), *members.params.RailSubscriptionID)
		require.Equal(t, periodEnd, *members.params.CurrentPeriodEndsAt)
		require.Equal(t, int64(9_990_000), members.params.Amount)

		require.Len(t, rows.rows, 1)
		row := rows.rows[0]
		require.Equal(t, models.SolanaSubscriptionActive, row.Status)
		require.Equal(t, wantSig, *row.LastSignature)
		require.Equal(t, periodEnd, row.NextPullAt, "next pull is period 2")
		require.Equal(t, testMerchantID.UUID(), row.MerchantID)
		require.Equal(t, members.id, row.SubscriptionID)
		require.Equal(t, planPDA.String(), row.PlanPDA)
	}
}
