package recurring

import (
	"context"
	"fmt"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	solanago "github.com/gagliardetto/solana-go"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
)

// EnrollService verifies a recurring Solana subscription's first payment:
// the atomic [init?, subscribe, transfer_subscription] transaction an order's
// wallet request built and the merchant co-signed. Activation rests only on
// that landed transaction: subscriber-signed, subscribing to the order's
// terms, pulling the full first period to the merchant, carrying the
// attempt's reference and landing within its validity. The caller claims the
// payment for its attempt and opens the membership in one transaction.
type EnrollService struct {
	chain     landedTxReader
	submitter Submitter // for MerchantAddress
	tokens    Tokens
	now       func() time.Time
}

// NewEnrollService builds an EnrollService.
func NewEnrollService(chain landedTxReader, submitter Submitter, tokens Tokens) *EnrollService {
	return &EnrollService{chain: chain, submitter: submitter, tokens: tokens, now: time.Now}
}

// EnrollInput is the attempt whose first payment activates a subscription.
type EnrollInput struct {
	MerchantID billing.MerchantID
	// SubscriberWallet is the wallet the attempt expects to sign; it becomes
	// the subscriber only once the landed payment proves it signed.
	SubscriberWallet string

	// Plan terms (from the price's Solana rail config).
	PlanID          uint64
	MintSymbol      string
	AmountBaseUnits uint64 // the full per-cycle pull; the first payment pulls exactly this
	PeriodHours     uint64
	PlanCreatedAt   int64 // ghost-plan fingerprint

	// Signature names the landed first-payment transaction.
	Signature string
	// Reference is the attempt's Solana Pay reference the prepared transaction carries.
	Reference string
	// ValidUntil is the attempt's validity; a later landing is refused.
	ValidUntil time.Time
}

// Enrollment is a verified first payment: the program subscription it opened
// and its first period.
type Enrollment struct {
	Signature              string
	PeriodStart, PeriodEnd time.Time
	// Subscription is the program's state; SubscriptionID is set by the
	// membership the payment opens.
	Subscription models.SolanaSubscription
}

// Verify proves the landed first payment is the attempt's: it references the
// attempt, subscribes to its terms and pulls the full first period to the
// merchant, in time.
func (s *EnrollService) Verify(ctx context.Context, in EnrollInput) (*Enrollment, error) {
	if in.SubscriberWallet == "" || in.Reference == "" {
		return nil, fmt.Errorf("recurring: subscriber wallet and reference are required")
	}
	if in.AmountBaseUnits == 0 || in.PeriodHours == 0 {
		return nil, fmt.Errorf("recurring: invalid plan terms (amount/period)")
	}
	merchantKey, err := s.submitter.MerchantAddress(ctx, in.MerchantID)
	if err != nil {
		return nil, fmt.Errorf("recurring: resolve merchant: %w", err)
	}
	mintStr, err := ResolveRecurringMintFromTokens(in.MintSymbol, s.tokens.of(ctx))
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
	periodHours, err := safecast.Convert[int64](in.PeriodHours)
	if err != nil {
		return nil, fmt.Errorf("recurring: enroll: period hours overflow: %w", err)
	}
	// The period starts when the first payment landed.
	start := s.now().UTC()
	if landedAt != nil {
		start = landedAt.UTC()
	}
	start = start.Truncate(time.Microsecond)
	end := start.Add(time.Duration(periodHours) * time.Hour)
	sig := payment.signature
	return &Enrollment{Signature: sig, PeriodStart: start, PeriodEnd: end, Subscription: models.SolanaSubscription{
		MerchantID:               in.MerchantID.UUID(),
		MerchantAddress:          merchantKey.String(),
		Mint:                     mint.String(),
		SubscriberWallet:         subscriber.String(),
		PlanPDA:                  planPDA.String(),
		SubscriptionPDA:          subPDA.String(),
		AuthorityPDA:             saPDA.String(),
		PlanCreatedAtFingerprint: in.PlanCreatedAt,
		NextPullAt:               end,
		LastPulledPeriodStartsAt: &start,
		LastSignature:            &sig,
		Status:                   models.SolanaSubscriptionActive,
	}}, nil
}
