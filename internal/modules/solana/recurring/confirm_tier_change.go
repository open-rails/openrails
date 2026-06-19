package recurring

import (
	"context"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
)

// tierChangeConfirmRPC confirms the wallet's tier-change transaction LANDED and
// SUCCEEDED on-chain (satisfied by *solanaint.RPCClient). Solana is the source of
// truth: we only mirror into the DB after observing the confirmed switch.
type tierChangeConfirmRPC interface {
	WatchTransaction(ctx context.Context, sig solanago.Signature, commitment rpc.CommitmentType, timeout time.Duration) (*solanaint.TransactionOutcome, error)
}

// tierChangeLifecycle is the membership surface the mirror drives (satisfied by
// *subscriptions.SubscriptionLifecycleService): cancel the OLD membership and
// create the NEW one.
type tierChangeLifecycle interface {
	CancelMembership(ctx context.Context, params *submod.CancelMembershipParams) error
	CreateMembership(ctx context.Context, params *submod.CreateMembershipParams) (*models.Subscription, error)
}

// tierChangeStore is the on-chain state store the mirror reads + writes
// (satisfied by *dbrepo.SolanaSubscriptionRepo). GetBySubscriptionPDA powers the
// idempotency/resumability check; SetStatus flips the old row cancelled; Upsert
// writes the new active row.
type tierChangeStore interface {
	GetBySubscriptionID(ctx context.Context, subscriptionID uuid.UUID) (*models.SolanaSubscription, error)
	GetBySubscriptionPDA(ctx context.Context, pda string) (*models.SolanaSubscription, error)
	Upsert(ctx context.Context, s *models.SolanaSubscription) error
	SetStatus(ctx context.Context, id uuid.UUID, status string) error
}

// ConfirmTierChangeInput describes a confirmed on-chain tier change to mirror.
// The OLD on-chain identifiers are loaded from the stored solana_subscriptions
// row; the NEW terms are the canonical server-resolved values from the new
// price's plan config (never client-supplied).
type ConfirmTierChangeInput struct {
	Signature string // the wallet's atomic tier-change tx signature

	// OldSubscriptionID is the lifecycle subscription being changed FROM (the
	// caller has already authorized ownership).
	OldSubscriptionID uuid.UUID

	// Acting user + new-plan identity (for the new membership).
	UserID     string
	UserEmail  string
	NewPriceID uuid.UUID

	// New on-chain subscription account the atomic tx created (from the prepare
	// step's result). This is the processor_subscription_id of the new membership +
	// the subscription_pda of the new row.
	NewSubscriptionPDA string

	// New plan terms (canonical, from the new price's Solana processor config).
	NewPlanID          uint64
	NewMintSymbol      string
	NewAmountBaseUnits uint64 // full per-cycle pull amount (token base units)
	NewPeriodHours     uint64
	NewPlanCreatedAt   int64

	// NewFiatAmount/NewCurrency are price.Amount (cents) + currency recorded on the
	// new membership.
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

	// FirstChargeBaseUnits is the prorated first pull recorded on the new
	// membership for an upgrade (informational; the on-chain transfer already
	// pulled it atomically). Ignored for downgrades.
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
// signature here. We confirm it landed + SUCCEEDED on-chain and only then MIRROR
// it into the DB:
//
//   - create the NEW membership (processor=solana, processor_subscription_id =
//     new subscription PDA, recording the prorated first charge for an upgrade /
//     no charge for a downgrade) + upsert the NEW solana_subscriptions row
//     (status active) with next_pull_at set per kind (upgrade => now+period;
//     downgrade => old period end);
//   - cancel the OLD membership (immediate) + flip the OLD solana_subscriptions
//     row to cancelled (the cranker stops pulling the old plan).
//
// It is idempotent/resumable: if the NEW row already exists (a prior confirm
// committed), it returns the existing new subscription without re-mirroring.
type ConfirmTierChangeService struct {
	rpc        tierChangeConfirmRPC
	lifecycle  tierChangeLifecycle
	store      tierChangeStore
	network    string
	tokens     map[string]config.TokenConfig
	commitment rpc.CommitmentType
	timeout    time.Duration
	now        func() time.Time
}

// NewConfirmTierChangeService builds a ConfirmTierChangeService. It confirms to
// the Confirmed commitment with the RPC client's default watch timeout. network
// ("mainnet"/"devnet") resolves the recurring mint for the new row.
func NewConfirmTierChangeService(rpcClient tierChangeConfirmRPC, lifecycle tierChangeLifecycle, store tierChangeStore, network string, tokens ...map[string]config.TokenConfig) *ConfirmTierChangeService {
	return &ConfirmTierChangeService{
		rpc:        rpcClient,
		lifecycle:  lifecycle,
		store:      store,
		network:    network,
		tokens:     normalizeRecurringTokens(firstTokenMap(tokens)),
		commitment: rpc.CommitmentConfirmed,
		timeout:    0, // 0 -> RPCClient.WatchTransaction default
		now:        time.Now,
	}
}

// Confirm verifies the atomic tier-change transaction landed + succeeded on-chain
// and mirrors it into the DB. Returns an error (and does NOT mutate the DB) if
// the signature never confirms or confirmed with an on-chain failure — the chain
// is the source of truth, so an unproven switch must not touch openrails.
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

	// Idempotency/resumability: if the NEW row already exists, a prior confirm
	// already mirrored this tier change. Return the existing new subscription
	// without re-running the (non-transactional) mirror. We look up by the NEW
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

	// Confirm on-chain: the single atomic tx must have LANDED and SUCCEEDED. The
	// switch (cancel-old + subscribe-new [+ transfer]) is all-or-nothing on-chain,
	// so one confirmed-success outcome proves the entire tier change executed.
	sig, err := solanago.SignatureFromBase58(in.Signature)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid signature: %w", err)
	}
	outcome, err := s.rpc.WatchTransaction(ctx, sig, s.commitment, s.timeout)
	if err != nil {
		return nil, fmt.Errorf("recurring: confirm tier-change signature %s: %w", in.Signature, err)
	}
	if !outcome.Succeeded() {
		return nil, fmt.Errorf("recurring: tier-change transaction did not succeed on-chain: %w", outcome.OnChainError())
	}

	// ---- MIRROR (billing-critical) ----
	// Order: create the NEW membership + row FIRST, then cancel the OLD. A retry
	// after a partial failure is safe because CreateMembership/Upsert are keyed on
	// the new PDA and CancelMembership is a no-op on an already-cancelled
	// membership; the idempotency guard above short-circuits a completed confirm.

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
			"solana_first_charge_base_units": in.FirstChargeBaseUnits,
		}
	} else {
		paymentMeta = map[string]any{"solana_tier_change": "downgrade"}
	}

	newSub, err := s.lifecycle.CreateMembership(ctx, &submod.CreateMembershipParams{
		UserID:                  in.UserID,
		PriceID:                 in.NewPriceID,
		Processor:               models.ProcessorSolana,
		ProcessorSubscriptionID: &newPDA,
		UserEmail:               emailPtr,
		TransactionID:           in.Signature,
		Amount:                  in.NewFiatAmount,
		AmountProvided:          true,
		Currency:                in.NewCurrency,
		CurrentPeriodStartsAt:   &newPeriodStart,
		CurrentPeriodEndsAt:     &newPeriodEnd,
		PaymentMetadata:         paymentMeta,
	})
	if err != nil {
		return nil, fmt.Errorf("recurring: create new membership: %w", err)
	}

	// Build the NEW on-chain row. Subscriber + merchant + authority are unchanged
	// (same wallet + mint, same merchant for a same-group tier change); the plan
	// PDA + subscription PDA are derived from the merchant + new plan id.
	newRow, err := s.buildNewRow(oldRow, newSub.ID, in, newPDA, newPeriodEnd)
	if err != nil {
		return nil, err
	}
	if err := s.store.Upsert(ctx, newRow); err != nil {
		return nil, fmt.Errorf("recurring: persist new solana subscription: %w", err)
	}

	// Mirror the OLD-side cancel LAST: cancel the membership immediately and flip
	// the old on-chain row to cancelled so the cranker stops pulling the old plan.
	// The on-chain cancel already happened atomically inside the tx — this is the
	// DB mirror. Idempotent: re-cancelling is a no-op.
	cancelType := models.CancelTypeMerchant
	if err := s.lifecycle.CancelMembership(ctx, &submod.CancelMembershipParams{
		SubscriptionID: &in.OldSubscriptionID,
		CancelType:     cancelType,
		RevokeAccess:   true,
	}); err != nil {
		return nil, fmt.Errorf("recurring: mirror old-membership cancel: %w", err)
	}
	// Belt-and-suspenders: ensure the old on-chain row is cancelled even if the
	// lifecycle cascade did not reach it. SetStatus is idempotent.
	if oldRow.ID != uuid.Nil {
		_ = s.store.SetStatus(ctx, oldRow.ID, models.SolanaSubscriptionCancelled)
	}

	return &ConfirmTierChangeResult{NewSubscription: newSub}, nil
}

// buildNewRow assembles the NEW solana_subscriptions row. It derives the new plan
// PDA from the old row's merchant + the new plan id and resolves the mint from the
// new symbol (falling back to the old row's mint, which is the same for a
// same-group change).
func (s *ConfirmTierChangeService) buildNewRow(oldRow *models.SolanaSubscription, newSubID uuid.UUID, in ConfirmTierChangeInput, newPDA string, nextPullAt time.Time) (*models.SolanaSubscription, error) {
	mintStr := oldRow.Mint
	if resolved, _, rerr := ResolveRecurringMintFromTokens(in.NewMintSymbol, s.tokens); rerr == nil && resolved != "" {
		mintStr = resolved
	}

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
