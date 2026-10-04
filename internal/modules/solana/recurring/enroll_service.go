package recurring

import (
	"context"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
	submod "github.com/open-rails/openrails/internal/modules/subscriptions"
)

// membershipCreator is the lifecycle surface enroll drives (satisfied by
// *subscriptions.SubscriptionLifecycleService).
type membershipCreator interface {
	CreateMembership(ctx context.Context, params *submod.CreateMembershipParams) (*models.Subscription, error)
}

// subscriptionStore persists the on-chain state row (satisfied by
// *solanasubs.SolanaSubscriptionRepo).
type subscriptionStore interface {
	Upsert(ctx context.Context, s *models.SolanaSubscription) error
}

// EnrollService activates a recurring Solana subscription from its first
// payment: the atomic [init?, subscribe, transfer_subscription(first period)]
// bundle a checkout prepared and the merchant co-signed (#286). Activation
// rests only on that landed transaction, read from the chain by signature: it
// must be signed by the subscriber, subscribe to the checkout's plan terms,
// pull the full first period into the merchant's account, carry the checkout's
// reference and land within the checkout's validity. The payment is then
// claimed for that one checkout before anything is granted.
type EnrollService struct {
	lifecycle membershipCreator
	repo      subscriptionStore
	chain     landedTxReader
	db        *db.DB
	submitter Submitter // for MerchantAddress
	network   string
	tokens    map[string]config.TokenConfig
	now       func() time.Time
}

// NewEnrollService builds an EnrollService. database claims each first payment
// for its checkout.
func NewEnrollService(lifecycle membershipCreator, repo subscriptionStore, chain landedTxReader, database *db.DB, submitter Submitter, network string, tokens ...map[string]config.TokenConfig) *EnrollService {
	return &EnrollService{lifecycle: lifecycle, repo: repo, chain: chain, db: database, submitter: submitter, network: network, tokens: normalizeRecurringTokens(firstTokenMap(tokens)), now: time.Now}
}

// EnrollInput describes the checkout whose first payment activates a subscription.
type EnrollInput struct {
	MerchantID billing.MerchantID
	// CheckoutAttemptID is the checkout the first payment settles.
	CheckoutAttemptID uuid.UUID
	UserID            string
	CustomerEmail     string
	PriceID           uuid.UUID
	// SubscriberWallet is the wallet the checkout expects to sign; it becomes
	// the subscriber only once the landed payment proves it signed.
	SubscriberWallet string

	// Plan terms (from the price's Solana rail config).
	PlanID          uint64
	MintSymbol      string
	AmountBaseUnits uint64 // the full per-cycle pull; the first payment pulls exactly this
	PeriodHours     uint64
	PlanCreatedAt   int64 // ghost-plan fingerprint
	FiatAmount      int64 // price.Amount (micros) recorded on the payment
	Currency        string

	// Signature names the landed first-payment transaction.
	Signature string
	// Reference is the checkout's Solana Pay reference the prepared bundle carries.
	Reference string
	// ValidUntil is the checkout's validity; a later landing is refused.
	ValidUntil time.Time
}

// ConfirmEnrollment verifies the first payment on-chain, claims it for the
// checkout, then creates the membership and persists the
// billing.solana_subscriptions row. Idempotent for the same checkout and
// signature (the claim repeats and CreateMembership upserts on the rail
// subscription id).
func (s *EnrollService) ConfirmEnrollment(ctx context.Context, in EnrollInput) (*models.Subscription, error) {
	if in.UserID == "" || in.SubscriberWallet == "" || in.CheckoutAttemptID == uuid.Nil {
		return nil, fmt.Errorf("recurring: user, subscriber wallet and checkout are required")
	}
	if in.AmountBaseUnits == 0 || in.PeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid plan terms (amount/period)")
	}

	merchantKey, err := s.submitter.MerchantAddress(ctx, in.MerchantID)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve merchant: %w", err)
	}
	mintStr, err := ResolveRecurringMintFromTokens(in.MintSymbol, s.tokens)
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
	terms := subscription{
		subscriber: subscriber, merchant: merchantKey, mint: mint, planID: in.PlanID,
		amount: in.AmountBaseUnits, periodHours: in.PeriodHours, createdAt: in.PlanCreatedAt,
	}
	planPDA, subPDA, saPDA, err := terms.addresses()
	if err != nil {
		return nil, err
	}

	payment, err := fetchLanded(ctx, s.chain, in.Signature)
	if err != nil {
		return nil, err
	}
	if err := payment.references(in.Reference); err != nil {
		return nil, err
	}
	if err := payment.subscribes(terms); err != nil {
		return nil, err
	}
	if _, err := payment.pulls(terms, in.AmountBaseUnits); err != nil {
		return nil, err
	}
	landedAt := payment.landedAt()
	if solanaint.SettlementTooLate(landedAt, in.ValidUntil) {
		return nil, &LatePaymentError{LandedAt: *landedAt, ValidUntil: in.ValidUntil}
	}
	if err := settlement.ClaimCheckout(ctx, s.db, in.CheckoutAttemptID, payment.signature); err != nil {
		return nil, err
	}

	row := &models.SolanaSubscription{
		MerchantAddress:          merchantKey.String(),
		Mint:                     mint.String(),
		SubscriberWallet:         subscriber.String(),
		PlanPDA:                  planPDA.String(),
		SubscriptionPDA:          subPDA.String(),
		AuthorityPDA:             saPDA.String(),
		PlanCreatedAtFingerprint: in.PlanCreatedAt,
	}
	sig := payment.signature

	periodHoursI64, err := safecast.Convert[int64](in.PeriodHours)
	if err != nil {
		return nil, fmt.Errorf("recurring: enroll: period hours overflow: %w", err)
	}
	// The period starts when the first payment landed.
	now := s.now().UTC()
	if landedAt != nil {
		now = *landedAt
	}
	periodEnd := now.Add(time.Duration(periodHoursI64) * time.Hour)
	subPDAStr := subPDA.String()
	sub, err := s.lifecycle.CreateMembership(ctx, &submod.CreateMembershipParams{
		UserID:                in.UserID,
		PriceID:               in.PriceID,
		Rail:                  models.RailSolana,
		RailSubscriptionID:    &subPDAStr,
		CustomerEmail:         in.CustomerEmail,
		TransactionID:         sig,
		Amount:                in.FiatAmount,
		AmountProvided:        true,
		Currency:              in.Currency,
		CurrentPeriodStartsAt: &now,
		CurrentPeriodEndsAt:   &periodEnd,
		PaymentMetadata: map[string]any{
			"solana_subscriber_wallet": row.SubscriberWallet,
			"solana_subscription_pda":  row.SubscriptionPDA,
			"solana_token_symbol":      in.MintSymbol,
			"solana_token_mint":        row.Mint,
			"solana_token_amount":      in.AmountBaseUnits,
			"solana_recipient_wallet":  row.MerchantAddress,
		},
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
