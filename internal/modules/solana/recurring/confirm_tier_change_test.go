package recurring

import (
	"context"
	"errors"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/solanafake"
)

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

// tierChain is a subscriber on plan 7 of a merchant, on the loopback chain.
type tierChain struct {
	fake       *solanafake.Node
	rpc        *solanaint.RPCClient
	signer     solanaint.Signer
	merchant   solanago.PublicKey
	mint       solanago.PublicKey
	subscriber solanago.PrivateKey
	old        *models.SolanaSubscription
	now        time.Time
}

const tierCharge = 31_330_000

func newTierChain(t *testing.T) *tierChain {
	t.Helper()
	fake := solanafake.New()
	t.Cleanup(fake.Close)
	fake.Mint(testDevnetUSDCMint, 6)
	signer, _, merchantKey := newCranker(t)
	c := &tierChain{
		fake:     fake,
		rpc:      solanaint.NewRPCClientWithConfig(solanaint.RPCClientConfig{Endpoint: fake.URL(), Network: "devnet", LoopbackFixture: true}),
		signer:   signer,
		merchant: merchantKey,
		mint:     solanago.MustPublicKeyFromBase58(testDevnetUSDCMint),
		now:      time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}
	fake.Fund(merchantKey, c.mint, 0)
	return c.another(t)
}

// another is a new funded subscriber on plan 7 of the same merchant and chain.
func (c *tierChain) another(t *testing.T) *tierChain {
	t.Helper()
	o := *c
	o.subscriber = randKey(t)
	subscriber := o.subscriber.PublicKey()
	require.NoError(t, c.fake.Authority(subscriber, c.mint, 42))
	c.fake.Fund(subscriber, c.mint, 100_000_000)
	oldPlan, _, _ := subscriptions.DerivePlanPDA(c.merchant, 7)
	oldSub, _, _ := subscriptions.DeriveSubscriptionPDA(oldPlan, subscriber)
	authority, _, _ := subscriptions.DeriveSubscriptionAuthority(subscriber, c.mint)
	o.old = &models.SolanaSubscription{
		ID: uuid.New(), MerchantID: uuid.New(), SubscriptionID: uuid.New(),
		SubscriberWallet: subscriber.String(), AuthorityPDA: authority.String(),
		SubscriptionPDA: oldSub.String(), PlanPDA: oldPlan.String(), MerchantAddress: c.merchant.String(),
		Mint: testDevnetUSDCMint, Status: models.SolanaSubscriptionActive,
	}
	return &o
}

// prepare builds the atomic tier change as the server does and signs it as the
// subscriber.
func (c *tierChain) prepare(t *testing.T, upgrade bool) (ConfirmTierChangeInput, *solanago.Transaction) {
	t.Helper()
	res, err := NewPrepareTierChangeService(c.signer, c.rpc, "devnet", testTokens()).Prepare(context.Background(), PrepareTierChangeInput{
		MerchantID: testMerchantID, SubscriberWallet: c.old.SubscriberWallet, MintSymbol: "USDC",
		OldPlanPDA: c.old.PlanPDA, OldSubscriptionPDA: c.old.SubscriptionPDA,
		NewPlanID: 99, NewAmountBaseUnits: 50_000_000, NewPeriodHours: 720, NewPlanCreatedAt: 1_700_000_000,
		IsUpgrade: upgrade, FirstChargeBaseUnits: tierCharge,
	})
	require.NoError(t, err)
	oldEnd := c.now.Add(14 * 24 * time.Hour)
	return ConfirmTierChangeInput{
		OldSubscriptionID: c.old.SubscriptionID, UserID: "user-1", NewPriceID: uuid.New(),
		NewSubscriptionPDA: res.NewSubscriptionPDA, NewPlanID: 99, NewMintSymbol: "USDC",
		NewAmountBaseUnits: 50_000_000, NewPeriodHours: 720, NewPlanCreatedAt: 1_700_000_000,
		NewFiatAmount: 50_000_000, NewCurrency: "USD", IsUpgrade: upgrade, FirstChargeBaseUnits: tierCharge,
		OldPeriodEndsAt: &oldEnd,
	}, signAs(t, decodeTx(t, res.Transaction), c.subscriber)
}

// change prepares, signs and lands the tier change.
func (c *tierChain) change(t *testing.T, upgrade bool) (ConfirmTierChangeInput, solanago.Signature) {
	t.Helper()
	in, tx := c.prepare(t, upgrade)
	sig := c.land(t, tx)
	in.Signature = sig.String()
	return in, sig
}

func (c *tierChain) land(t *testing.T, tx *solanago.Transaction) solanago.Signature {
	t.Helper()
	sig, err := c.fake.Land(tx, c.now)
	require.NoError(t, err)
	return sig
}

func (c *tierChain) service(life *tierLifecycle, store *tierStore) *ConfirmTierChangeService {
	svc := NewConfirmTierChangeService(c.rpc, life, store, inlineTx{}, "devnet", testTokens())
	svc.now = func() time.Time { return c.now }
	return svc
}

// signAs completes key's signature slot of a partially signed transaction.
func signAs(t *testing.T, tx *solanago.Transaction, key solanago.PrivateKey) *solanago.Transaction {
	t.Helper()
	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	sig, err := key.Sign(msg)
	require.NoError(t, err)
	if len(tx.Signatures) < int(tx.Message.Header.NumRequiredSignatures) {
		tx.Signatures = make([]solanago.Signature, tx.Message.Header.NumRequiredSignatures)
	}
	for i, k := range tx.Message.AccountKeys[:tx.Message.Header.NumRequiredSignatures] {
		if k.Equals(key.PublicKey()) {
			tx.Signatures[i] = sig
			return tx
		}
	}
	t.Fatalf("%s is not a signer", key.PublicKey())
	return nil
}

// signedBy builds and fully signs a transaction the payer sends alone.
func signedBy(t *testing.T, c *tierChain, payer solanago.PrivateKey, ixs ...solanago.Instruction) *solanago.Transaction {
	t.Helper()
	hash, err := c.rpc.GetLatestBlockhash(context.Background())
	require.NoError(t, err)
	tx, err := solanago.NewTransaction(ixs, hash, solanago.TransactionPayer(payer.PublicKey()))
	require.NoError(t, err)
	tx.Signatures = nil
	return signAs(t, tx, payer)
}

// #272: after the atomic switch lands, the old membership+row are cancelled
// and the new ones created in one DB tx. Upgrade: period 1 was pulled in the
// tx, so the next pull is now+period. Downgrade: no charge; the first pull is
// deferred to the OLD period end.
func TestConfirmTierChangeMirrorsSwitch(t *testing.T) {
	for _, upgrade := range []bool{true, false} {
		c := newTierChain(t)
		in, sig := c.change(t, upgrade)
		store := &tierStore{oldRow: c.old}
		life := &tierLifecycle{newID: uuid.New()}

		res, err := c.service(life, store).Confirm(context.Background(), in)
		require.NoError(t, err)
		require.Equal(t, life.newID, res.NewSubscription.ID)
		require.False(t, res.AlreadyConfirmed)

		require.Len(t, life.cancels, 1)
		require.Equal(t, c.old.SubscriptionID, *life.cancels[0].SubscriptionID)
		require.True(t, life.cancels[0].RevokeAccess)
		require.Equal(t, []string{models.SolanaSubscriptionCancelled}, store.statuses)

		wantNext, wantMeta := *in.OldPeriodEndsAt, map[string]any{"solana_tier_change": "downgrade"}
		if upgrade {
			wantNext, wantMeta = c.now.Add(720*time.Hour), map[string]any{"solana_tier_change": "upgrade", "solana_first_charge_base_units": uint64(tierCharge)}
			require.Equal(t, uint64(tierCharge), c.fake.Balance(c.merchant, c.mint), "the upgrade charge moved")
		}
		require.Len(t, life.creates, 1)
		created := life.creates[0]
		require.Equal(t, in.NewSubscriptionPDA, *created.RailSubscriptionID)
		require.Equal(t, sig.String(), created.TransactionID)
		require.Equal(t, wantNext, *created.CurrentPeriodEndsAt)
		require.Equal(t, wantMeta, created.PaymentMetadata)

		require.Len(t, store.upserted, 1)
		row := store.upserted[0]
		newPlan, _, _ := subscriptions.DerivePlanPDA(c.merchant, 99)
		require.Equal(t, wantNext, row.NextPullAt)
		require.Equal(t, in.NewSubscriptionPDA, row.SubscriptionPDA)
		require.Equal(t, newPlan.String(), row.PlanPDA)
		require.Equal(t, testDevnetUSDCMint, row.Mint)
		require.Equal(t, c.old.SubscriberWallet, row.SubscriberWallet)
		require.Equal(t, models.SolanaSubscriptionActive, row.Status)
	}
}

// Only the landed tier change itself mirrors: any other successful signature,
// a switch without the co-signed charge, another subscriber's switch or a
// charge other than the quoted one changes nothing.
func TestConfirmTierChangeNeverMirrorsUnprovenSwitch(t *testing.T) {
	statusErr := errors.New("old mirror status failed")
	for _, tc := range []struct {
		name    string
		input   func(*testing.T, *tierChain) ConfirmTierChangeInput
		store   func(*tierStore)
		wantErr error
	}{
		{"any successful transaction", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, true)
			from, _, _ := solanago.FindAssociatedTokenAddress(c.subscriber.PublicKey(), c.mint)
			to, _, _ := solanago.FindAssociatedTokenAddress(c.merchant, c.mint)
			in.Signature = c.land(t, signedBy(t, c, c.subscriber, token.NewTransferInstruction(1, from, to, c.subscriber.PublicKey(), nil).Build())).String()
			return in
		}, nil, ErrPaymentUnverified},
		{"upgrade without the charge", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, false)
			in.IsUpgrade = true
			return in
		}, nil, ErrPaymentUnverified},
		{"another subscriber's switch", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			theirs, _ := c.another(t).change(t, true)
			in, _ := c.prepare(t, true)
			in.Signature = theirs.Signature
			return in
		}, nil, ErrPaymentUnverified},
		{"a charge other than the quote", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, true)
			in.FirstChargeBaseUnits = tierCharge + 1
			return in
		}, nil, ErrPaymentUnverified},
		{"a checkout change without the checkout's reference", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, true)
			in.CheckoutSessionID, in.Reference = uuid.New(), randAddr(t)
			return in
		}, nil, ErrPaymentUnverified},
		{"reverted", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, tx := c.prepare(t, true)
			c.fake.Fund(c.subscriber.PublicKey(), c.mint, 1)
			in.Signature = c.land(t, tx).String()
			return in
		}, nil, ErrPaymentUnverified},
		{"invalid signature", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, true)
			in.Signature = "not-a-real-signature"
			return in
		}, nil, ErrPaymentUnverified},
		{"downgrade without old period end", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, false)
			in.OldPeriodEndsAt = nil
			return in
		}, nil, nil},
		{"missing new pda", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, true)
			in.NewSubscriptionPDA = ""
			return in
		}, nil, nil},
		{"old row missing", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, true)
			return in
		}, func(s *tierStore) { s.oldRow = nil }, nil},
		{"old status write fails", func(t *testing.T, c *tierChain) ConfirmTierChangeInput {
			in, _ := c.change(t, true)
			return in
		}, func(s *tierStore) { s.statusErr = statusErr }, statusErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTierChain(t)
			in := tc.input(t, c)
			store := &tierStore{oldRow: c.old}
			if tc.store != nil {
				tc.store(store)
			}
			life := &tierLifecycle{newID: uuid.New()}
			_, err := c.service(life, store).Confirm(context.Background(), in)
			require.Error(t, err)
			if tc.wantErr != nil {
				require.ErrorIs(t, err, tc.wantErr)
			}
			if tc.wantErr != statusErr {
				require.Empty(t, life.creates)
				require.Empty(t, life.cancels)
			}
			require.Empty(t, store.upserted)
		})
	}
}

type chainUnread struct{ reads int }

func (c *chainUnread) GetTransaction(context.Context, solanago.Signature) (*rpc.GetTransactionResult, error) {
	c.reads++
	return nil, errors.New("unexpected chain read")
}

// Re-confirming a tier change already mirrored (new row exists) returns the
// existing subscription without touching the chain or the DB.
func TestConfirmTierChangeIsIdempotent(t *testing.T) {
	c := newTierChain(t)
	in, _ := c.change(t, true)
	existing := uuid.New()
	chain := &chainUnread{}
	store := &tierStore{oldRow: c.old, byNewPDA: &models.SolanaSubscription{SubscriptionID: existing}}
	life := &tierLifecycle{}

	res, err := NewConfirmTierChangeService(chain, life, store, inlineTx{}, "devnet", testTokens()).Confirm(context.Background(), in)
	require.NoError(t, err)
	require.True(t, res.AlreadyConfirmed)
	require.Equal(t, existing, res.NewSubscription.ID)
	require.Zero(t, chain.reads)
	require.Empty(t, life.cancels)
	require.Empty(t, life.creates)
}
