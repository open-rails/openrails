package recurring

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// tierChangeLifecycle is the membership surface the mirror drives (satisfied by
// *subscriptions.SubscriptionLifecycleService): cancel the OLD membership and
// create the NEW one.
type tierChangeLifecycle interface {
	CancelMembershipTx(ctx context.Context, txDB *db.DB, params *submod.CancelMembershipParams) (*submod.CancelMembershipTxResult, error)
	CreateMembershipTx(ctx context.Context, txDB *db.DB, params *submod.CreateMembershipParams) (*models.Subscription, []*models.NotificationQueue, error)
	DispatchNotifications(ctx context.Context, notifications []*models.NotificationQueue)
}

type tierChangeTransactor interface {
	MerchantTx(ctx context.Context, fn func(context.Context, pgx.Tx) error) error
}

// tierChangeStore is the solana_subscriptions store the mirror reads and
// writes; GetBySubscriptionPDA is the idempotency check.
type tierChangeStore interface {
	GetBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.SolanaSubscription, error)
	GetBySubscriptionPDA(ctx context.Context, pda string) (*models.SolanaSubscription, error)
	UpsertTx(ctx context.Context, txDB *db.DB, s *models.SolanaSubscription) error
	SetStatusTx(ctx context.Context, txDB *db.DB, id uuid.UUID, status string) error
}

// ConfirmTierChangeInput describes a confirmed on-chain tier change to mirror.
// The OLD on-chain identifiers are loaded from the stored solana_subscriptions
// row; the NEW terms are the canonical server-resolved values from the new
// price's plan config (never client-supplied).
type ConfirmTierChangeInput struct {
	Signature string // the wallet's atomic tier-change tx signature

	// CheckoutAttemptID and Reference bind a checkout-driven change: the
	// transaction must carry the checkout's reference and is claimed for it.
	// Both empty for the subscription route, which binds by the subscription
	// accounts alone.
	CheckoutAttemptID uuid.UUID
	Reference         string

	// OldSubscriptionID is the lifecycle subscription being changed FROM (the
	// caller has already authorized ownership).
	OldSubscriptionID uuid.UUID

	// Acting user + new-plan identity (for the new membership).
	UserID     string
	NewPriceID uuid.UUID

	// New on-chain subscription account the atomic tx created (from the prepare
	// step's result). This is the rail_subscription_id of the new membership +
	// the subscription_pda of the new row.
	NewSubscriptionPDA string

	// New plan terms (canonical, from the new price's Solana rail config).
	NewPlanID          uint64
	NewMintSymbol      string
	NewAmountBaseUnits uint64 // full per-cycle pull amount (token base units)
	NewPeriodHours     uint64
	NewPlanCreatedAt   int64

	// NewFiatAmount is the full price; the signed first-charge quote cannot exceed it.
	NewFiatAmount int64
	NewCurrency   string

	// IsUpgrade selects the first pull and recorded charge: an upgrade pulls
	// period 1 in the tx (next pull = now + new period, prorated charge
	// recorded); a downgrade charges nothing and defers the first pull to the
	// old period end.
	IsUpgrade bool

	// FirstChargeBaseUnits is the prorated first pull the upgrade must carry.
	// Zero accepts the merchant-co-signed pull; its signed fiat quote is recorded
	// on the payment. Ignored for downgrades.
	FirstChargeBaseUnits uint64

	// OldPeriodEndsAt is the OLD lifecycle subscription's CurrentPeriodEndsAt — the
	// downgrade's deferred first-pull time. Required for a downgrade.
	OldPeriodEndsAt *time.Time
}

// ConfirmTierChangeResult is the mirrored new subscription.
type ConfirmTierChangeResult struct {
	// NewSubscription is the new lifecycle membership.
	NewSubscription *models.Subscription
	// AlreadyConfirmed is true when a prior confirm already mirrored this tier
	// change (idempotent re-confirm returns the existing new subscription).
	AlreadyConfirmed bool
}

// ConfirmTierChangeService confirms the subscriber's atomic tier-change tx
// (built by PrepareTierChangeService). It proves from the chain that the tx is
// subscriber-signed, cancels the old subscription, subscribes to the new plan's
// terms and, for an upgrade, pulls the co-signed prorated charge. Only then
// does it mirror in one DB transaction: cancel the old membership and row
// (releasing the tier-group slot), create the new membership and upsert the new
// active row. Idempotent: an existing new row returns its subscription.
type ConfirmTierChangeService struct {
	chain      landedTxReader
	lifecycle  tierChangeLifecycle
	store      tierChangeStore
	transactor tierChangeTransactor
	network    string
	tokens     map[string]config.TokenConfig
	now        func() time.Time
}

// NewConfirmTierChangeService builds a ConfirmTierChangeService. network
// ("mainnet"/"devnet") resolves the recurring mint for the new row.
func NewConfirmTierChangeService(chain landedTxReader, lifecycle tierChangeLifecycle, store tierChangeStore, transactor tierChangeTransactor, network string, tokens ...map[string]config.TokenConfig) *ConfirmTierChangeService {
	return &ConfirmTierChangeService{
		chain:      chain,
		lifecycle:  lifecycle,
		store:      store,
		transactor: transactor,
		network:    network,
		tokens:     normalizeRecurringTokens(firstTokenMap(tokens)),
		now:        time.Now,
	}
}

// Confirm verifies the landed transaction is this tier change and mirrors it.
// Anything short of a successful, confirmed transaction proving the switch
// returns an error and leaves the DB untouched.
func (s *ConfirmTierChangeService) Confirm(ctx context.Context, in ConfirmTierChangeInput) (*ConfirmTierChangeResult, error) {
	if in.OldSubscriptionID == uuid.Nil {
		return nil, fmt.Errorf("recurring: old subscription id is required")
	}
	if in.Signature == "" {
		return nil, fmt.Errorf("recurring: signature is required")
	}
	if in.NewSubscriptionPDA == "" {
		return nil, fmt.Errorf("recurring: new subscription pda is required")
	}
	if in.NewAmountBaseUnits == 0 || in.NewPeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid new plan terms (amount/period)")
	}
	if !in.IsUpgrade && (in.OldPeriodEndsAt == nil || in.OldPeriodEndsAt.IsZero()) {
		return nil, fmt.Errorf("recurring: downgrade requires the old period end (deferred first pull)")
	}

	if s.transactor == nil {
		return nil, fmt.Errorf("recurring: tier-change transaction manager is required")
	}

	// Idempotent: a new row means a prior confirm mirrored this change. Keyed
	// by the new PDA because the old row is canceled in place.
	if existing, err := s.store.GetBySubscriptionPDA(ctx, in.NewSubscriptionPDA); err == nil && existing != nil && existing.SubscriptionID != uuid.Nil {
		return &ConfirmTierChangeResult{
			NewSubscription:  &models.Subscription{ID: existing.SubscriptionID},
			AlreadyConfirmed: true,
		}, nil
	}

	// The old row is canceled and carries its identity onto the new row.
	oldRow, err := s.store.GetBySubscriptionID(ctx, in.OldSubscriptionID)
	if err != nil {
		return nil, fmt.Errorf("recurring: load old solana subscription: %w", err)
	}
	if oldRow == nil {
		return nil, fmt.Errorf("recurring: no solana subscription for %s", in.OldSubscriptionID)
	}

	// The switch (cancel-old + subscribe-new [+ transfer]) is all-or-nothing
	// on-chain, so one landed transaction proving every part proves the change.
	payment, err := fetchLanded(ctx, s.chain, in.Signature)
	if err != nil {
		return nil, err
	}
	charged, err := s.verifyLanded(payment, oldRow, in)
	if err != nil {
		return nil, err
	}
	var chargeAmount int64
	if in.IsUpgrade {
		chargeAmount, err = payment.tierChangeCharge(in.NewCurrency, in.NewFiatAmount)
		if err != nil {
			return nil, err
		}
	}

	// Mirror in one DB transaction: a failure leaves no partial state for the
	// idempotency guard to mistake for a completed switch.

	now := s.now().UTC()
	if landedAt := payment.landedAt(); landedAt != nil {
		now = *landedAt
	}
	newPeriodStart := now
	var newPeriodEnd time.Time
	if in.IsUpgrade {
		// Period 1 was pulled atomically in the tx, so the membership's current
		// period is [now, now+new_period] and the cranker's first pull is period 2.
		newPeriodHoursI64, safeErr := safecast.Convert[int64](in.NewPeriodHours)
		if safeErr != nil {
			return nil, fmt.Errorf("recurring: confirm tier-change: new period hours overflow: %w", safeErr)
		}
		newPeriodEnd = now.Add(time.Duration(newPeriodHoursI64) * time.Hour)
	} else {
		// Downgrade: no charge now; the paid higher tier runs to the old
		// period end, then the lower tier rebills.
		newPeriodEnd = in.OldPeriodEndsAt.UTC()
	}

	newPDA := in.NewSubscriptionPDA

	// Recorded first charge: the prorated amount actually pulled on-chain for an
	// upgrade; nothing for a downgrade (deferred, no charge).
	var paymentMeta map[string]any
	if in.IsUpgrade {
		paymentMeta = map[string]any{
			"solana_tier_change":             "upgrade",
			"solana_first_charge_base_units": charged,
		}
	} else {
		paymentMeta = map[string]any{"solana_tier_change": "downgrade"}
	}

	createParams := &submod.CreateMembershipParams{
		UserID:                in.UserID,
		PriceID:               in.NewPriceID,
		Rail:                  models.RailSolana,
		RailSubscriptionID:    &newPDA,
		Amount:                chargeAmount,
		AmountProvided:        true,
		Currency:              in.NewCurrency,
		CurrentPeriodStartsAt: &newPeriodStart,
		CurrentPeriodEndsAt:   &newPeriodEnd,
		PaymentMetadata:       paymentMeta,
	}
	if in.IsUpgrade {
		createParams.TransactionID = in.Signature
	}

	var (
		newSub        *models.Subscription
		notifications []*models.NotificationQueue
	)
	err = s.transactor.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := db.NewWithPgxTx(tx)
		if in.CheckoutAttemptID != uuid.Nil {
			if err := settlement.ClaimCheckout(ctx, txDB, in.CheckoutAttemptID, in.Signature); err != nil {
				return err
			}
		}

		// Release the old row's live tier-group slot before inserting the new
		// membership. Both operations share this transaction, so any later error
		// restores the old membership and its access automatically.
		cancelType := models.CancelTypeMerchant
		cancelResult, cancelErr := s.lifecycle.CancelMembershipTx(ctx, txDB, &submod.CancelMembershipParams{
			SubscriptionID: &in.OldSubscriptionID,
			CancelType:     cancelType,
			RevokeAccess:   true,
		})
		if cancelErr != nil {
			return fmt.Errorf("recurring: mirror old-membership cancel: %w", cancelErr)
		}
		if oldRow.ID != uuid.Nil {
			if err := s.store.SetStatusTx(ctx, txDB, oldRow.ID, models.SolanaSubscriptionCanceled); err != nil {
				return fmt.Errorf("recurring: mark old solana subscription canceled: %w", err)
			}
		}

		var createNotifications []*models.NotificationQueue
		newSub, createNotifications, err = s.lifecycle.CreateMembershipTx(ctx, txDB, createParams)
		if err != nil {
			return fmt.Errorf("recurring: create new membership: %w", err)
		}

		// Subscriber + merchant + authority are unchanged. The new plan and
		// subscription PDAs identify the newly mirrored on-chain membership.
		newRow, buildErr := s.buildNewRow(oldRow, newSub.ID, in, newPDA, newPeriodEnd)
		if buildErr != nil {
			return buildErr
		}
		if in.IsUpgrade {
			newRow.LastSignature = &in.Signature
			newRow.LastPulledPeriodStartsAt = &newPeriodStart
		}
		if err := s.store.UpsertTx(ctx, txDB, newRow); err != nil {
			return fmt.Errorf("recurring: persist new solana subscription: %w", err)
		}

		notifications = append(notifications, createNotifications...)
		if cancelResult != nil {
			notifications = append(notifications, cancelResult.Notifications...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.lifecycle.DispatchNotifications(ctx, notifications)

	return &ConfirmTierChangeResult{NewSubscription: newSub}, nil
}

// verifyLanded proves the landed tier-change transaction
// switched this subscriber from the old subscription to the new plan's terms,
// returning the upgrade's pulled amount (0 for a downgrade).
func (s *ConfirmTierChangeService) verifyLanded(payment *landedTx, oldRow *models.SolanaSubscription, in ConfirmTierChangeInput) (uint64, error) {
	var err error
	if in.CheckoutAttemptID != uuid.Nil {
		if err := payment.references(in.Reference); err != nil {
			return 0, err
		}
	}
	var keys [5]solanago.PublicKey
	for i, v := range []string{oldRow.SubscriberWallet, oldRow.MerchantAddress, s.newMint(oldRow, in.NewMintSymbol), oldRow.PlanPDA, oldRow.SubscriptionPDA} {
		if keys[i], err = solanago.PublicKeyFromBase58(v); err != nil {
			return 0, fmt.Errorf("recurring: stored solana subscription %s is malformed: %w", oldRow.SubscriptionPDA, err)
		}
	}
	subscriber, merchantKey, mint, oldPlan, oldSub := keys[0], keys[1], keys[2], keys[3], keys[4]
	if err := payment.cancels(subscriber, oldPlan, oldSub); err != nil {
		return 0, err
	}
	terms := subscription{
		subscriber: subscriber, merchant: merchantKey, mint: mint, planID: in.NewPlanID,
		amount: in.NewAmountBaseUnits, periodHours: in.NewPeriodHours, createdAt: in.NewPlanCreatedAt,
	}
	_, newSub, _, err := terms.addresses()
	if err != nil {
		return 0, err
	}
	if newSub.String() != in.NewSubscriptionPDA {
		return 0, fmt.Errorf("%w: new subscription %s is not the subscriber's account for plan %d", ErrPaymentUnverified, in.NewSubscriptionPDA, in.NewPlanID)
	}
	if err := payment.subscribes(terms); err != nil {
		return 0, err
	}
	if !in.IsUpgrade {
		return 0, nil
	}
	return payment.pulls(terms, in.FirstChargeBaseUnits)
}

func (l *landedTx) tierChangeCharge(currency string, maximum int64) (int64, error) {
	var amount int64
	found := false
	for _, ix := range l.tx.Message.Instructions {
		program, err := l.tx.ResolveProgramIDIndex(ix.ProgramIDIndex)
		if err != nil || !program.Equals(solanago.MemoProgramID) {
			continue
		}
		quote, ok := strings.CutPrefix(string(ix.Data), tierChangeQuotePrefix)
		if !ok {
			continue
		}
		quotedCurrency, value, ok := strings.Cut(quote, ":")
		if found || !ok || quotedCurrency != moneyutil.NormalizeCurrency(currency) {
			return 0, fmt.Errorf("%w: invalid tier-change quote", ErrPaymentUnverified)
		}
		amount, err = strconv.ParseInt(value, 10, 64)
		if err != nil || amount < 0 || amount > maximum {
			return 0, fmt.Errorf("%w: invalid tier-change charge", ErrPaymentUnverified)
		}
		found = true
	}
	if !found {
		return 0, fmt.Errorf("%w: missing signed tier-change quote", ErrPaymentUnverified)
	}
	return amount, nil
}

// newMint resolves the new plan's mint, falling back to the old row's mint
// (the same for a same-group change).
func (s *ConfirmTierChangeService) newMint(oldRow *models.SolanaSubscription, symbol string) string {
	if resolved, err := ResolveRecurringMintFromTokens(symbol, s.tokens); err == nil && resolved != "" {
		return resolved
	}
	return oldRow.Mint
}

// buildNewRow assembles the NEW solana_subscriptions row. It derives the new plan
// PDA from the old row's merchant + the new plan id.
func (s *ConfirmTierChangeService) buildNewRow(oldRow *models.SolanaSubscription, newSubID uuid.UUID, in ConfirmTierChangeInput, newPDA string, nextPullAt time.Time) (*models.SolanaSubscription, error) {
	mintStr := s.newMint(oldRow, in.NewMintSymbol)

	planPDAStr := oldRow.PlanPDA
	if merchant, perr := solanago.PublicKeyFromBase58(oldRow.MerchantAddress); perr == nil {
		if newPlanPDA, _, derr := subscriptions.DerivePlanPDA(merchant, in.NewPlanID); derr == nil {
			planPDAStr = newPlanPDA.String()
		}
	}

	return &models.SolanaSubscription{
		SubscriptionID:           newSubID,
		MerchantID:               oldRow.MerchantID,
		SubscriberWallet:         oldRow.SubscriberWallet,
		AuthorityPDA:             oldRow.AuthorityPDA,
		SubscriptionPDA:          newPDA,
		PlanPDA:                  planPDAStr,
		MerchantAddress:          oldRow.MerchantAddress,
		Mint:                     mintStr,
		PlanCreatedAtFingerprint: in.NewPlanCreatedAt,
		NextPullAt:               nextPullAt,
		Status:                   models.SolanaSubscriptionActive,
	}, nil
}
