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
	"github.com/open-rails/openrails/internal/integrations/fx"
	solanarpc "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/observability"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	redis "github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/metric"
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

var ErrSolanaSubscribePending = errors.New("solana: recurring subscribe pending subscribe step")

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
	Lifecycle   bool `json:"lifecycle,omitempty"`
	Subscribe   bool `json:"subscribe,omitempty"`
}

type SolanaPayService struct {
	db                 *db.DB
	redis              *redis.Client
	cfg                *config.Config
	clock              clockwork.Clock
	priceService       *catalog.PriceService
	productService     *catalog.ProductService
	eligibilityChecker purchaseEligibilityChecker
	fxProvider         fx.Provider
	priceProvider      TokenPriceProvider
	latency            metric.Float64Histogram
	errors             metric.Int64Counter
}

func NewSolanaPayService(
	db *db.DB,
	redis *redis.Client,
	cfg *config.Config,
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	eligibilityChecker purchaseEligibilityChecker,
	fxProvider fx.Provider,
	priceProvider TokenPriceProvider,
	meter *observability.Meter,
	clocks ...clockwork.Clock,
) *SolanaPayService {
	if meter == nil {
		meter = observability.NewNoopMeter()
	}
	return &SolanaPayService{
		db:                 db,
		redis:              redis,
		cfg:                cfg,
		priceService:       priceService,
		productService:     productService,
		eligibilityChecker: eligibilityChecker,
		fxProvider:         fxProvider,
		priceProvider:      priceProvider,
		clock:              timeutil.FirstClock(clocks...),
		latency:            meter.Latency,
		errors:             meter.ErrCounter,
	}
}

// SetMeter updates the meter after construction (called after InitTelemetry).
func (s *SolanaPayService) SetMeter(meter *observability.Meter) {
	if meter == nil {
		return
	}
	s.latency = meter.Latency
	s.errors = meter.ErrCounter
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
	start := time.Now()
	defer func() {
		s.latency.Record(ctx, time.Since(start).Seconds())
	}()

	if sessionID == nil || *sessionID == uuid.Nil {
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("checkout session id is required for solana payments")
	}
	tokenSymbol = strings.ToUpper(strings.TrimSpace(tokenSymbol))
	if tokenSymbol == "" {
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("token symbol is required")
	}

	if s.eligibilityChecker != nil {
		eligibility, err := s.eligibilityChecker.CheckPurchaseEligibility(ctx, userID, priceID)
		if err != nil {
			s.errors.Add(ctx, 1)
			return nil, fmt.Errorf("failed to check purchase eligibility: %w", err)
		}

		switch eligibility.Status {
		case eligibilityBlocked:
			return nil, fmt.Errorf("purchase blocked: %s", eligibility.Reason)
		case eligibilityUpgrade, eligibilityDowngrade:
			return nil, fmt.Errorf("solana does not support subscription tier changes; please cancel existing subscription first")
		case eligibilityAllowed:
			// Continue
		}
	}

	price, err := s.priceService.GetByID(ctx, priceID)
	if err != nil {
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsPurchasable() {
		return nil, fmt.Errorf("price is not active")
	}

	if s.productService != nil {
		product, err := s.productService.GetByID(ctx, price.ProductID)
		if err != nil {
			s.errors.Add(ctx, 1)
			return nil, fmt.Errorf("product not found: %w", err)
		}
		if !product.IsPurchasable() {
			return nil, fmt.Errorf("product is not active")
		}
	}

	solanaProc, err := RequireSolanaProcessorConfig(s.cfg)
	if err != nil {
		s.errors.Add(ctx, 1)
		return nil, err
	}
	tokenCfg, ok := solanaProc.Tokens[tokenSymbol]
	if !ok {
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("invalid or unsupported token: %s", tokenSymbol)
	}

	quote, err := CalculateTokenQuote(ctx, tokenSymbol, tokenCfg, price.Amount, price.Currency, s.fxProvider, s.priceProvider)
	if err != nil {
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("failed to calculate token quote: %w", err)
	}
	tokenUnits := quote.Units
	if tokenUnits == 0 {
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("calculated token amount is zero")
	}

	reference, err := solanarpc.GenerateReference()
	if err != nil {
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("failed to generate reference: %w", err)
	}

	recipient := solanaProc.RecipientWallet
	if recipient == "" {
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("merchant wallet not configured")
	}

	tokenMint := tokenCfg.Mint
	now := s.now()
	expiresAt := now.Add(pendingPaymentTTL)

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
		s.errors.Add(ctx, 1)
		return nil, fmt.Errorf("failed to store pending payment: %w", err)
	}

	url := s.buildTransferRequestURL(recipient, tokenUnits, tokenMint, tokenSymbol, reference)

	return &PayResult{
		URL:            url,
		Reference:      reference,
		Amount:         price.Amount,
		Currency:       price.Currency,
		TokenAmount:    formatTokenAmount(tokenUnits, tokenCfg.Decimals),
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

func (s *SolanaPayService) buildTransferRequestURL(recipient string, amount uint64, tokenMint, tokenSymbol, reference string) string {
	baseURL := fmt.Sprintf("solana:%s", recipient)
	solanaProc, err := RequireSolanaProcessorConfig(s.cfg)
	if err != nil {
		return baseURL
	}
	tokenCfg := solanaProc.Tokens[tokenSymbol]
	formattedAmount := formatTokenAmount(amount, tokenCfg.Decimals)
	params := fmt.Sprintf("?amount=%s", formattedAmount)
	if tokenMint != "" && tokenSymbol != "SOL" {
		params += fmt.Sprintf("&spl-token=%s", tokenMint)
	}
	params += fmt.Sprintf("&reference=%s", reference)
	label := "Purchase"
	if s.cfg != nil && s.cfg.Store != nil {
		if name := strings.TrimSpace(s.cfg.Store.Name); name != "" {
			label = name + " Purchase"
		}
	}
	params += fmt.Sprintf("&label=%s", url.QueryEscape(label))
	return baseURL + params
}

func (s *SolanaPayService) storePendingPayment(ctx context.Context, reference string, pending *PendingSolanaPayment) error {
	start := time.Now()
	defer func() {
		s.latency.Record(ctx, time.Since(start).Seconds())
	}()

	if s.redis == nil {
		return fmt.Errorf("redis not configured")
	}

	key := solanaPayKeyPrefix + reference
	data, err := json.Marshal(pending)
	if err != nil {
		s.errors.Add(ctx, 1)
		return fmt.Errorf("failed to marshal pending payment: %w", err)
	}

	if err := s.redis.Set(ctx, key, data, pendingPaymentTTL).Err(); err != nil {
		s.errors.Add(ctx, 1)
		return fmt.Errorf("failed to store pending payment: %w", err)
	}

	if err := s.redis.SAdd(ctx, pendingSolanaPaymentsKey, reference).Err(); err != nil {
		s.redis.Del(ctx, key)
		s.errors.Add(ctx, 1)
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

func (s *SolanaPayService) GetPendingPayment(ctx context.Context, reference string) (*PendingSolanaPayment, error) {
	if s.redis == nil {
		return nil, fmt.Errorf("redis not configured")
	}
	key := solanaPayKeyPrefix + reference
	data, err := s.redis.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
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

func (s *SolanaPayService) GetAllPendingReferences(ctx context.Context) ([]string, error) {
	if s.redis == nil {
		return nil, nil
	}
	refs, err := s.redis.SMembers(ctx, pendingSolanaPaymentsKey).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get pending references: %w", err)
	}
	return refs, nil
}

func (s *SolanaPayService) RegisterPendingReference(ctx context.Context, reference string) error {
	if s.redis == nil {
		return nil
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil
	}
	if err := s.redis.SAdd(ctx, pendingSolanaPaymentsKey, reference).Err(); err != nil {
		return fmt.Errorf("failed to add reference to pending set: %w", err)
	}
	return nil
}

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
	if err := s.redis.SRem(ctx, pendingSolanaPaymentsKey, reference).Err(); err != nil {
		removeErr = fmt.Errorf("failed to remove from pending set: %w", err)
	}
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
	references := strings.TrimSpace(reference)
	if reference == "" {
		return nil
	}
	if _, err := s.MarkReferenceConsumed(ctx, references, transactionID); err != nil {
		return err
	}
	if err := s.RemovePendingPayment(ctx, references); err != nil {
		return err
	}
	return nil
}

// GetPaymentStatus checks if a payment is pending, confirmed, or expired
func (s *SolanaPayService) GetPaymentStatus(ctx context.Context, reference string) (status string, payment *models.Payment, err error) {
	// First check Postgres for confirmed payment
	payment, err = s.getPaymentByReference(ctx, reference)
	if err == nil && payment != nil {
		return "confirmed", payment, nil
	}

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
