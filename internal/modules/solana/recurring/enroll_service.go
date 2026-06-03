package recurring

import (
	"context"
	"fmt"
	"time"

	solanago "github.com/doujins-org/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/tenant"
)

// membershipCreator is the lifecycle surface enroll drives (satisfied by
// *subscriptions.SubscriptionLifecycleService).
type membershipCreator interface {
	CreateMembership(ctx context.Context, params *submod.CreateMembershipParams) (*models.Subscription, error)
}

// balanceChecker is the minimal RPC surface used to confirm the on-chain
// subscription exists (satisfied by *solana.RPCClient).
type balanceChecker interface {
	GetBalance(ctx context.Context, address solanago.PublicKey) (uint64, error)
}

// subscriptionStore persists the on-chain state row (satisfied by
// *dbrepo.SolanaSubscriptionRepo).
type subscriptionStore interface {
	Upsert(ctx context.Context, s *models.SolanaSubscription) error
}

// EnrollService activates a recurring Solana subscription after the user has
// signed init_subscription_authority + subscribe in their wallet (#255). It
// confirms the on-chain subscription exists, charges the first cycle (crank),
// creates the lifecycle membership, and persists the on-chain state row.
type EnrollService struct {
	cranker   *CrankService
	lifecycle membershipCreator
	repo      subscriptionStore
	rpc       balanceChecker
	submitter Submitter // for MerchantAddress
	network   string
	now       func() time.Time
}

// NewEnrollService builds an EnrollService.
func NewEnrollService(cranker *CrankService, lifecycle membershipCreator, repo subscriptionStore, rpc balanceChecker, submitter Submitter, network string) *EnrollService {
	return &EnrollService{cranker: cranker, lifecycle: lifecycle, repo: repo, rpc: rpc, submitter: submitter, network: network, now: time.Now}
}

// EnrollInput describes a confirmed wallet enrollment to activate.
type EnrollInput struct {
	TenantID         tenant.ID
	UserID           string
	UserEmail        string
	PriceID          uuid.UUID
	SubscriberWallet string

	// Plan terms (from the price's Solana processor config).
	PlanID          uint64
	MintSymbol      string
	AmountBaseUnits uint64 // on-chain pull amount (token base units) — the NORMAL full per-cycle amount
	PeriodHours     uint64
	PlanCreatedAt   int64 // ghost-plan fingerprint
	FiatAmount      int64 // price.Amount (cents) recorded on the payment
	Currency        string

	// FirstChargeBaseUnits, when non-zero, overrides the amount pulled on the
	// FIRST crank only (token base units). Used by the Model-B upgrade flow (#267)
	// to charge a reduced first pull (new_full - old_unused) while the persisted
	// row keeps the normal full AmountBaseUnits so every subsequent cranker pull is
	// the full per-cycle amount. Zero => use AmountBaseUnits for the first pull.
	FirstChargeBaseUnits uint64
}

// ConfirmEnrollment derives the on-chain PDAs, verifies the subscription exists
// (proving the user ran subscribe), charges the first cycle, creates the
// membership, and persists the billing.solana_subscriptions row.
func (s *EnrollService) ConfirmEnrollment(ctx context.Context, in EnrollInput) (*models.Subscription, error) {
	if in.UserID == "" || in.SubscriberWallet == "" {
		return nil, fmt.Errorf("recurring: user id and subscriber wallet are required")
	}
	if in.AmountBaseUnits == 0 || in.PeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid plan terms (amount/period)")
	}

	merchant, err := s.submitter.MerchantAddress(ctx, in.TenantID)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve merchant: %w", err)
	}
	mintStr, _, err := ResolveRecurringMint(in.MintSymbol, s.network)
	if err != nil {
		return nil, err
	}
	mint, err := solanago.PublicKeyFromBase58(mintStr)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid mint: %w", err)
	}
	subscriber, err := solanago.PublicKeyFromBase58(in.SubscriberWallet)
	if err != nil {
		return nil, fmt.Errorf("recurring: invalid subscriber wallet: %w", err)
	}

	planPDA, _, err := subscriptions.DerivePlanPDA(merchant, in.PlanID)
	if err != nil {
		return nil, err
	}
	subPDA, _, err := subscriptions.DeriveSubscriptionPDA(planPDA, subscriber)
	if err != nil {
		return nil, err
	}
	saPDA, _, err := subscriptions.DeriveSubscriptionAuthority(subscriber, mint)
	if err != nil {
		return nil, err
	}

	// Confirm the subscription exists on-chain (subscribe funds the PDA atomically).
	//
	// READ-AFTER-CONFIRM LAG: the wallet signed + sent the subscribe and confirmed
	// it client-side, so the server never saw that tx's slot to gate this read on.
	// A single GetBalance right after can hit a lagging RPC node that does not yet
	// see the just-created PDA (balance 0), spuriously rejecting a valid enrollment.
	// Poll until the PDA is funded (eventual consistency) — the production analogue
	// of the test-side awaitTokenCredit poller.
	bal, berr := solanaint.ReadUntilConsistent(ctx, solanaint.ReadUntilConsistentOpts{},
		func(ctx context.Context) (uint64, error) { return s.rpc.GetBalance(ctx, subPDA) },
		func(b uint64) bool { return b > 0 },
	)
	if berr != nil {
		return nil, fmt.Errorf("recurring: subscription %s not found on-chain — did the wallet run subscribe? (%w)", subPDA, berr)
	}
	if bal == 0 {
		return nil, fmt.Errorf("recurring: subscription %s not found on-chain — did the wallet run subscribe?", subPDA)
	}

	row := &models.SolanaSubscription{
		MerchantAddress:          merchant.String(),
		Mint:                     mint.String(),
		SubscriberWallet:         subscriber.String(),
		PlanPDA:                  planPDA.String(),
		SubscriptionPDA:          subPDA.String(),
		AuthorityPDA:             saPDA.String(),
		PlanCreatedAtFingerprint: in.PlanCreatedAt,
	}

	// First charge = the first crank. It must succeed to activate (no membership
	// without a paid first cycle — same as a card first charge). For a Model-B
	// upgrade (#267) the first pull is REDUCED (FirstChargeBaseUnits =
	// new_full - old_unused); every subsequent cranker pull uses the persisted
	// full AmountBaseUnits, so only this first cycle differs.
	firstPull := in.AmountBaseUnits
	if in.FirstChargeBaseUnits != 0 {
		firstPull = in.FirstChargeBaseUnits
	}
	sig, err := s.cranker.Crank(ctx, in.TenantID, row, firstPull)
	if err != nil {
		return nil, fmt.Errorf("recurring: first crank failed: %w", err)
	}

	now := s.now().UTC()
	periodEnd := now.Add(time.Duration(in.PeriodHours) * time.Hour)
	subPDAStr := subPDA.String()
	var emailPtr *string
	if in.UserEmail != "" {
		emailPtr = &in.UserEmail
	}
	sub, err := s.lifecycle.CreateMembership(ctx, &submod.CreateMembershipParams{
		UserID:                  in.UserID,
		PriceID:                 in.PriceID,
		Processor:               models.ProcessorSolana,
		ProcessorSubscriptionID: &subPDAStr,
		UserEmail:               emailPtr,
		TransactionID:           sig,
		Amount:                  in.FiatAmount,
		AmountProvided:          true,
		Currency:                in.Currency,
		CurrentPeriodStartsAt:   &now,
		CurrentPeriodEndsAt:     &periodEnd,
	})
	if err != nil {
		return nil, fmt.Errorf("recurring: create membership: %w", err)
	}

	row.SubscriptionID = sub.ID
	row.TenantID = in.TenantID.UUID()
	row.NextPullAt = periodEnd
	row.LastPulledPeriodStart = &now
	row.LastSignature = &sig
	row.Status = models.SolanaSubscriptionActive
	if err := s.repo.Upsert(ctx, row); err != nil {
		return nil, fmt.Errorf("recurring: persist solana subscription: %w", err)
	}
	return sub, nil
}
