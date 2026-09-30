package recurring

import (
	"context"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
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

// tierChangeStore is the on-chain state store the mirror reads + writes
// (satisfied by *solanasubs.SolanaSubscriptionRepo). GetBySubscriptionPDA powers the
// idempotency/resumability check; SetStatus flips the old row cancelled; Upsert
// writes the new active row.
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

	// CheckoutSessionID and Reference bind a checkout-driven change: the
	// transaction must carry the checkout's reference and is claimed for it.
	// Both empty for the subscription route, which binds by the subscription
	// accounts alone.
	CheckoutSessionID uuid.UUID
	Reference         string

	// OldSubscriptionID is the lifecycle subscription being changed FROM (the
	// caller has already authorized ownership).
	OldSubscriptionID uuid.UUID

	// Acting user + new-plan identity (for the new membership).
	UserID     string
	UserEmail  string
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

	// NewFiatAmount/NewCurrency are price.Amount (micros) + currency recorded on
	// the new membership.
	NewFiatAmount int64
	NewCurrency   string

	// IsUpgrade selects the next_pull_at policy + the recorded first charge:
	//   UPGRADE   -> period 1 was pulled atomically in the tx, so the cranker's
	//                first pull is period 2: next_pull_at = now + new_period.
	//                The prorated first charge is recorded on the new membership.
	//   DOWNGRADE -> NO immediate charge; the first pull is DEFERRED to the OLD
	//                subscription's period end (the user keeps the higher tier
	//                they already paid for until then).
	IsUpgrade bool

	// FirstChargeBaseUnits is the prorated first pull the upgrade must carry.
	// 0 accepts the amount the merchant co-signed at prepare time (the
	// subscription route re-quotes at confirm and cannot reproduce it). The
	// pulled amount is recorded on the new membership. Ignored for downgrades.
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

// ConfirmTierChangeService is the CONFIRM step of the on-chain tier-change loop
// (#272). Solana is the source of truth: the subscriber signs + sends the single
// ATOMIC tier-change tx (built by PrepareTierChangeService), then posts its
// signature here. We read that transaction from the chain and require that it
// is the tier change: signed by the subscriber, cancelling the old
// subscription, subscribing to the new plan's terms and, for an upgrade,
// pulling the co-signed prorated charge into the merchant's account. Only then
// do we MIRROR it into the DB:
//
//   - atomically cancel the OLD membership + mirror row, releasing the
//     database's live tier-group slot;
//   - create the NEW membership (rail=solana, rail_subscription_id =
//     new subscription PDA, recording the prorated first charge for an upgrade /
//     no charge for a downgrade) + upsert the NEW solana_subscriptions row
//     (status active) with next_pull_at set per kind (upgrade => now+period;
//     downgrade => old period end). Any failure rolls the OLD cancellation back.
//
// It is idempotent/resumable: if the NEW row already exists (a prior confirm
// committed), it returns the existing new subscription without re-mirroring.
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

// Confirm verifies the landed transaction is this tier change and mirrors it
// into the DB. Returns an error (and does NOT mutate the DB) for any signature
// that is not a successful, confirmed transaction proving the switch — the
// chain is the source of truth, so an unproven switch must not touch openrails.
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

	// Idempotency/resumability: if the NEW row already exists, a prior confirm
	// already mirrored this tier change. Return the existing new subscription
	// without re-running the transactional mirror. We look up by the NEW
	// subscription PDA because the OLD row gets cancelled in-place.
	if existing, err := s.store.GetBySubscriptionPDA(ctx, in.NewSubscriptionPDA); err == nil && existing != nil && existing.SubscriptionID != uuid.Nil {
		return &ConfirmTierChangeResult{
			NewSubscription:  &models.Subscription{ID: existing.SubscriptionID},
			AlreadyConfirmed: true,
		}, nil
	}

	// Load the OLD on-chain row up front (we need it both to mirror the old-row
	// cancel and to carry the subscriber/merchant identity forward onto the new
	// row).
	oldRow, err := s.store.GetBySubscriptionID(ctx, in.OldSubscriptionID)
	if err != nil {
		return nil, fmt.Errorf("recurring: load old solana subscription: %w", err)
	}
	if oldRow == nil {
		return nil, fmt.Errorf("recurring: no solana subscription for %s", in.OldSubscriptionID)
	}

	// The switch (cancel-old + subscribe-new [+ transfer]) is all-or-nothing
	// on-chain, so one landed transaction proving every part proves the change.
	charged, err := s.verifyLanded(ctx, oldRow, in)
	if err != nil {
		return nil, err
	}

	// ---- MIRROR (billing-critical) ----
	// Create the new membership + mirror and cancel the old membership + mirror
	// in one database transaction. A failed mirror therefore leaves no partial
	// state for the idempotency guard to mistake for a completed switch.

	now := s.now().UTC()
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
		// Downgrade: no immediate charge. The user keeps the (higher-tier) access
		// they already paid for until the OLD period end; the lower tier rebills
		// then. The new membership's current period runs to the old period end.
		newPeriodEnd = in.OldPeriodEndsAt.UTC()
	}

	newPDA := in.NewSubscriptionPDA
	var emailPtr *string
	if in.UserEmail != "" {
		emailPtr = &in.UserEmail
	}

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
		UserEmail:             emailPtr,
		TransactionID:         in.Signature,
		Amount:                in.NewFiatAmount,
		AmountProvided:        true,
		Currency:              in.NewCurrency,
		CurrentPeriodStartsAt: &newPeriodStart,
		CurrentPeriodEndsAt:   &newPeriodEnd,
		PaymentMetadata:       paymentMeta,
	}

	var (
		newSub        *models.Subscription
		notifications []*models.NotificationQueue
	)
	err = s.transactor.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txDB := db.NewWithPgxTx(tx)
		if in.CheckoutSessionID != uuid.Nil {
			if err := settlement.ClaimCheckout(ctx, txDB, in.CheckoutSessionID, in.Signature); err != nil {
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
			if err := s.store.SetStatusTx(ctx, txDB, oldRow.ID, models.SolanaSubscriptionCancelled); err != nil {
				return fmt.Errorf("recurring: mark old solana subscription cancelled: %w", err)
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

// verifyLanded reads the tier-change transaction from the chain and proves it
// switched this subscriber from the old subscription to the new plan's terms,
// returning the upgrade's pulled amount (0 for a downgrade).
func (s *ConfirmTierChangeService) verifyLanded(ctx context.Context, oldRow *models.SolanaSubscription, in ConfirmTierChangeInput) (uint64, error) {
	payment, err := fetchLanded(ctx, s.chain, in.Signature)
	if err != nil {
		return 0, err
	}
	if in.CheckoutSessionID != uuid.Nil {
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
