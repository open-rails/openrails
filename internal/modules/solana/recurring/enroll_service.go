package recurring

import (
	"context"
	"fmt"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/integrations/solana/subscriptions"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

// membershipCreator is the lifecycle surface enroll drives (satisfied by
// *subscriptions.SubscriptionLifecycleService).
type membershipCreator interface {
	CreateMembership(ctx context.Context, params *submod.CreateMembershipParams) (*models.Subscription, error)
}

// accountReader is the minimal RPC surface used to confirm the on-chain
// subscription account exists (satisfied by *solana.RPCClient). GetAccountData
// reads at CONFIRMED commitment and returns (nil, nil) when the account is
// absent — so we settle as soon as the subscribe tx is confirmed rather than
// waiting for finalization (which raced the HTTP confirm window and 500'd).
type accountReader interface {
	GetAccountData(ctx context.Context, address solanago.PublicKey) ([]byte, error)
}

// subscriptionStore persists the on-chain state row (satisfied by
// *dbrepo.SolanaSubscriptionRepo).
type subscriptionStore interface {
	Upsert(ctx context.Context, s *models.SolanaSubscription) error
}

// EnrollService activates a recurring Solana subscription after the user has
// signed the ATOMIC subscribe bundle in their wallet (#255, #286). As of #286
// the first pull happens INSIDE the atomic subscribe tx ([subscribe +
// transfer_subscription(first period)]), so there is NO separate first crank
// here: the service confirms the on-chain subscription PDA exists — which (the
// bundle being atomic: both land or both revert) proves the first period was
// pulled — then creates the lifecycle membership and persists the on-chain row.
type EnrollService struct {
	lifecycle membershipCreator
	repo      subscriptionStore
	rpc       accountReader
	submitter Submitter // for MerchantAddress
	network   string
	tokens    map[string]config.TokenConfig
	now       func() time.Time
}

// NewEnrollService builds an EnrollService. The cranker is no longer a dependency
// of confirm (#286): the first pull is bundled into the atomic subscribe tx, so
// confirm only verifies + creates the membership. Recurring rebills still use
// CrankService elsewhere.
func NewEnrollService(lifecycle membershipCreator, repo subscriptionStore, rpc accountReader, submitter Submitter, network string, tokens ...map[string]config.TokenConfig) *EnrollService {
	return &EnrollService{lifecycle: lifecycle, repo: repo, rpc: rpc, submitter: submitter, network: network, tokens: normalizeRecurringTokens(firstTokenMap(tokens)), now: time.Now}
}

// EnrollInput describes a confirmed wallet enrollment to activate.
type EnrollInput struct {
	MerchantID       merchant.ID
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

	// Signature, when set, is the confirmed atomic subscribe-bundle signature the
	// wallet submitted (#286). It is recorded as the membership TransactionID + the
	// row's LastSignature. Empty => the on-chain subscription PDA is used as a
	// stable fallback identifier (the bundle is what proves the first pull).
	Signature string
}

// ConfirmEnrollment derives the on-chain PDAs, verifies the subscription PDA
// exists on-chain — which, because the subscribe bundle is ATOMIC ([subscribe +
// transfer(first period)] both land or both revert), proves the first period was
// already pulled in that same tx (#286) — then creates the membership and
// persists the openrails.solana_subscriptions row. There is NO separate first
// crank: the first pull happened inside the atomic subscribe tx. Idempotent (the
// lifecycle CreateMembership upserts on the processor subscription id).
func (s *EnrollService) ConfirmEnrollment(ctx context.Context, in EnrollInput) (*models.Subscription, error) {
	if in.UserID == "" || in.SubscriberWallet == "" {
		return nil, fmt.Errorf("recurring: user id and subscriber wallet are required")
	}
	if in.AmountBaseUnits == 0 || in.PeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid plan terms (amount/period)")
	}

	merchant, err := s.submitter.MerchantAddress(ctx, in.MerchantID)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve merchant: %w", err)
	}
	mintStr, _, err := ResolveRecurringMintFromTokens(in.MintSymbol, s.tokens)
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

	// Confirm the atomic subscribe bundle landed by checking the subscription PDA
	// exists on-chain. The bundle is atomic ([subscribe + transfer(first period)]),
	// so a funded PDA means BOTH instructions succeeded — i.e. the first period was
	// already pulled in that same tx. There is no separate crank to run here (#286).
	//
	// READ-AFTER-CONFIRM LAG: the wallet signed + sent the bundle and confirmed it
	// client-side, so the server never saw that tx's slot to gate this read on. A
	// single read right after can hit a lagging RPC node that does not yet see the
	// just-created PDA, spuriously rejecting a valid enrollment. Poll until the PDA
	// account exists (eventual consistency). GetAccountData reads at CONFIRMED and
	// returns (nil,nil) when absent, so we settle once the subscribe tx is confirmed
	// rather than waiting for finalization (the lag that 500'd the HTTP confirm).
	data, berr := solanaint.ReadUntilConsistent(ctx, solanaint.ReadUntilConsistentOpts{},
		func(ctx context.Context) ([]byte, error) { return s.rpc.GetAccountData(ctx, subPDA) },
		func(d []byte) bool { return len(d) > 0 },
	)
	if berr != nil {
		return nil, fmt.Errorf("recurring: subscription %s not found on-chain — did the wallet run the atomic subscribe? (%w)", subPDA, berr)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("recurring: subscription %s not found on-chain — did the wallet run the atomic subscribe?", subPDA)
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

	// The first pull already happened inside the atomic subscribe tx; record that
	// tx's signature (when the caller supplied it) as the membership/row reference,
	// falling back to the on-chain subscription PDA as a stable identifier.
	sig := strings.TrimSpace(in.Signature)
	if sig == "" {
		sig = subPDA.String()
	}

	periodHoursI64, err := safecast.Convert[int64](in.PeriodHours)
	if err != nil {
		return nil, fmt.Errorf("recurring: enroll: period hours overflow: %w", err)
	}
	now := s.now().UTC()
	periodEnd := now.Add(time.Duration(periodHoursI64) * time.Hour)
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
	row.MerchantID = in.MerchantID.UUID()
	row.NextPullAt = periodEnd
	row.LastPulledPeriodStart = &now
	row.LastSignature = &sig
	row.Status = models.SolanaSubscriptionActive
	if err := s.repo.Upsert(ctx, row); err != nil {
		return nil, fmt.Errorf("recurring: persist solana subscription: %w", err)
	}
	return sub, nil
}
