package solana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/fx"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/merchant"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

const (
	// Redis keys
	pendingSolanaPaymentsKey = "pending_solana_payments"
	solanaPayKeyPrefix       = "solana_pay:"
	solanaPayConsumedPrefix  = "solana_pay_consumed:"

	// TTL for pending payments
	pendingPaymentTTL = 15 * time.Minute
	consumedRefTTL    = 24 * time.Hour
)

type purchaseEligibilityChecker interface {
	CheckPurchaseEligibility(ctx context.Context, userID string, priceID uuid.UUID) (*PurchaseEligibilityResult, error)
}

type PurchaseEligibilityResult struct {
	Status string
	Reason string
}

const (
	eligibilityAllowed   = "allowed"
	eligibilityBlocked   = "blocked"
	eligibilityUpgrade   = "upgrade"
	eligibilityDowngrade = "downgrade"
)

// ErrSolanaSubscribePending is returned by ConfirmSolanaSubscribeSession (and
// surfaced through the poller) when a RECURRING subscribe Solana Pay session has
// only its init tx landed so far — the authority now exists but the atomic
// [subscribe+transfer] bundle has not landed yet. The poller treats it as
// "keep polling": it does NOT consume the reference (the subscribe tx carries the
// SAME reference) and re-checks until the subscription PDA is funded. It is a
// normal in-progress state, not a failure.
var ErrSolanaSubscribePending = errors.New("solana: recurring subscribe pending subscribe step")

// PendingSolanaPayment represents a pending Solana payment stored in Redis
type PendingSolanaPayment struct {
	UserID      string    `json:"user_id"`
	PriceID     string    `json:"price_id"`
	SessionID   string    `json:"session_id,omitempty"`
	Amount      int64     `json:"amount"`   // cents (fiat equivalent)
	Currency    string    `json:"currency"` // e.g., "usd"
	Token       string    `json:"token"`    // e.g., "USDC"
	TokenMint   string    `json:"token_mint"`
	TokenAmount uint64    `json:"token_amount"` // token base units
	Recipient   string    `json:"recipient"`    // merchant wallet
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at,omitempty"`

	// Lifecycle marks a checkout-session-backed pending record whose confirmation
	// mirrors an on-chain CANCEL or TIER-CHANGE (checkout modes solana_cancel /
	// solana_tier_change) rather than registering a purchase. When set, the poller
	// skips token/amount verification (the random reference already identifies the
	// tx) and routes the confirmed signature to ConfirmSolanaLifecycleSession.
	Lifecycle bool `json:"lifecycle,omitempty"`

	// Subscribe marks a checkout-session-backed pending record for a RECURRING
	// subscribe over Solana Pay (mode=subscription, flow=transaction_request). Like
	// Lifecycle, the poller skips token verification (the random reference already
	// identifies the tx) and routes the confirmed signature to
	// ConfirmSolanaSubscribeSession, which advances the init→subscribe step and, once
	// the atomic [subscribe+transfer] bundle has landed, enrolls the membership. The
	// init tx and the subscribe tx share this same reference, so confirmation is
	// step-aware + idempotent.
	Subscribe bool `json:"subscribe,omitempty"`
}

// SolanaPayService handles Solana Pay Transfer Request flow
type SolanaPayService struct {
	db                 *db.DB
	redis              *redis.Client
	cfg                *config.Config
	rails              railresolve.Source
	clock              clockwork.Clock
	priceService       *catalog.PriceService
	productService     *catalog.ProductService
	eligibilityChecker purchaseEligibilityChecker
	fxProvider         fx.Provider
	priceProvider      TokenPriceProvider
	// mints reads SPL mint decimals from the chain (#817). Late-bound like the
	// poller's merchant RPC: it needs the per-merchant RPC resolver, which is
	// armed after service construction. nil = quotes fail closed.
	mints MintDecimalsSource
}

// NewSolanaPayService creates a new SolanaPayService
func NewSolanaPayService(
	db *db.DB,
	redis *redis.Client,
	cfg *config.Config,
	railSet railresolve.Source,
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	eligibilityChecker purchaseEligibilityChecker,
	fxProvider fx.Provider,
	priceProvider TokenPriceProvider,
	clocks ...clockwork.Clock,
) *SolanaPayService {
	return &SolanaPayService{
		db:                 db,
		redis:              redis,
		cfg:                cfg,
		rails:              railSet,
		priceService:       priceService,
		productService:     productService,
		eligibilityChecker: eligibilityChecker,
		fxProvider:         fxProvider,
		priceProvider:      priceProvider,
		clock:              timeutil.FirstClock(clocks...),
	}
}

// SetMintDecimals arms the on-chain mint-decimals resolver (#817).
func (s *SolanaPayService) SetMintDecimals(mints MintDecimalsSource) {
	s.mints = mints
}

func (s *SolanaPayService) SetEligibilityChecker(checker purchaseEligibilityChecker) {
	s.eligibilityChecker = checker
}

func (s *SolanaPayService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *SolanaPayService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

func (s *SolanaPayService) Clock() clockwork.Clock {
	return s.clock
}

// GeneratePayment creates a new pending Solana payment and returns the Transfer Request URL.
// It first checks purchase eligibility to prevent duplicate purchases.
func (s *SolanaPayService) GeneratePayment(ctx context.Context, userID string, priceID uuid.UUID, tokenSymbol string, sessionID *uuid.UUID) (*PayResult, error) {
	if sessionID == nil || *sessionID == uuid.Nil {
		return nil, fmt.Errorf("checkout session id is required for solana payments")
	}
	tokenSymbol = strings.ToUpper(strings.TrimSpace(tokenSymbol))
	if tokenSymbol == "" {
		return nil, fmt.Errorf("token symbol is required")
	}

	// Check purchase eligibility BEFORE generating the payment URL
	if s.eligibilityChecker != nil {
		eligibility, err := s.eligibilityChecker.CheckPurchaseEligibility(ctx, userID, priceID)
		if err != nil {
			return nil, fmt.Errorf("failed to check purchase eligibility: %w", err)
		}

		switch eligibility.Status {
		case eligibilityBlocked:
			return nil, fmt.Errorf("purchase blocked: %s", eligibility.Reason)
		case eligibilityUpgrade, eligibilityDowngrade:
			// Solana doesn't support subscription upgrades/downgrades
			return nil, fmt.Errorf("solana does not support subscription tier changes; please cancel existing subscription first")
		case eligibilityAllowed:
			// Continue with payment generation
		}
	}

	// Validate price
	price, err := s.priceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsPurchasable() {
		return nil, fmt.Errorf("price is not active")
	}

	// Validate product
	if s.productService != nil {
		product, err := s.productService.GetByID(ctx, price.ProductID)
		if err != nil {
			return nil, fmt.Errorf("product not found: %w", err)
		}
		if !product.IsPurchasable() {
			return nil, fmt.Errorf("product is not active")
		}
	}

	// Validate Solana config
	solanaProc, err := RequireSolanaRailConfig(ctx, s.rails)
	if err != nil {
		return nil, err
	}
	if solanaProc.Solana == nil {
		return nil, fmt.Errorf("solana rail is not configured")
	}
	tokenCfg, ok := solanaProc.Solana.Tokens[tokenSymbol]
	if !ok {
		return nil, fmt.Errorf("invalid or unsupported token: %s", tokenSymbol)
	}

	// Decimals come from the MINT on-chain, never from config (#817).
	decimals, err := RequireMintDecimals(ctx, s.mints, tokenCfg.Mint)
	if err != nil {
		return nil, err
	}

	// Calculate token amount from fiat price with FX conversion if needed
	quote, err := CalculateTokenQuote(ctx, tokenSymbol, tokenCfg.Mint, decimals, moneyutil.Micros(price.Amount), price.Currency, s.fxProvider, s.priceProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate token quote: %w", err)
	}
	tokenUnits := quote.Units
	if tokenUnits == 0 {
		return nil, fmt.Errorf("calculated token amount is zero")
	}

	// Generate reference for Solana Pay
	reference, err := solanarpc.GenerateReference()
	if err != nil {
		return nil, fmt.Errorf("failed to generate reference: %w", err)
	}

	recipient, err := ResolveRecipientWallet(ctx, s.db, s.cfg)
	if err != nil {
		return nil, err
	}

	// Get token mint
	tokenMint := tokenCfg.Mint

	now := s.now()
	expiresAt := now.Add(pendingPaymentTTL)

	// Store pending payment in Redis
	pending := &PendingSolanaPayment{
		UserID:      userID,
		PriceID:     priceID.String(),
		Amount:      price.Amount,
		Currency:    price.Currency,
		Token:       tokenSymbol,
		TokenMint:   tokenMint,
		TokenAmount: tokenUnits,
		Recipient:   recipient,
		CreatedAt:   now,
		ExpiresAt:   expiresAt,
	}
	if sessionID != nil && *sessionID != uuid.Nil {
		pending.SessionID = sessionID.String()
	}

	if err := s.storePendingPayment(ctx, reference, pending); err != nil {
		return nil, fmt.Errorf("failed to store pending payment: %w", err)
	}

	// Build Solana Pay Transfer Request URL. The memo field (#713) stamps the
	// checkout session id on the wallet-built tx: per the Solana Pay spec the
	// wallet includes it as an SPL Memo instruction BEFORE the transfer.
	// Discovery hint, never money truth.
	url := s.buildTransferRequestURL(ctx, recipient, tokenUnits, decimals, tokenMint, tokenSymbol, reference, solanarpc.PurchaseMemo(*sessionID))

	return &PayResult{
		URL:            url,
		Reference:      reference,
		Amount:         price.Amount,
		Currency:       price.Currency,
		TokenAmount:    FormatBaseUnits(tokenUnits, decimals),
		TokenUnits:     tokenUnits,
		TokenMint:      tokenMint,
		Recipient:      recipient,
		TokenPriceUSD:  quote.TokenPriceUSD,
		FXRate:         quote.FXRate,
		FXCurrency:     quote.FXCurrency,
		QuotedAt:       quote.QuotedAt,
		QuoteExpiresAt: expiresAt,
		Token:          tokenSymbol,
		ExpiresAt:      expiresAt,
	}, nil
}

// buildTransferRequestURL constructs the solana: URL per the Solana Pay spec.
// `decimals` is the caller's already-resolved ON-CHAIN mint precision (#817) —
// re-reading it here from a map without an ok-check turned an unknown symbol
// into decimals=0, i.e. the raw base-unit count on the wire (a 10^d overcharge).
func (s *SolanaPayService) buildTransferRequestURL(ctx context.Context, recipient string, amount uint64, decimals int, tokenMint, tokenSymbol, reference, memo string) string {
	// Base URL: solana:<recipient>
	baseURL := fmt.Sprintf("solana:%s", recipient)

	// Add query params
	params := fmt.Sprintf("?amount=%s", FormatBaseUnits(amount, decimals))

	// Add spl-token param if not native SOL
	if tokenMint != "" && tokenSymbol != "SOL" {
		params += fmt.Sprintf("&spl-token=%s", tokenMint)
	}

	// Add reference for payment detection
	params += fmt.Sprintf("&reference=%s", reference)

	// #713 self-recognition memo (SPL Memo instruction, placed by the wallet
	// before the transfer per spec).
	if memo != "" {
		params += fmt.Sprintf("&memo=%s", url.QueryEscape(memo))
	}

	// Add label
	label := "Purchase"
	if s.db != nil {
		if cfg, _, err := merchantconfig.NewStore(s.db).Get(ctx); err == nil {
			if name := strings.TrimSpace(cfg.Profile.DisplayName); name != "" {
				label = name + " Purchase"
			}
		}
	}
	params += fmt.Sprintf("&label=%s", url.QueryEscape(label))

	return baseURL + params
}

// pendingReferenceMember encodes a pending-set member as "<merchant_id>|<ref>"
// (#728): the poller fans out per merchant, so every pending reference carries
// its merchant. References are base58, so '|' never collides.
func pendingReferenceMember(mid merchant.ID, reference string) string {
	return mid.String() + "|" + reference
}

// parsePendingReferenceMember splits a set member back into merchant + ref.
// or#893: `<merchant_id>|<reference>` is the ONLY shape. A member that does not
// parse is not history — the bare pre-#728 form has long since aged out of any
// live set through its own TTL — it is corruption of a live poller input, and
// the poller must say so rather than quietly discarding somebody's payment
// reference.
func parsePendingReferenceMember(member string) (merchant.ID, string, error) {
	midStr, ref, cut := strings.Cut(member, "|")
	if !cut {
		return merchant.ID{}, "", fmt.Errorf("malformed pending member %q: expected <merchant_id>|<reference>", member)
	}
	id, err := uuid.Parse(midStr)
	if err != nil || id == uuid.Nil {
		return merchant.ID{}, "", fmt.Errorf("malformed pending member %q: %q is not a merchant id", member, midStr)
	}
	if strings.TrimSpace(ref) == "" {
		return merchant.ID{}, "", fmt.Errorf("malformed pending member %q: empty reference", member)
	}
	return merchant.ID(id), strings.TrimSpace(ref), nil
}

// storePendingPayment stores a pending payment in Redis
func (s *SolanaPayService) storePendingPayment(ctx context.Context, reference string, pending *PendingSolanaPayment) error {
	if s.redis == nil {
		return fmt.Errorf("redis not configured")
	}
	// #728: the pending set is merchant-attributed; a payment without a resolved
	// merchant cannot be polled.
	mid, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("solana pending payment requires a merchant: %w", err)
	}

	key := solanaPayKeyPrefix + reference
	data, err := json.Marshal(pending)
	if err != nil {
		return fmt.Errorf("failed to marshal pending payment: %w", err)
	}

	// Store the payment data with TTL
	if err := s.redis.Set(ctx, key, data, pendingPaymentTTL).Err(); err != nil {
		return fmt.Errorf("failed to store pending payment: %w", err)
	}

	// Add to the pending payments set
	if err := s.redis.SAdd(ctx, pendingSolanaPaymentsKey, pendingReferenceMember(mid, reference)).Err(); err != nil {
		// Try to cleanup the key we just set
		s.redis.Del(ctx, key)
		return fmt.Errorf("failed to add to pending set: %w", err)
	}

	log.WithFields(log.Fields{
		"reference": reference,
		"user_id":   pending.UserID,
		"amount":    pending.Amount,
		"token":     pending.Token,
	}).Info("Stored pending Solana payment")

	return nil
}

// GetPendingPayment retrieves a pending payment by reference
func (s *SolanaPayService) GetPendingPayment(ctx context.Context, reference string) (*PendingSolanaPayment, error) {
	if s.redis == nil {
		return nil, fmt.Errorf("redis not configured")
	}

	key := solanaPayKeyPrefix + reference
	data, err := s.redis.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil // Not found (expired)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get pending payment: %w", err)
	}

	var pending PendingSolanaPayment
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pending payment: %w", err)
	}

	return &pending, nil
}

// PendingReferencesByMerchant returns the pending payment references grouped
// by merchant (#728) — the poller's per-merchant fan-out input.
//
// or#893: a member that does not parse FAILS the pass. Every writer
// (storePendingPayment, RegisterPendingReference) requires a merchant and emits
// the canonical form, so an unparseable member means the set was written by
// something else — and silently SREMing it, as the pre-#728 compatibility lane
// did, deletes a buyer's pending payment reference and with it the poller's
// only chance to credit a payment that may already be on chain.
func (s *SolanaPayService) PendingReferencesByMerchant(ctx context.Context) (map[merchant.ID][]string, error) {
	if s.redis == nil {
		return nil, nil
	}

	members, err := s.redis.SMembers(ctx, pendingSolanaPaymentsKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get pending references: %w", err)
	}
	out := make(map[merchant.ID][]string, len(members))
	for _, member := range members {
		mid, ref, err := parsePendingReferenceMember(member)
		if err != nil {
			return nil, fmt.Errorf("solana pending set is corrupt: %w", err)
		}
		out[mid] = append(out[mid], ref)
	}
	return out, nil
}

// RegisterPendingReference adds an already-bound checkout-session reference to the
// poller's pending set so the reference poller actually iterates it. The one-off
// transfer-request flow seeds its reference via GeneratePayment; the
// transaction-request lifecycle (cancel/tier-change) + recurring-subscribe flows
// instead bind the reference to the DB session and call this — the poller then
// recovers the session via GetByReference. Idempotent (SAdd on a set).
func (s *SolanaPayService) RegisterPendingReference(ctx context.Context, reference string) error {
	if s.redis == nil {
		return nil
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil
	}
	// #728: merchant-attributed member so the poller polls it in the right scope.
	mid, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("solana pending reference requires a merchant: %w", err)
	}
	if err := s.redis.SAdd(ctx, pendingSolanaPaymentsKey, pendingReferenceMember(mid, reference)).Err(); err != nil {
		return fmt.Errorf("failed to add reference to pending set: %w", err)
	}
	return nil
}

// RemovePendingPayment removes a pending payment from Redis
func (s *SolanaPayService) RemovePendingPayment(ctx context.Context, reference string) error {
	if s.redis == nil {
		return nil
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil
	}

	key := solanaPayKeyPrefix + reference
	var removeErr error

	// Remove from set. or#893: only the canonical merchant-attributed member —
	// the bare form is not a shape this set can hold, and SREMing a bare
	// `<reference>` on a set that only ever holds `<merchant>|<reference>` was
	// always a no-op dressed as cleanup. A removal with no merchant on ctx
	// cannot name the member and must say so.
	mid, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("solana pending reference removal requires a merchant: %w", err)
	}
	if err := s.redis.SRem(ctx, pendingSolanaPaymentsKey, pendingReferenceMember(mid, reference)).Err(); err != nil {
		removeErr = fmt.Errorf("failed to remove from pending set: %w", err)
	}

	// Delete the key
	if err := s.redis.Del(ctx, key).Err(); err != nil {
		if removeErr != nil {
			removeErr = fmt.Errorf("%v; failed to delete pending payment key: %w", removeErr, err)
		} else {
			removeErr = fmt.Errorf("failed to delete pending payment key: %w", err)
		}
	}

	if removeErr != nil {
		log.WithError(removeErr).WithField("reference", reference).Warn("Failed to remove pending Solana payment")
		return removeErr
	}

	return nil
}

func (s *SolanaPayService) IsReferenceConsumed(ctx context.Context, reference string) (bool, error) {
	if s.redis == nil {
		return false, nil
	}

	reference = strings.TrimSpace(reference)
	if reference == "" {
		return false, nil
	}

	count, err := s.redis.Exists(ctx, solanaPayConsumedPrefix+reference).Result()
	if err != nil {
		return false, fmt.Errorf("failed checking consumed reference: %w", err)
	}

	return count > 0, nil
}

func (s *SolanaPayService) MarkReferenceConsumed(ctx context.Context, reference, transactionID string) (bool, error) {
	if s.redis == nil {
		return false, fmt.Errorf("redis not configured")
	}

	reference = strings.TrimSpace(reference)
	if reference == "" {
		return false, fmt.Errorf("reference is required")
	}

	value := strings.TrimSpace(transactionID)
	if value == "" {
		value = "consumed"
	}

	claimed, err := s.redis.SetNX(ctx, solanaPayConsumedPrefix+reference, value, consumedRefTTL).Result()
	if err != nil {
		return false, fmt.Errorf("failed to mark reference consumed: %w", err)
	}

	return claimed, nil
}

func (s *SolanaPayService) ConsumeAndRemovePending(ctx context.Context, reference, transactionID string) error {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil
	}

	if _, err := s.MarkReferenceConsumed(ctx, reference, transactionID); err != nil {
		return err
	}

	if err := s.RemovePendingPayment(ctx, reference); err != nil {
		return err
	}

	return nil
}

// GetPaymentStatus checks if a payment is pending, confirmed, or expired
func (s *SolanaPayService) GetPaymentStatus(ctx context.Context, reference string) (status string, payment *models.Payment, err error) {
	// or#869 (staticcheck SA4023): there used to be a "first check Postgres for
	// a confirmed payment" step here, guarded by `err == nil && payment != nil`.
	// It could never be taken — its helper, getPaymentByReference, was a stub
	// that unconditionally returned an error, because a Solana reference is
	// EPHEMERAL: it exists only for on-chain matching during checkout, and a
	// confirmed payment is identified by its transaction signature instead.
	// So there is no confirmed-payment lookup by reference to do, and pretending
	// to try one made this function read as if there were. Redis is the only
	// answer this reference can have.

	// Check Redis for pending payment
	pending, err := s.GetPendingPayment(ctx, reference)
	if err != nil {
		return "", nil, fmt.Errorf("failed to check pending payment: %w", err)
	}

	if pending == nil {
		return "expired", nil, nil
	}

	return "pending", nil, nil
}

// getPaymentByReference looks up a payment by its Solana reference.
// Note: Reference-based lookup is not currently supported since payments
// are identified by their transaction signature (stored in Payment.TransactionID).
// The reference is only used during the checkout flow for on-chain matching.
func (s *SolanaPayService) getPaymentByReference(ctx context.Context, reference string) (*models.Payment, error) {
	// References are ephemeral and used only during checkout flow for on-chain matching.
	// Once a payment is confirmed, it's identified by its transaction signature.
	// Return not found - callers should check Redis for pending status.
	return nil, fmt.Errorf("payment not found for reference")
}
