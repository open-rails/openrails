package services

import (
	"context"
	"encoding/json"
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
	solana "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/modules/catalog"
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
}

// SolanaPayResult is returned when creating a new Solana Pay URL
type SolanaPayResult struct {
	URL            string
	Reference      string
	Amount         int64 // cents
	Currency       string
	TokenAmount    string // formatted token amount (e.g., "9.99")
	TokenUnits     uint64 // token amount in base units
	TokenMint      string // token mint used for this quote/payment
	Recipient      string // merchant wallet for this quote/payment
	TokenPriceUSD  float64
	FXRate         float64
	FXCurrency     string
	QuotedAt       time.Time
	QuoteExpiresAt time.Time
	Token          string
	ExpiresAt      time.Time
}

// SolanaPayService handles Solana Pay Transfer Request flow
type SolanaPayService struct {
	db              *db.DB
	redis           *redis.Client
	cfg             *config.Config
	Clock           clockwork.Clock
	priceService    *catalog.PriceService
	productService  *catalog.ProductService
	checkoutService *CheckoutService
	fxProvider      fx.Provider
}

// NewSolanaPayService creates a new SolanaPayService
func NewSolanaPayService(
	db *db.DB,
	redis *redis.Client,
	cfg *config.Config,
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	checkoutService *CheckoutService,
	fxProvider fx.Provider,
) *SolanaPayService {
	return &SolanaPayService{
		db:              db,
		redis:           redis,
		cfg:             cfg,
		priceService:    priceService,
		productService:  productService,
		checkoutService: checkoutService,
		fxProvider:      fxProvider,
	}
}

func (s *SolanaPayService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// SetCheckoutService sets the checkout service (used to break circular dependency during init)
func (s *SolanaPayService) SetCheckoutService(cs *CheckoutService) {
	s.checkoutService = cs
}

// GeneratePayment creates a new pending Solana payment and returns the Transfer Request URL.
// It first checks purchase eligibility to prevent duplicate purchases.
func (s *SolanaPayService) GeneratePayment(ctx context.Context, userID string, priceID uuid.UUID, tokenSymbol string, sessionID *uuid.UUID) (*SolanaPayResult, error) {
	// Check purchase eligibility BEFORE generating the payment URL
	if s.checkoutService != nil {
		eligibility, err := s.checkoutService.CheckPurchaseEligibility(ctx, userID, priceID)
		if err != nil {
			return nil, fmt.Errorf("failed to check purchase eligibility: %w", err)
		}

		switch eligibility.Status {
		case EligibilityBlocked:
			return nil, fmt.Errorf("purchase blocked: %s", eligibility.Reason)
		case EligibilityUpgrade, EligibilityDowngrade:
			// Solana doesn't support subscription upgrades/downgrades
			return nil, fmt.Errorf("solana does not support subscription tier changes; please cancel existing subscription first")
		case EligibilityAllowed:
			// Continue with payment generation
		}
	}

	// Validate price
	price, err := s.priceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsActive {
		return nil, fmt.Errorf("price is not active")
	}

	// Validate product
	if s.productService != nil {
		product, err := s.productService.GetByID(ctx, price.ProductID)
		if err != nil {
			return nil, fmt.Errorf("product not found: %w", err)
		}
		if !product.IsActive {
			return nil, fmt.Errorf("product is not active")
		}
	}

	// Validate Solana config
	solanaProc, err := requireSolanaProcessorConfig(s.cfg)
	if err != nil {
		return nil, err
	}
	tokenCfg, ok := solanaProc.SupportedTokens[tokenSymbol]
	if !ok || !tokenCfg.Enabled {
		return nil, fmt.Errorf("invalid or unsupported token: %s", tokenSymbol)
	}

	// Calculate token amount from fiat price with FX conversion if needed
	quote, err := CalculateTokenQuote(ctx, tokenCfg, price.Amount, price.Currency, s.fxProvider)
	if err != nil {
		return nil, fmt.Errorf("failed to calculate token quote: %w", err)
	}
	tokenUnits := quote.Units
	if tokenUnits == 0 {
		return nil, fmt.Errorf("calculated token amount is zero")
	}

	// Generate reference for Solana Pay
	reference, err := solana.GenerateReference()
	if err != nil {
		return nil, fmt.Errorf("failed to generate reference: %w", err)
	}

	// Get merchant recipient wallet
	recipient := solanaProc.RecipientWallet
	if recipient == "" {
		return nil, fmt.Errorf("merchant wallet not configured")
	}

	// Get token mint
	tokenMint := tokenCfg.Mint
	if solanaProc.Network == "mainnet" && tokenCfg.MainnetMint != "" {
		tokenMint = tokenCfg.MainnetMint
	}

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
	}
	if sessionID != nil && *sessionID != uuid.Nil {
		pending.SessionID = sessionID.String()
	}

	if err := s.storePendingPayment(ctx, reference, pending); err != nil {
		return nil, fmt.Errorf("failed to store pending payment: %w", err)
	}

	// Build Solana Pay Transfer Request URL
	url := s.buildTransferRequestURL(recipient, tokenUnits, tokenMint, tokenSymbol, reference)

	return &SolanaPayResult{
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

// buildTransferRequestURL constructs the solana: URL per the Solana Pay spec
func (s *SolanaPayService) buildTransferRequestURL(recipient string, amount uint64, tokenMint, tokenSymbol, reference string) string {
	// Base URL: solana:<recipient>
	baseURL := fmt.Sprintf("solana:%s", recipient)

	// Get token config for decimals
	solanaProc, err := requireSolanaProcessorConfig(s.cfg)
	if err != nil {
		return baseURL // fallback without params if not configured
	}
	tokenCfg := solanaProc.SupportedTokens[tokenSymbol]

	// Format amount with proper decimals
	formattedAmount := formatTokenAmount(amount, tokenCfg.Decimals)

	// Add query params
	params := fmt.Sprintf("?amount=%s", formattedAmount)

	// Add spl-token param if not native SOL
	if tokenMint != "" && tokenSymbol != "SOL" {
		params += fmt.Sprintf("&spl-token=%s", tokenMint)
	}

	// Add reference for payment detection
	params += fmt.Sprintf("&reference=%s", reference)

	// Add label
	label := "Purchase"
	if s.cfg != nil && s.cfg.Store != nil {
		if name := strings.TrimSpace(s.cfg.Store.Name); name != "" {
			label = name + " Purchase"
		}
	}
	params += fmt.Sprintf("&label=%s", url.QueryEscape(label))

	return baseURL + params
}

// storePendingPayment stores a pending payment in Redis
func (s *SolanaPayService) storePendingPayment(ctx context.Context, reference string, pending *PendingSolanaPayment) error {
	if s.redis == nil {
		return fmt.Errorf("redis not configured")
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
	if err := s.redis.SAdd(ctx, pendingSolanaPaymentsKey, reference).Err(); err != nil {
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

// GetAllPendingReferences returns all pending payment references
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

	// Remove from set
	if err := s.redis.SRem(ctx, pendingSolanaPaymentsKey, reference).Err(); err != nil {
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

// formatTokenAmount formats a token amount with the appropriate decimal places
func formatTokenAmount(amount uint64, decimals int) string {
	if decimals <= 0 {
		return fmt.Sprintf("%d", amount)
	}
	// Convert to string with decimal point
	divisor := uint64(1)
	for i := 0; i < decimals; i++ {
		divisor *= 10
	}
	whole := amount / divisor
	frac := amount % divisor
	if frac == 0 {
		return fmt.Sprintf("%d", whole)
	}
	// Format fractional part with leading zeros
	fracStr := fmt.Sprintf("%0*d", decimals, frac)
	// Trim trailing zeros
	fracStr = strings.TrimRight(fracStr, "0")
	return fmt.Sprintf("%d.%s", whole, fracStr)
}
