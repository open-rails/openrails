package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/processors"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

// CheckoutRequest represents a unified checkout request
type CheckoutRequest struct {
	PriceID         string `json:"price_id"`
	PaymentMethodID string `json:"payment_method_id,omitempty"` // For stored payment methods
	PaymentToken    string `json:"payment_token,omitempty"`     // For direct tokenized card
	Processor       string `json:"processor"`                   // mobius, ccbill, solana
	SuccessURL      string `json:"success_url,omitempty"`
	CancelURL       string `json:"cancel_url,omitempty"`

	// IdempotencyKey is an optional client-provided key for deduplication.
	// If provided, the same key with a successful result will return the cached response.
	// If not provided, a key is generated from user_id:price_id.
	IdempotencyKey string `json:"-"` // Set from header, not JSON body
	// CheckoutSessionID links hosted checkout redirects to a checkout session.
	CheckoutSessionID string `json:"-"`

	// Optional billing info (used when creating vault from payment token)
	Email      string `json:"email,omitempty"`
	FirstName  string `json:"first_name,omitempty"`
	LastName   string `json:"last_name,omitempty"`
	Address1   string `json:"address1,omitempty"`
	City       string `json:"city,omitempty"`
	State      string `json:"state,omitempty"`
	Zip        string `json:"zip,omitempty"`
	Country    string `json:"country,omitempty"`
	LastFour   string `json:"last_four,omitempty"`
	CardType   string `json:"card_type,omitempty"`
	ExpiryDate string `json:"expiry_date,omitempty"`

	// Optional metadata forwarded from checkout session (used for E2E tracing).
	Metadata map[string]string `json:"-"`
}

// CheckoutResponse represents the unified checkout response
type CheckoutResponse struct {
	Status         string     `json:"status"`                    // success, pending, redirect_required, blocked
	Action         string     `json:"action,omitempty"`          // new, upgrade, downgrade - indicates the type of checkout
	Message        string     `json:"message,omitempty"`         // User-friendly message
	SubscriptionID *uuid.UUID `json:"subscription_id,omitempty"` // For subscription purchases
	PaymentID      *uuid.UUID `json:"payment_id,omitempty"`      // For one-time purchases
	TransactionID  string     `json:"transaction_id,omitempty"`  // Processor transaction ID
	RedirectURL    string     `json:"redirect_url,omitempty"`    // For CCBill FlexForm
	DelayedStart   *time.Time `json:"delayed_start,omitempty"`   // When subscription/entitlement will start
}

// TierChangeRequest represents a request to change subscription tier
type TierChangeRequest struct {
	PriceID        string    `json:"price_id"`
	SubscriptionID uuid.UUID `json:"-"` // Set from path param
	IdempotencyKey string    `json:"-"` // Set from header, not JSON body
}

// TierChangeResponse represents the response from a tier change operation.
// This reuses the CheckoutSessionResponse envelope pattern for API consistency.
type TierChangeResponse struct {
	Object         string                         `json:"object"`                    // "tier_change"
	ID             string                         `json:"id,omitempty"`              // Operation ID for tracking
	Status         string                         `json:"status"`                    // succeeded, requires_action, blocked
	Mode           string                         `json:"mode"`                      // "tier_change"
	Action         string                         `json:"action,omitempty"`          // upgrade, downgrade
	PriceID        string                         `json:"price_id"`                  // Target price ID
	Payment        CheckoutSessionPaymentResponse `json:"payment"`                   // Processor info
	SubscriptionID *string                        `json:"subscription_id,omitempty"` // Affected subscription
	NextAction     *CheckoutSessionNextAction     `json:"next_action,omitempty"`     // For redirects
	Message        string                         `json:"message,omitempty"`         // User-friendly message
	DelayedStart   *time.Time                     `json:"delayed_start,omitempty"`   // For scheduled downgrades
}

// Sentinel errors for tier change operations
var (
	ErrTierChangeNoSubscription = errors.New("no active subscription found")
	ErrTierChangeNotSupported   = errors.New("tier change not supported for this processor")
	ErrTierChangeBlocked        = errors.New("tier change blocked")
	ErrTierChangePending        = errors.New("tier change already pending")
	ErrTierChangeSameProduct    = errors.New("already on this plan")
	ErrTierChangeDifferentGroup = errors.New("cannot change to a different tier group")
)

// TierChangeError provides structured error details for tier change operations
type TierChangeError struct {
	HTTPStatus int
	Message    string
	Code       string
}

func (e *TierChangeError) Error() string {
	return e.Message
}

// CoverageInfo represents the user's current product coverage
type CoverageInfo struct {
	HasCoverage  bool       // Whether user has any active coverage
	IsIndefinite bool       // True if coverage has no end date
	EndDate      *time.Time // When coverage ends (nil if indefinite)
	SourceType   string     // "subscription" or "entitlement"
	SourceID     *uuid.UUID // ID of the subscription or entitlement source
}

// EligibilityStatus represents the result of a purchase eligibility check
type EligibilityStatus string

const (
	EligibilityAllowed   EligibilityStatus = "allowed"   // User can purchase
	EligibilityBlocked   EligibilityStatus = "blocked"   // User already has this product
	EligibilityUpgrade   EligibilityStatus = "upgrade"   // User is upgrading within a tier group
	EligibilityDowngrade EligibilityStatus = "downgrade" // User is downgrading within a tier group
)

// EligibilityResult contains the result of checking if a user can purchase a product
type EligibilityResult struct {
	Status               EligibilityStatus    // Whether the purchase is allowed
	Reason               string               // Human-readable explanation
	Coverage             *CoverageInfo        // User's current coverage for this product
	ExistingSubscription *models.Subscription // For upgrades/downgrades, the existing subscription
	ExistingProduct      *models.Product      // For upgrades/downgrades, the existing product
}

// CheckoutService handles unified checkout for subscriptions and one-time purchases
type CheckoutService struct {
	SubscriptionService  *SubscriptionService
	ProductService       *ProductService
	PriceService         *PriceService
	PaymentService       *PaymentService
	EntitlementService   *EntitlementService
	PaymentMethodService *PaymentMethodService
	VaultService         *VaultService
	IdempotencyService   *IdempotencyService
	NMIClients           map[string]*nmi.NMIClient
	Clock                clockwork.Clock
	Config               *config.Config
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *CheckoutService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// NewCheckoutService creates a new CheckoutService
func NewCheckoutService(
	subscriptionService *SubscriptionService,
	productService *ProductService,
	priceService *PriceService,
	paymentService *PaymentService,
	entitlementService *EntitlementService,
	paymentMethodService *PaymentMethodService,
	vaultService *VaultService,
	idempotencyService *IdempotencyService,
	nmiClients map[string]*nmi.NMIClient,
	cfg *config.Config,
) *CheckoutService {
	return &CheckoutService{
		SubscriptionService:  subscriptionService,
		ProductService:       productService,
		PriceService:         priceService,
		PaymentService:       paymentService,
		EntitlementService:   entitlementService,
		PaymentMethodService: paymentMethodService,
		VaultService:         vaultService,
		IdempotencyService:   idempotencyService,
		NMIClients:           nmiClients,
		Config:               cfg,
	}
}

// getIdempotencyKey returns the idempotency key to use for a checkout operation.
// If the request contains a client-provided key, use it. Otherwise generate one.
func (s *CheckoutService) getIdempotencyKey(req *CheckoutRequest, userID string, priceID uuid.UUID, operation string) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	// Fall back to generated key based on operation type
	switch operation {
	case "nmi_sale":
		return GenerateKeyForSale(userID, priceID)
	case "nmi_subscription":
		return GenerateKeyForSubscription(userID, priceID)
	default:
		return GenerateKeyForSale(userID, priceID)
	}
}

// getUpgradeIdempotencyKey returns the idempotency key for an upgrade operation.
// If client-provided key exists, use it. Otherwise generate from upgrade parameters.
func (s *CheckoutService) getUpgradeIdempotencyKey(req *CheckoutRequest, userID string, existingSubID, newPriceID uuid.UUID) string {
	if req.IdempotencyKey != "" {
		return req.IdempotencyKey
	}
	return GenerateKeyForUpgrade(userID, existingSubID, newPriceID)
}

// CheckPurchaseEligibility determines if a user can purchase a given price.
// This should be called BEFORE generating payment URLs or charging cards.
//
// Returns:
//   - EligibilityAllowed: User can proceed with purchase
//   - EligibilityBlocked: User already owns this product (duplicate prevention)
//   - EligibilityUpgrade: User is upgrading within a tier group
//   - EligibilityDowngrade: User is downgrading within a tier group
//
// For upgrades/downgrades, the caller can decide how to handle (e.g., proration).
// For blocked, the caller should reject the purchase attempt.
func (s *CheckoutService) CheckPurchaseEligibility(ctx context.Context, userID string, priceID uuid.UUID) (*EligibilityResult, error) {
	// Get price
	price, err := s.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsActive {
		return &EligibilityResult{
			Status: EligibilityBlocked,
			Reason: "price is not available for purchase",
		}, nil
	}

	// Get product
	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found: %w", err)
	}
	if !product.IsActive {
		return &EligibilityResult{
			Status: EligibilityBlocked,
			Reason: "product is not available for purchase",
		}, nil
	}

	// Check for tier group conflicts (upgrade/downgrade scenarios)
	if product.TierGroup != nil && *product.TierGroup != "" {
		existingSub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndTierGroup(ctx, userID, *product.TierGroup)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("failed to check tier group: %w", err)
		}

		if existingSub != nil {
			existingProduct := existingSub.Price.Product
			if existingProduct == nil {
				return nil, errors.New("failed to load existing product for tier comparison")
			}

			if existingProduct.ID == product.ID {
				// Same product - fall through to coverage check
			} else if existingProduct.TierRank < product.TierRank {
				// Upgrade: existing is lower tier than requested
				return &EligibilityResult{
					Status:               EligibilityUpgrade,
					Reason:               fmt.Sprintf("Upgrading from %s to %s", existingProduct.DisplayName, product.DisplayName),
					ExistingSubscription: existingSub,
					ExistingProduct:      existingProduct,
				}, nil
			} else if existingProduct.TierRank > product.TierRank {
				// Downgrade: existing is higher tier than requested
				return &EligibilityResult{
					Status:               EligibilityDowngrade,
					Reason:               fmt.Sprintf("Downgrading from %s to %s", existingProduct.DisplayName, product.DisplayName),
					ExistingSubscription: existingSub,
					ExistingProduct:      existingProduct,
				}, nil
			} else {
				// Same tier rank but different product - block as duplicate
				return &EligibilityResult{
					Status: EligibilityBlocked,
					Reason: fmt.Sprintf("You already have an equivalent product (%s) in this tier", existingProduct.DisplayName),
				}, nil
			}
		}
	}

	// Check for existing coverage
	coverage, err := s.GetUserProductCoverage(ctx, userID, product)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing coverage: %w", err)
	}

	if coverage.HasCoverage && coverage.IsIndefinite {
		// User has indefinite coverage - block purchase
		return &EligibilityResult{
			Status:   EligibilityBlocked,
			Reason:   "You already have active access to this product",
			Coverage: coverage,
		}, nil
	}

	// User can purchase (possibly with delayed start if they have finite coverage)
	return &EligibilityResult{
		Status:   EligibilityAllowed,
		Reason:   "Purchase allowed",
		Coverage: coverage,
	}, nil
}

// Checkout processes a unified checkout request
func (s *CheckoutService) Checkout(ctx context.Context, req *CheckoutRequest, user *UserIdentity) (*CheckoutResponse, error) {
	// Parse and validate price
	priceID, err := api.ParsePriceID(req.PriceID)
	if err != nil {
		return nil, fmt.Errorf("invalid price_id: %w", err)
	}

	price, err := s.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsActive {
		return nil, errors.New("price is not available for purchase")
	}

	// Get product
	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found: %w", err)
	}
	if !product.IsActive {
		return nil, errors.New("product is not available for purchase")
	}

	// Normalize processor
	processor := strings.TrimSpace(strings.ToLower(req.Processor))
	if processor == "" {
		return nil, errors.New("processor is required")
	}

	// Check for tier group conflicts (upgrade/downgrade scenarios)
	// This must happen BEFORE the general coverage check
	if product.TierGroup != nil && *product.TierGroup != "" {
		existingSub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndTierGroup(ctx, user.ID, *product.TierGroup)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("failed to check tier group: %w", err)
		}

		if existingSub != nil {
			// User has an active subscription in the same tier group
			existingProduct := existingSub.Price.Product
			if existingProduct == nil {
				return nil, errors.New("failed to load existing product for tier comparison")
			}

			if existingProduct.ID == product.ID {
				// Same product - this is a duplicate, not an upgrade/downgrade
				// Fall through to normal coverage check below
			} else if existingProduct.TierRank < product.TierRank {
				// Upgrade detected - direct to change-tier endpoint
				return &CheckoutResponse{
					Status:  "blocked",
					Message: "Use POST /v1/me/subscriptions/change-tier for tier upgrades",
				}, nil
			} else if existingProduct.TierRank > product.TierRank {
				// Downgrade detected - direct to change-tier endpoint
				return &CheckoutResponse{
					Status:  "blocked",
					Message: "Use POST /v1/me/subscriptions/change-tier for tier downgrades",
				}, nil
			} else {
				// Same tier rank but different product - treat as duplicate
				return &CheckoutResponse{
					Status:  "blocked",
					Message: fmt.Sprintf("You already have an equivalent product (%s) in this tier", existingProduct.DisplayName),
				}, nil
			}
		}
	}

	// Check for existing coverage and determine if purchase is allowed
	coverage, err := s.GetUserProductCoverage(ctx, user.ID, product)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing coverage: %w", err)
	}

	// Deduplication logic
	if coverage.HasCoverage {
		if coverage.IsIndefinite {
			// User has indefinite coverage - block purchase
			return &CheckoutResponse{
				Status:  "blocked",
				Message: "You already have active access to this product",
			}, nil
		}

		// User has coverage with an end date
		// CCBill cannot schedule future start dates - block
		if processor == "ccbill" {
			return &CheckoutResponse{
				Status:  "blocked",
				Message: "You already have active access. CCBill subscriptions cannot be scheduled for future start. Please try again when your current access expires.",
			}, nil
		}

		// Other processors: allow with delayed start
	}

	// Determine if this is a subscription or one-time purchase
	isSubscription := price.BillingCycleDays != nil

	if isSubscription {
		return s.processSubscription(ctx, req, user, price, product, coverage, processor)
	}
	return s.processOneTimePurchase(ctx, req, user, price, product, coverage, processor)
}

// GetUserProductCoverage checks if user has active coverage for a product.
// It checks both:
// 1. Active/pending subscriptions (using the denormalized ProductID field)
// 2. Active entitlements matching the product's EntitlementsSpec
func (s *CheckoutService) GetUserProductCoverage(ctx context.Context, userID string, product *models.Product) (*CoverageInfo, error) {
	now := s.now()
	coverage := &CoverageInfo{HasCoverage: false}

	// Check for active/pending subscription for this product (single query using denormalized ProductID)
	if s.SubscriptionService != nil {
		sub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndProductID(ctx, userID, product.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("failed to check subscription: %w", err)
		}
		if sub != nil {
			coverage.HasCoverage = true
			coverage.SourceType = "subscription"
			coverage.SourceID = &sub.ID

			// Check if subscription has an end date
			if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
				coverage.IsIndefinite = true
				return coverage, nil
			}

			coverage.EndDate = sub.CurrentPeriodEndsAt
		}
	}

	// Check for active entitlements from the product's EntitlementsSpec
	if s.EntitlementService != nil && product.EntitlementsSpec != nil {
		for entitlementName := range product.EntitlementsSpec {
			// Check for indefinite entitlement
			hasIndefinite, err := s.EntitlementService.HasActiveIndefinite(ctx, userID, entitlementName, now)
			if err != nil {
				return nil, fmt.Errorf("failed to check indefinite entitlement: %w", err)
			}
			if hasIndefinite {
				coverage.HasCoverage = true
				coverage.IsIndefinite = true
				coverage.SourceType = "entitlement"
				return coverage, nil
			}

			// Check for finite entitlement
			ent, err := s.EntitlementService.LatestFiniteWindow(ctx, userID, entitlementName, now)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("failed to check finite entitlement: %w", err)
			}
			if ent != nil && ent.EndAt != nil {
				coverage.HasCoverage = true
				coverage.SourceType = "entitlement"
				coverage.SourceID = &ent.ID

				if coverage.EndDate == nil || ent.EndAt.After(*coverage.EndDate) {
					coverage.EndDate = ent.EndAt
				}
			}
		}
	}

	return coverage, nil
}

// processSubscription handles subscription purchases
func (s *CheckoutService) processSubscription(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
	product *models.Product,
	coverage *CoverageInfo,
	processor string,
) (*CheckoutResponse, error) {
	// Route to processor-specific handler based on config type detection
	// This allows adding new NMI providers via config without code changes
	switch {
	case processor == "ccbill":
		return s.processCCBillSubscription(ctx, req, user, price)
	case processors.IsNMIBacked(processor):
		return s.processNMISubscription(ctx, req, user, price, product, coverage)
	case processor == "stripe":
		return s.processStripeSubscription(ctx, req, user, price, coverage)
	case processor == "solana":
		return nil, errors.New("solana does not support recurring subscriptions; use a one-time price instead")
	default:
		return nil, fmt.Errorf("unsupported processor: %s", processor)
	}
}

// processOneTimePurchase handles one-time purchases
func (s *CheckoutService) processOneTimePurchase(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
	product *models.Product,
	coverage *CoverageInfo,
	processor string,
) (*CheckoutResponse, error) {
	// Route to processor-specific handler based on config type detection
	// This allows adding new NMI providers via config without code changes
	switch {
	case processors.IsNMIBacked(processor):
		return s.processNMISale(ctx, req, user, price, product, coverage)
	case processor == "solana":
		return s.processSolanaPurchase(ctx, req, user, price, product, coverage)
	case processor == "ccbill":
		return nil, errors.New("ccbill does not support one-time purchases; use a subscription price instead")
	case processor == "stripe":
		return s.processStripePayment(ctx, req, user, price)
	default:
		return nil, fmt.Errorf("unsupported processor for one-time purchases: %s", processor)
	}
}

// processCCBillSubscription handles CCBill subscription creation
// Returns a FlexForm URL that the client can redirect to for payment
func (s *CheckoutService) processCCBillSubscription(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
) (*CheckoutResponse, error) {
	// Validate CCBill configuration
	ccbillProc, err := requireCCBillProcessorConfig(s.Config)
	if err != nil {
		return nil, err
	}

	// Validate price has CCBill configuration
	formName, flexID, hasCCBill := price.GetCCBillFlexForm()
	if !hasCCBill {
		return nil, fmt.Errorf("price %s is not configured for CCBill", price.ID)
	}

	// User must have verified email for CCBill payments
	if user.Email == nil || strings.TrimSpace(*user.Email) == "" {
		return nil, errors.New("verified email required for CCBill payments")
	}

	// User must have a username for CCBill (used for webhook resolution via profiles.users)
	if user.Username == "" {
		return nil, errors.New("username required for CCBill payments")
	}

	// Generate FlexForm URL
	ccbillClient := ccbill.NewClient(ccbillProc.ToCCBillConfig(), s.Config.IsTestMode())
	flexFormParams := &ccbill.GenerateFlexFormURLParams{
		Username:      user.Username,
		Email:         *user.Email,
		CustomerFName: req.FirstName,
		CustomerLName: req.LastName,
		Address1:      req.Address1,
		City:          req.City,
		State:         req.State,
		ZipCode:       req.Zip,
		Country:       req.Country,
		FlexID:        flexID,
		FormName:      formName,
	}

	response, err := ccbillClient.GenerateFlexFormURL(flexFormParams)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CCBill FlexForm URL: %w", err)
	}

	log.WithFields(log.Fields{
		"user_id":  user.ID,
		"price_id": price.ID,
	}).Info("Generated CCBill FlexForm URL via checkout")

	return &CheckoutResponse{
		Status:      "redirect_required",
		Action:      "new",
		Message:     "Redirect to CCBill payment form",
		RedirectURL: response.RedirectURL,
	}, nil
}

// processCCBillUpgrade handles CCBill subscription upgrades
// Returns a FlexForm URL for the upgrade that the client can redirect to
func (s *CheckoutService) processCCBillUpgrade(
	ctx context.Context,
	user *UserIdentity,
	newPrice *models.Price,
	existingSub *models.Subscription,
) (*CheckoutResponse, error) {
	// Validate CCBill configuration
	ccbillProc, err := requireCCBillProcessorConfig(s.Config)
	if err != nil {
		return nil, err
	}

	// Validate existing subscription is CCBill
	if existingSub.Processor != models.ProcessorCCBill {
		return nil, errors.New("existing subscription is not a CCBill subscription")
	}
	if existingSub.ProcessorSubscriptionID == "" {
		return nil, errors.New("existing subscription is missing CCBill reference")
	}

	// Validate new price has CCBill configuration
	formName, flexID, hasCCBill := newPrice.GetCCBillFlexForm()
	if !hasCCBill {
		return nil, fmt.Errorf("target price %s is not configured for CCBill", newPrice.ID)
	}

	// User must have verified email for CCBill payments
	if user.Email == nil || strings.TrimSpace(*user.Email) == "" {
		return nil, errors.New("verified email required for CCBill payments")
	}

	// User must have a username for CCBill (used for webhook resolution via profiles.users)
	if user.Username == "" {
		return nil, errors.New("username required for CCBill payments")
	}

	// Generate upgrade FlexForm URL
	ccbillClient := ccbill.NewClient(ccbillProc.ToCCBillConfig(), s.Config.IsTestMode())
	upgradeParams := &ccbill.GenerateUpgradeFlexFormURLParams{
		Username:               user.Username,
		Email:                  *user.Email,
		FormName:               formName,
		FlexID:                 flexID,
		OriginalSubscriptionID: existingSub.ProcessorSubscriptionID,
	}

	response, err := ccbillClient.GenerateUpgradeFlexFormURL(upgradeParams)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CCBill upgrade FlexForm URL: %w", err)
	}

	log.WithFields(log.Fields{
		"user_id":                   user.ID,
		"subscription_id":           existingSub.ID,
		"current_price_id":          existingSub.PriceID,
		"target_price_id":           newPrice.ID,
		"processor_subscription_id": existingSub.ProcessorSubscriptionID,
	}).Info("Generated CCBill upgrade FlexForm URL via checkout")

	return &CheckoutResponse{
		Status:         "redirect_required",
		Action:         "upgrade",
		Message:        "Redirect to CCBill upgrade form",
		RedirectURL:    response.RedirectURL,
		SubscriptionID: &existingSub.ID,
	}, nil
}

// processNMISubscription handles NMI-backed subscription creation (mobius, etc.)
// subscriptionIdempotencyResult stores the cached result of a successful subscription creation
type subscriptionIdempotencyResult struct {
	SubscriptionID string  `json:"subscription_id"`
	TransactionID  string  `json:"transaction_id"`
	DelayedStart   *string `json:"delayed_start,omitempty"`
}

func (s *CheckoutService) processNMISubscription(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
	product *models.Product,
	coverage *CoverageInfo,
) (*CheckoutResponse, error) {
	// Get NMI plan configuration from price
	nmiPlanID, provider, hasNMI := price.GetNMIConfig()
	if !hasNMI || nmiPlanID == "" {
		return nil, fmt.Errorf("price %s is missing NMI plan configuration", price.ID)
	}

	client, ok := s.NMIClients[provider]
	if !ok {
		return nil, fmt.Errorf("NMI provider '%s' is not configured", provider)
	}

	// Get idempotency key (client-provided or generated)
	const idempOp = "nmi_subscription"
	idempotencyKey := s.getIdempotencyKey(req, user.ID, price.ID, idempOp)

	// Check idempotency - have we already processed this request?
	idempRec, alreadyExists, err := s.IdempotencyService.Begin(ctx, idempOp, idempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("idempotency check failed: %w", err)
	}

	if alreadyExists {
		switch idempRec.Status {
		case IdempotencyStatusSuccess:
			// Return cached result
			var cached subscriptionIdempotencyResult
			if err := json.Unmarshal(idempRec.Result, &cached); err != nil {
				log.WithError(err).Warn("failed to unmarshal cached subscription result")
				return &CheckoutResponse{
					Status:        "success",
					Action:        "new",
					Message:       "Subscription already created",
					TransactionID: cached.TransactionID,
				}, nil
			}
			subID, _ := uuid.Parse(cached.SubscriptionID)
			var delayedStart *time.Time
			if cached.DelayedStart != nil {
				if t, err := time.Parse(time.RFC3339, *cached.DelayedStart); err == nil {
					delayedStart = &t
				}
			}
			return &CheckoutResponse{
				Status:         "success",
				Action:         "new",
				Message:        "Subscription already created",
				SubscriptionID: &subID,
				TransactionID:  cached.TransactionID,
				DelayedStart:   delayedStart,
			}, nil
		case IdempotencyStatusPending:
			return nil, errors.New("subscription creation already in progress, please wait")
		case IdempotencyStatusFailed:
			return nil, errors.New("previous subscription attempt failed, please try again")
		}
	}

	// Get or create vault (payment method)
	customerVaultID, createdPaymentMethod, err := s.resolveVault(ctx, req, user, provider)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// Build subscription ID
	subscriptionID := uuid.New()

	// Optional E2E tracing (forwarded from checkout session metadata)
	orderID := subscriptionID.String()
	poNumber := subscriptionID.String()
	if req.Metadata != nil {
		if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
			orderID = fmt.Sprintf("e2e_%s_%s", runID, subscriptionID.String())
			poNumber = orderID
		}
	}

	// Determine start date for delayed start
	var startDate string
	var delayedStart *time.Time
	if coverage.HasCoverage && coverage.EndDate != nil {
		// Schedule subscription to start when current coverage ends
		startDate = coverage.EndDate.Format("20060102") // YYYYMMDD format for NMI
		delayedStart = coverage.EndDate
	}

	// Build NMI params
	params := nmi.RecurringPaymentData{
		CardUserData: nmi.CardUserData{
			FirstName: s.resolveFirstName(req, user),
			LastName:  s.resolveLastName(req),
			Address1:  s.defaultIfEmpty(req.Address1, "N/A"),
			City:      s.defaultIfEmpty(req.City, "N/A"),
			State:     s.defaultIfEmpty(req.State, "N/A"),
			Zip:       s.defaultIfEmpty(req.Zip, "00000"),
			Country:   s.defaultIfEmpty(req.Country, "US"),
		},
		PlanID:          nmiPlanID,
		CustomerVaultID: customerVaultID,
		Amount:          float64(price.Amount) / 100.0, // Convert cents to dollars
		Currency:        price.Currency,
		Email:           req.Email,
		OrderID:         orderID,
		PONumber:        poNumber,
		CustomerID:      user.ID,
		StartDate:       startDate,
	}

	// Create subscription with NMI
	resp, err := client.AddRecurringSubscription(params)
	if err != nil {
		wrappedErr := fmt.Errorf("failed to create subscription: %w", err)
		var nmiErr *nmi.CustomerVaultError
		if errors.As(err, &nmiErr) {
			wrappedErr = &VaultError{
				Err:            wrappedErr,
				LocalizationID: nmiErr.LocalizationID,
				Message:        wrappedErr.Error(),
			}
		}
		// Cleanup vault if we created it
		if createdPaymentMethod != nil && s.VaultService != nil {
			if cleanupErr := s.VaultService.DeleteVault(ctx, createdPaymentMethod); cleanupErr != nil {
				log.WithError(cleanupErr).WithField("vault_id", customerVaultID).Warn("failed to cleanup payment method after subscription error")
			}
		}
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, wrappedErr)
		return nil, wrappedErr
	}

	// Determine initial status
	status := models.StatusPending
	if startDate != "" {
		// Delayed start - subscription won't charge until start date
		status = models.StatusPending
	}

	// Create subscription record
	var emailPtr *string
	if req.Email != "" {
		emailPtr = &req.Email
	}

	subscription := &models.Subscription{
		ID:                      subscriptionID,
		UserID:                  user.ID,
		ProductID:               price.ProductID,
		PriceID:                 price.ID,
		ProcessorSubscriptionID: resp.SubscriptionID,
		Status:                  status,
		Processor:               models.ProcessorMobius,
		UserEmail:               emailPtr,
		StartedAt:               *timePtr(time.Now()),
	}

	if req.Metadata != nil {
		if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
			if payload, err := json.Marshal(map[string]any{"e2e_run_id": runID, "order_id": orderID}); err == nil {
				subscription.Metadata = payload
			}
		}
	}

	if createdPaymentMethod != nil {
		subscription.PaymentMethodID = &createdPaymentMethod.ID
	} else if req.PaymentMethodID != "" {
		if pmID, err := api.ParsePaymentMethodID(req.PaymentMethodID); err == nil {
			subscription.PaymentMethodID = &pmID
		} else {
			log.WithError(err).Warn("failed to parse payment_method_id while persisting subscription")
		}
	}

	if err := s.SubscriptionService.Create(ctx, subscription); err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("failed to save subscription: %w", err)
	}

	// Cache successful result for idempotency replay
	var delayedStartStr *string
	if delayedStart != nil {
		ds := delayedStart.Format(time.RFC3339)
		delayedStartStr = &ds
	}
	cachedResult, _ := json.Marshal(subscriptionIdempotencyResult{
		SubscriptionID: subscriptionID.String(),
		TransactionID:  resp.TransactionID,
		DelayedStart:   delayedStartStr,
	})
	_ = s.IdempotencyService.Complete(ctx, idempOp, idempotencyKey, cachedResult)

	statusMsg := "pending"
	message := "Subscription created successfully"
	if delayedStart != nil {
		message = fmt.Sprintf("Subscription scheduled to start on %s", delayedStart.Format("2006-01-02"))
	}

	// Leaving RegisterPurchase for immediate starts only,
	// TODO - Test in production to see when NMI charges the card.
	/*_, err = s.RegisterPurchase(ctx, &RegisterPurchaseRequest{
		UserID:         user.ID,
		PriceID:        price.ID,
		Processor:      "mobius",
		TransactionID:  resp.TransactionID,
		Amount:         price.Amount,
		Currency:       price.Currency,
		SubscriptionID: &subscriptionID,
		Metadata: func() map[string]any {
			if req.Metadata == nil {
				return nil
			}
			if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
				return map[string]any{"e2e_run_id": runID, "order_id": orderID}
			}
			return nil
		}(),
	})

	if err != nil {
		return nil, fmt.Errorf("failed to register purchase: %w", err)
	}*/

	return &CheckoutResponse{
		Status:         statusMsg,
		Action:         "new",
		Message:        message,
		SubscriptionID: &subscriptionID,
		TransactionID:  resp.TransactionID,
		DelayedStart:   delayedStart,
	}, nil
}

// saleIdempotencyResult stores the cached result of a successful sale for idempotency replay
type saleIdempotencyResult struct {
	TransactionID string    `json:"transaction_id"`
	PaymentID     uuid.UUID `json:"payment_id"`
	DelayedStart  *string   `json:"delayed_start,omitempty"`
}

// upgradeIdempotencyResult stores the cached result of a successful upgrade for idempotency replay
type upgradeIdempotencyResult struct {
	SubscriptionID         string `json:"subscription_id"`
	ProrationTransactionID string `json:"proration_transaction_id,omitempty"`
	Message                string `json:"message"`
}

// processNMISale handles NMI one-time sale (card purchase)
func (s *CheckoutService) processNMISale(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
	product *models.Product,
	coverage *CoverageInfo,
) (*CheckoutResponse, error) {
	provider := "mobius" // Default NMI provider

	client, ok := s.NMIClients[provider]
	if !ok {
		return nil, fmt.Errorf("NMI provider '%s' is not configured", provider)
	}

	// Get idempotency key (client-provided or generated)
	const idempOp = "nmi_sale"
	idempotencyKey := s.getIdempotencyKey(req, user.ID, price.ID, idempOp)

	// Check idempotency - have we already processed this request?
	idempRec, alreadyExists, err := s.IdempotencyService.Begin(ctx, idempOp, idempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("idempotency check failed: %w", err)
	}

	if alreadyExists {
		switch idempRec.Status {
		case IdempotencyStatusSuccess:
			// Return cached result
			var cached saleIdempotencyResult
			if err := json.Unmarshal(idempRec.Result, &cached); err != nil {
				log.WithError(err).Warn("failed to unmarshal cached sale result, proceeding anyway")
				return &CheckoutResponse{
					Status:        "success",
					Action:        "new",
					Message:       "Purchase already completed",
					TransactionID: cached.TransactionID,
				}, nil
			}
			var delayedStart *time.Time
			if cached.DelayedStart != nil {
				if t, err := time.Parse(time.RFC3339, *cached.DelayedStart); err == nil {
					delayedStart = &t
				}
			}
			return &CheckoutResponse{
				Status:        "success",
				Action:        "new",
				Message:       "Purchase already completed",
				PaymentID:     &cached.PaymentID,
				TransactionID: cached.TransactionID,
				DelayedStart:  delayedStart,
			}, nil
		case IdempotencyStatusPending:
			return nil, errors.New("checkout already in progress, please wait")
		case IdempotencyStatusFailed:
			// Previous attempt failed - allow retry after TTL expires
			return nil, errors.New("previous checkout attempt failed, please try again")
		}
	}

	// Get or create vault
	customerVaultID, createdPaymentMethod, err := s.resolveVault(ctx, req, user, provider)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// Generate order ID for the sale
	orderID := uuid.New().String()
	if req.Metadata != nil {
		if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
			orderID = fmt.Sprintf("e2e_%s_%s", runID, orderID)
		}
	}

	// Execute sale via NMI
	saleResp, err := client.RunSale(nmi.SaleParams{
		CustomerVaultID:  customerVaultID,
		Amount:           price.Amount,
		Currency:         price.Currency,
		OrderDescription: fmt.Sprintf("Purchase: %s - %s", product.DisplayName, price.DisplayName),
		OrderID:          orderID,
	})
	if err != nil {
		// Cleanup vault if we created it
		if createdPaymentMethod != nil && s.VaultService != nil {
			_ = s.VaultService.DeleteVault(ctx, createdPaymentMethod)
		}
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("payment failed: %w", err)
	}

	// Use RegisterPurchase to record payment and grant entitlements
	result, err := s.RegisterPurchase(ctx, &RegisterPurchaseRequest{
		UserID:        user.ID,
		PriceID:       price.ID,
		Processor:     string(models.ProcessorMobius),
		TransactionID: saleResp.TransactionID,
		Amount:        price.Amount,
		Currency:      price.Currency,
		Metadata: func() map[string]any {
			if req.Metadata == nil {
				return nil
			}
			if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
				return map[string]any{"e2e_run_id": runID, "order_id": orderID}
			}
			return nil
		}(),
	})
	if err != nil {
		log.WithError(err).WithField("transaction_id", saleResp.TransactionID).Error("failed to register purchase after successful NMI sale")
		// Note: We still mark as failed because the purchase wasn't fully registered
		// The user was charged but entitlements weren't granted - this needs manual review
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("payment processed but failed to register: %w", err)
	}

	// Cache successful result for idempotency replay
	var delayedStartStr *string
	if result.DelayedStart != nil {
		str := result.DelayedStart.Format(time.RFC3339)
		delayedStartStr = &str
	}
	cachedResult, _ := json.Marshal(saleIdempotencyResult{
		TransactionID: saleResp.TransactionID,
		PaymentID:     result.PaymentID,
		DelayedStart:  delayedStartStr,
	})
	_ = s.IdempotencyService.Complete(ctx, idempOp, idempotencyKey, cachedResult)

	return &CheckoutResponse{
		Status:        "success",
		Action:        "new",
		Message:       "Purchase completed successfully",
		PaymentID:     &result.PaymentID,
		TransactionID: saleResp.TransactionID,
		DelayedStart:  result.DelayedStart,
	}, nil
}

// processSolanaPurchase handles Solana one-time purchases
func (s *CheckoutService) processSolanaPurchase(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
	product *models.Product,
	coverage *CoverageInfo,
) (*CheckoutResponse, error) {
	return nil, errors.New("solana checkout is handled via /v1/checkout sessions")
}

func (s *CheckoutService) processStripeSubscription(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
	coverage *CoverageInfo,
) (*CheckoutResponse, error) {
	stripeProc, _, err := requireStripeSecretKey(s.Config)
	if err != nil {
		return nil, err
	}
	stripePriceID, err := getStripePriceID(price)
	if err != nil {
		return nil, err
	}
	successURL := strings.TrimSpace(req.SuccessURL)
	cancelURL := strings.TrimSpace(req.CancelURL)
	if successURL == "" {
		successURL = strings.TrimSpace(stripeProc.SuccessURL)
	}
	if cancelURL == "" {
		cancelURL = strings.TrimSpace(stripeProc.CancelURL)
	}
	if successURL == "" || cancelURL == "" {
		return nil, errors.New("stripe success/cancel URLs not available")
	}

	trialEnd := int64(0)
	if coverage != nil && coverage.HasCoverage && coverage.EndDate != nil && coverage.EndDate.After(time.Now().Add(5*time.Minute)) {
		trialEnd = coverage.EndDate.Unix()
	}
	urlStr, err := s.createStripeCheckoutSession(ctx, stripeCheckoutParams{
		Mode:              "subscription",
		PriceID:           stripePriceID,
		SuccessURL:        successURL,
		CancelURL:         cancelURL,
		UserID:            user.ID,
		InternalPriceID:   price.ID.String(),
		TrialEnd:          trialEnd,
		CheckoutSessionID: req.CheckoutSessionID,
	})
	if err != nil {
		return nil, err
	}
	return &CheckoutResponse{
		Status:      "redirect_required",
		Action:      "new",
		Message:     "Redirect to Stripe checkout",
		RedirectURL: urlStr,
	}, nil
}

func (s *CheckoutService) processStripePayment(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
) (*CheckoutResponse, error) {
	stripeProc, _, err := requireStripeSecretKey(s.Config)
	if err != nil {
		return nil, err
	}
	stripePriceID, err := getStripePriceID(price)
	if err != nil {
		return nil, err
	}
	successURL := strings.TrimSpace(req.SuccessURL)
	cancelURL := strings.TrimSpace(req.CancelURL)
	if successURL == "" {
		successURL = strings.TrimSpace(stripeProc.SuccessURL)
	}
	if cancelURL == "" {
		cancelURL = strings.TrimSpace(stripeProc.CancelURL)
	}
	if successURL == "" || cancelURL == "" {
		return nil, errors.New("stripe success/cancel URLs not available")
	}

	urlStr, err := s.createStripeCheckoutSession(ctx, stripeCheckoutParams{
		Mode:              "payment",
		PriceID:           stripePriceID,
		SuccessURL:        successURL,
		CancelURL:         cancelURL,
		UserID:            user.ID,
		InternalPriceID:   price.ID.String(),
		CheckoutSessionID: req.CheckoutSessionID,
	})
	if err != nil {
		return nil, err
	}
	return &CheckoutResponse{
		Status:      "redirect_required",
		Action:      "new",
		Message:     "Redirect to Stripe checkout",
		RedirectURL: urlStr,
	}, nil
}

func getStripePriceID(price *models.Price) (string, error) {
	if price == nil {
		return "", errors.New("price is required")
	}
	cfg := price.GetProcessorConfig(models.ProcessorStripe)
	if cfg == nil {
		return "", errors.New("stripe price not configured")
	}
	id := strings.TrimSpace(cfg[models.ProcessorKeyStripePriceID])
	if id == "" {
		return "", errors.New("stripe price id missing")
	}
	return id, nil
}

type stripeCheckoutParams struct {
	Mode              string
	PriceID           string
	SuccessURL        string
	CancelURL         string
	UserID            string
	InternalPriceID   string
	TrialEnd          int64
	CheckoutSessionID string
}

func (s *CheckoutService) createStripeCheckoutSession(ctx context.Context, params stripeCheckoutParams) (string, error) {
	stripeProc, _, err := requireStripeSecretKey(s.Config)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("mode", params.Mode)
	values.Set("success_url", params.SuccessURL)
	values.Set("cancel_url", params.CancelURL)
	values.Set("client_reference_id", params.UserID)
	values.Set("line_items[0][price]", params.PriceID)
	values.Set("line_items[0][quantity]", "1")
	values.Set("metadata[user_id]", params.UserID)
	values.Set("metadata[internal_price_id]", params.InternalPriceID)
	if strings.TrimSpace(params.CheckoutSessionID) != "" {
		values.Set("metadata[checkout_session_id]", strings.TrimSpace(params.CheckoutSessionID))
	}
	if params.Mode == "subscription" {
		values.Set("subscription_data[metadata][user_id]", params.UserID)
		values.Set("subscription_data[metadata][internal_price_id]", params.InternalPriceID)
		if strings.TrimSpace(params.CheckoutSessionID) != "" {
			values.Set("subscription_data[metadata][checkout_session_id]", strings.TrimSpace(params.CheckoutSessionID))
		}
		if params.TrialEnd > 0 {
			values.Set("subscription_data[trial_end]", strconv.FormatInt(params.TrialEnd, 10))
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/checkout/sessions", strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(stripeProc.SecretKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("stripe checkout failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		msg := parseStripeError(body)
		if msg == "" {
			msg = fmt.Sprintf("stripe checkout failed (%d)", resp.StatusCode)
		}
		return "", errors.New(msg)
	}
	var out struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("stripe checkout parse failed: %w", err)
	}
	if strings.TrimSpace(out.URL) == "" {
		return "", errors.New("stripe checkout returned empty URL")
	}
	return out.URL, nil
}

func parseStripeError(body []byte) string {
	var out struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return ""
	}
	return strings.TrimSpace(out.Error.Message)
}

// resolveVault gets an existing vault or creates one from payment token
func (s *CheckoutService) resolveVault(ctx context.Context, req *CheckoutRequest, user *UserIdentity, provider string) (string, *models.PaymentMethod, error) {
	// Try existing payment method first
	if req.PaymentMethodID != "" {
		pmID, err := api.ParsePaymentMethodID(req.PaymentMethodID)
		if err != nil {
			return "", nil, fmt.Errorf("invalid payment_method_id: %w", err)
		}

		pm, err := s.PaymentMethodService.ValidatePaymentMethodOperation(ctx, pmID, user.ID)
		if err != nil {
			return "", nil, fmt.Errorf("invalid payment method: %w", err)
		}

		if !processors.IsNMIBackedProcessor(pm.Processor) {
			return "", nil, errors.New("payment method is not compatible with card payments")
		}

		return pm.VaultID, nil, nil
	}

	// Create new vault from payment token
	if req.PaymentToken == "" {
		return "", nil, errors.New("payment_method_id or payment_token is required")
	}

	if s.VaultService == nil {
		return "", nil, errors.New("vault service unavailable")
	}

	pm, err := s.VaultService.CreateVault(ctx, user, &CreateVaultRequest{
		PaymentToken: req.PaymentToken,
		Provider:     provider,
		FirstName:    s.resolveFirstName(req, user),
		LastName:     s.resolveLastName(req),
		Address1:     req.Address1,
		City:         req.City,
		State:        req.State,
		Zip:          req.Zip,
		Country:      req.Country,
		Email:        req.Email,
		LastFour: func() string {
			if len(req.LastFour) > 4 {
				return req.LastFour[len(req.LastFour)-4:]
			}
			return req.LastFour
		}(),
		CardType:   req.CardType,
		ExpiryDate: req.ExpiryDate,
		Metadata: func() map[string]any {
			if req.Metadata == nil {
				return nil
			}
			if v := strings.TrimSpace(req.Metadata["e2e_run_id"]); v != "" {
				return map[string]any{"e2e_run_id": v}
			}
			return nil
		}(),
	})
	if err != nil {
		return "", nil, err
	}

	return pm.VaultID, pm, nil
}

// grantProductEntitlements grants entitlements from product spec after a one-time or subscription purchase
func (s *CheckoutService) grantProductEntitlements(
	ctx context.Context,
	userID string,
	product *models.Product,
	paymentID uuid.UUID,
	coverage *CoverageInfo,
	subscription bool,
	walletPurchase bool,
	billingCycleDays *int,
) error {
	if s.EntitlementService == nil || product.EntitlementsSpec == nil {
		return nil
	}

	now := s.now()

	for entitlementName, durationDays := range product.EntitlementsSpec {
		var startAt time.Time
		var endAt *time.Time

		// Determine start time
		if coverage.HasCoverage && coverage.EndDate != nil {
			// Delayed start - entitlement starts when current coverage ends
			startAt = *coverage.EndDate
		} else {
			// Immediate start
			startAt = now
		}

		// Set entitlements to expire 1 month from purchase for wallet purchases
		if walletPurchase && billingCycleDays == nil {
			newDate := startAt.AddDate(0, 1, 0)
			endAt = &newDate
		} else if billingCycleDays != nil && *billingCycleDays > 0 {
			// For wallet purchases with known billing cycle, set end date to match billing cycle
			end := startAt.Add(time.Duration(*billingCycleDays) * 24 * time.Hour)
			endAt = &end
		}

		// Determine end time
		if durationDays != nil && *durationDays > 0 {
			end := startAt.Add(time.Duration(*durationDays) * 24 * time.Hour)
			endAt = &end
		}
		// If durationDays is nil or 0, endAt stays nil (indefinite)

		paymentMode := models.EntitlementSourceOneOff
		if subscription {
			paymentMode = models.EntitlementSourceSubscription
		}
		notBefore := startAt
		var params PushNewEntitlementParams
		if endAt == nil {
			params = PushNewEntitlementParams{
				UserID:      userID,
				Entitlement: entitlementName,
				NotBefore:   &notBefore,
				Indefinite:  true,
				SourceType:  paymentMode,
				SourceID:    paymentID,
			}
		} else {
			e := endAt.UTC()
			params = PushNewEntitlementParams{
				UserID:      userID,
				Entitlement: entitlementName,
				NotBefore:   &notBefore,
				EndAt:       &e,
				SourceType:  paymentMode,
				SourceID:    paymentID,
			}
		}
		_, err := s.EntitlementService.PushNewEntitlement(ctx, params)

		if err != nil {
			log.WithError(err).WithFields(log.Fields{
				"user_id":     userID,
				"entitlement": entitlementName,
				"payment_id":  paymentID,
			}).Error("failed to grant entitlement")
			return err
		}

		log.WithFields(log.Fields{
			"user_id":     userID,
			"entitlement": entitlementName,
			"payment_id":  paymentID,
			"start_at":    startAt,
			"end_at":      endAt,
		}).Info(fmt.Sprintf("granted entitlement from %s purchase", paymentMode))
	}

	return nil
}

// Helper functions
func (s *CheckoutService) resolveFirstName(req *CheckoutRequest, user *UserIdentity) string {
	if req.FirstName != "" {
		return req.FirstName
	}
	if user.Username != "" {
		return user.Username
	}
	return "Customer"
}

func (s *CheckoutService) resolveLastName(req *CheckoutRequest) string {
	if req.LastName != "" {
		return req.LastName
	}
	return "Member"
}

func (s *CheckoutService) defaultIfEmpty(value, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}

// UpgradeResponse extends CheckoutResponse with upgrade-specific details
type UpgradeResponse struct {
	*CheckoutResponse
	ProratedAmount    int64      `json:"prorated_amount,omitempty"`     // Amount charged for proration
	OldSubscriptionID *uuid.UUID `json:"old_subscription_id,omitempty"` // The subscription being replaced
}

// RegisterPurchaseRequest contains everything needed to record a completed one-time purchase.
// This is the universal entry point for all processors after payment is confirmed.
type RegisterPurchaseRequest struct {
	UserID         string     // User who made the purchase
	PriceID        uuid.UUID  // Price that was purchased
	Processor      string     // "solana", "mobius", "ccbill"
	TransactionID  string     // Processor's transaction/signature ID
	Amount         int64      // Amount in smallest unit (cents for usd, lamports for SOL)
	Currency       string     // "usd", "USDC", "SOL", etc.
	SubscriptionID *uuid.UUID // Optional: link payment to subscription (for subscription renewals/purchases)
	WalletPurchase bool       // Is this purchase made via on-chain wallet (Solana)?
	// Optional: when this purchase happened (defaults to now).
	PurchasedAt *time.Time

	// Optional discount metadata (useful for off-channel/manual purchases).
	DiscountCode     *string
	DiscountReason   *string
	DiscountMetadata map[string]any

	// Optional metadata (e.g., E2E tracing).
	Metadata map[string]any
}

// RegisterPurchaseResponse contains the result of a registered purchase
type RegisterPurchaseResponse struct {
	PaymentID    uuid.UUID         // Created payment record ID
	Entitlements []string          // Names of entitlements granted
	DelayedStart *time.Time        // If user had coverage, when entitlements start
	Eligibility  EligibilityStatus // For logging: was this allowed, blocked (duplicate), upgrade, or downgrade
}

// RegisterPurchase records a confirmed one-time purchase and grants entitlements.
// This is the single source of truth for "user paid for product" logic.
//
// Called by:
//   - NMI/Mobius sale (after charging card)
//   - Solana poller (after detecting on-chain payment)
//   - CCBill webhook (after receiving payment confirmation)
//   - Admin API (for manual grants)
//
// It handles:
//  1. Creating the Payment record
//  2. Looking up Product from Price
//  3. Checking coverage for delayed start
//  4. Granting entitlements from Product.EntitlementsSpec
func (s *CheckoutService) RegisterPurchase(ctx context.Context, req *RegisterPurchaseRequest) (*RegisterPurchaseResponse, error) {
	// Validate required fields
	if req.UserID == "" {
		return nil, errors.New("user_id is required")
	}
	if req.TransactionID == "" {
		return nil, errors.New("transaction_id is required")
	}
	if req.Processor == "" {
		return nil, errors.New("processor is required")
	}

	// Get price
	price, err := s.PriceService.GetByID(ctx, req.PriceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}

	// Get product
	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found: %w", err)
	}

	// Check eligibility (for logging/analytics - can't block at this point, payment already happened)
	eligibility, err := s.CheckPurchaseEligibility(ctx, req.UserID, req.PriceID)
	if err != nil {
		// Log but don't fail - the payment happened, we need to record it
		log.WithError(err).WithFields(log.Fields{
			"user_id":  req.UserID,
			"price_id": req.PriceID,
		}).Warn("failed to check eligibility during RegisterPurchase")
		eligibility = &EligibilityResult{Status: EligibilityAllowed}
	}

	// Get coverage from eligibility result (avoids duplicate query)
	coverage := eligibility.Coverage
	if coverage == nil {
		coverage = &CoverageInfo{}
	}

	// Use provided amount/currency or fall back to price defaults
	amount := req.Amount
	if amount == 0 {
		amount = price.Amount
	}
	currency := req.Currency
	if currency == "" {
		currency = price.Currency
	}

	now := s.now()
	purchasedAt := now
	if req.PurchasedAt != nil {
		purchasedAt = (*req.PurchasedAt).UTC()
	}

	// Create payment record
	paymentID := uuid.New()
	payment := &models.Payment{
		ID:               paymentID,
		UserID:           req.UserID,
		PriceID:          price.ID,
		SubscriptionID:   req.SubscriptionID, // Link to subscription if provided
		Processor:        models.Processor(req.Processor),
		TransactionID:    req.TransactionID,
		Amount:           amount,
		ListAmount:       price.Amount,
		Currency:         currency,
		PurchasedAt:      purchasedAt,
		CreatedAt:        now,
		DiscountCode:     req.DiscountCode,
		DiscountReason:   req.DiscountReason,
		DiscountMetadata: req.DiscountMetadata,
		Metadata:         req.Metadata,
	}

	if err := s.PaymentService.Create(ctx, payment); err != nil {
		return nil, fmt.Errorf("failed to create payment record: %w", err)
	}

	sourceId := paymentID
	if req.SubscriptionID != nil {
		log.WithFields(log.Fields{
			"payment_id":      paymentID,
			"user_id":         req.UserID,
			"price_id":        req.PriceID,
			"subscription_id": req.SubscriptionID,
		}).Info("registered subscription payment")
		sourceId = *req.SubscriptionID
	}

	// Grant entitlements
	var grantedEntitlements []string
	if err := s.grantProductEntitlements(ctx, req.UserID, product, sourceId, coverage, req.SubscriptionID != nil && req.SubscriptionID.String() != "", req.WalletPurchase, price.BillingCycleDays); err != nil {
		log.WithError(err).WithField("payment_id", sourceId).Error("failed to grant entitlements after payment")
		// Don't fail - payment record was created successfully
	} else if product.EntitlementsSpec != nil {
		for entName := range product.EntitlementsSpec {
			grantedEntitlements = append(grantedEntitlements, entName)
		}
	}

	// Determine delayed start
	var delayedStart *time.Time
	if coverage.HasCoverage && coverage.EndDate != nil {
		delayedStart = coverage.EndDate
	}

	log.WithFields(log.Fields{
		"payment_id":     paymentID,
		"user_id":        req.UserID,
		"price_id":       req.PriceID,
		"product_id":     product.ID,
		"processor":      req.Processor,
		"transaction_id": req.TransactionID,
		"entitlements":   grantedEntitlements,
		"delayed_start":  delayedStart,
		"eligibility":    eligibility.Status,
	}).Info("registered purchase") // one-time

	return &RegisterPurchaseResponse{
		PaymentID:    paymentID,
		Entitlements: grantedEntitlements,
		DelayedStart: delayedStart,
		Eligibility:  eligibility.Status,
	}, nil
}

// processUpgrade handles tier upgrades with proration
// Upgrade = user moving to a higher tier (higher TierRank)
// Behavior: Immediate switch, charge prorated difference for remaining days
func (s *CheckoutService) processUpgrade(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	newPrice *models.Price,
	newProduct *models.Product,
	existingSub *models.Subscription,
	processor string,
) (*CheckoutResponse, error) {
	now := s.now()

	// CCBill handles upgrades via their own Package Upgrade flow
	if processor == "ccbill" {
		return s.processCCBillUpgrade(ctx, user, newPrice, existingSub)
	}

	// Solana doesn't support subscriptions
	if processor == "solana" {
		return nil, errors.New("solana does not support subscription upgrades")
	}

	// Only NMI-backed processors support programmatic upgrades
	if processor != "mobius" {
		return nil, fmt.Errorf("unsupported processor for upgrades: %s", processor)
	}

	// Get idempotency key (client-provided or generated)
	const idempOp = "nmi_upgrade"
	idempotencyKey := s.getUpgradeIdempotencyKey(req, user.ID, existingSub.ID, newPrice.ID)

	// Check idempotency - have we already processed this upgrade?
	idempRec, alreadyExists, err := s.IdempotencyService.Begin(ctx, idempOp, idempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("idempotency check failed: %w", err)
	}

	if alreadyExists {
		switch idempRec.Status {
		case IdempotencyStatusSuccess:
			// Return cached result
			var cached upgradeIdempotencyResult
			if err := json.Unmarshal(idempRec.Result, &cached); err != nil {
				log.WithError(err).Warn("failed to unmarshal cached upgrade result")
				return &CheckoutResponse{
					Status:        "success",
					Action:        "upgrade",
					Message:       "Upgrade already completed",
					TransactionID: cached.ProrationTransactionID,
				}, nil
			}
			subID, _ := uuid.Parse(cached.SubscriptionID)
			return &CheckoutResponse{
				Status:         "success",
				Action:         "upgrade",
				Message:        cached.Message,
				SubscriptionID: &subID,
				TransactionID:  cached.ProrationTransactionID,
			}, nil
		case IdempotencyStatusPending:
			return nil, errors.New("upgrade already in progress, please wait")
		case IdempotencyStatusFailed:
			return nil, errors.New("previous upgrade attempt failed, please try again")
		}
	}

	// Validate existing subscription has required data
	if existingSub.Price == nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, errors.New("existing subscription missing price data"))
		return nil, errors.New("existing subscription missing price data")
	}
	oldPrice := existingSub.Price

	// Calculate proration
	prorationAmount, daysRemaining, cycleDays := s.CalculateProration(
		oldPrice.Amount,
		newPrice.Amount,
		existingSub.CurrentPeriodEndsAt,
		oldPrice.BillingCycleDays,
		now,
	)

	log.WithFields(log.Fields{
		"user_id":          user.ID,
		"old_price":        oldPrice.Amount,
		"new_price":        newPrice.Amount,
		"days_remaining":   daysRemaining,
		"cycle_days":       cycleDays,
		"proration_amount": prorationAmount,
	}).Info("calculating upgrade proration")

	// Get NMI client
	_, provider, hasNMI := newPrice.GetNMIConfig()
	if !hasNMI {
		err := fmt.Errorf("new price %s is missing NMI plan configuration", newPrice.ID)
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	client, ok := s.NMIClients[provider]
	if !ok {
		err := fmt.Errorf("NMI provider '%s' is not configured", provider)
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// Get or create vault
	customerVaultID, createdPaymentMethod, err := s.resolveVault(ctx, req, user, provider)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// Step 1: Charge prorated difference (if positive)
	var prorationTransactionID string
	if prorationAmount > 0 {
		saleResp, err := client.RunSale(nmi.SaleParams{
			CustomerVaultID:  customerVaultID,
			Amount:           prorationAmount,
			Currency:         newPrice.Currency,
			OrderDescription: fmt.Sprintf("Upgrade proration: %s to %s", oldPrice.DisplayName, newPrice.DisplayName),
			OrderID:          fmt.Sprintf("upgrade-%s-%s", existingSub.ID.String()[:8], uuid.New().String()[:8]),
		})
		if err != nil {
			// Cleanup vault if we created it
			if createdPaymentMethod != nil && s.VaultService != nil {
				_ = s.VaultService.DeleteVault(ctx, createdPaymentMethod)
			}
			prorationErr := fmt.Errorf("failed to charge proration: %w", err)
			_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, prorationErr)
			return nil, prorationErr
		}
		prorationTransactionID = saleResp.TransactionID

		log.WithFields(log.Fields{
			"user_id":        user.ID,
			"transaction_id": prorationTransactionID,
			"amount":         prorationAmount,
		}).Info("charged upgrade proration")
	}

	// Step 2: Cancel old subscription at NMI
	if err := s.cancelNMISubscription(ctx, existingSub, provider); err != nil {
		log.WithError(err).WithField("subscription_id", existingSub.ID).
			Warn("failed to cancel old NMI subscription during upgrade, continuing anyway")
		// Don't fail the upgrade - the old sub will just not renew
	}

	// Step 3: Create new subscription at NMI
	newSubscriptionID := uuid.New()
	nmiPlanID, _, _ := newPrice.GetNMIConfig()

	params := nmi.RecurringPaymentData{
		CardUserData: nmi.CardUserData{
			FirstName: s.resolveFirstName(req, user),
			LastName:  s.resolveLastName(req),
			Address1:  s.defaultIfEmpty(req.Address1, "N/A"),
			City:      s.defaultIfEmpty(req.City, "N/A"),
			State:     s.defaultIfEmpty(req.State, "N/A"),
			Zip:       s.defaultIfEmpty(req.Zip, "00000"),
			Country:   s.defaultIfEmpty(req.Country, "US"),
		},
		PlanID:          nmiPlanID,
		CustomerVaultID: customerVaultID,
		Amount:          float64(newPrice.Amount) / 100.0,
		Currency:        newPrice.Currency,
		Email:           req.Email,
		OrderID:         newSubscriptionID.String(),
		PONumber:        newSubscriptionID.String(),
		CustomerID:      user.ID,
		// Start date = when current period ends (so they're not double-charged)
		StartDate: existingSub.CurrentPeriodEndsAt.Format("20060102"),
	}

	resp, err := client.AddRecurringSubscription(params)
	if err != nil {
		subErr := fmt.Errorf("failed to create upgraded subscription: %w", err)
		var nmiErr *nmi.CustomerVaultError
		if errors.As(err, &nmiErr) {
			subErr = &VaultError{
				Err:            subErr,
				LocalizationID: nmiErr.LocalizationID,
				Message:        subErr.Error(),
			}
		}
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, subErr)
		return nil, subErr
	}

	// Step 4: Update local database
	// Cancel old subscription
	cancelType := models.CancelType("upgrade")
	existingSub.Status = models.StatusCancelled
	existingSub.CancelledAt = &now
	existingSub.CancelType = &cancelType
	existingSub.CancelFeedback = nil
	if err := s.SubscriptionService.Update(ctx, existingSub); err != nil {
		log.WithError(err).WithField("subscription_id", existingSub.ID).
			Error("failed to mark old subscription as cancelled during upgrade")
	}

	// Create new subscription record
	var emailPtr *string
	if req.Email != "" {
		emailPtr = &req.Email
	}

	newSubscription := &models.Subscription{
		ID:                      newSubscriptionID,
		UserID:                  user.ID,
		ProductID:               newPrice.ProductID,
		PriceID:                 newPrice.ID,
		ProcessorSubscriptionID: resp.SubscriptionID,
		Status:                  models.StatusActive, // Active immediately since user paid proration
		Processor:               models.ProcessorMobius,
		UserEmail:               emailPtr,
		StartedAt:               now,
		CurrentPeriodStartsAt:   existingSub.CurrentPeriodStartsAt,
		CurrentPeriodEndsAt:     existingSub.CurrentPeriodEndsAt, // Keep same period end
	}

	if createdPaymentMethod != nil {
		newSubscription.PaymentMethodID = &createdPaymentMethod.ID
	} else if req.PaymentMethodID != "" {
		if pmID, err := api.ParsePaymentMethodID(req.PaymentMethodID); err == nil {
			newSubscription.PaymentMethodID = &pmID
		} else {
			log.WithError(err).Warn("failed to parse payment_method_id while scheduling upgrade subscription")
		}
	}

	if err := s.SubscriptionService.Create(ctx, newSubscription); err != nil {
		saveErr := fmt.Errorf("failed to save upgraded subscription: %w", err)
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, saveErr)
		return nil, saveErr
	}

	// Step 5: Update entitlements immediately (grant new tier entitlements)
	if s.EntitlementService != nil && newProduct.EntitlementsSpec != nil {
		for entitlementName, durationDays := range newProduct.EntitlementsSpec {
			notBefore := now
			var params PushNewEntitlementParams
			if durationDays != nil && *durationDays > 0 {
				d := time.Duration(*durationDays) * 24 * time.Hour
				params = PushNewEntitlementParams{
					UserID:      user.ID,
					Entitlement: entitlementName,
					NotBefore:   &notBefore,
					Duration:    &d,
					SourceType:  models.EntitlementSourceSubscription,
					SourceID:    newSubscriptionID,
				}
			} else {
				params = PushNewEntitlementParams{
					UserID:      user.ID,
					Entitlement: entitlementName,
					NotBefore:   &notBefore,
					Indefinite:  true,
					SourceType:  models.EntitlementSourceSubscription,
					SourceID:    newSubscriptionID,
				}
			}

			_, err := s.EntitlementService.PushNewEntitlement(ctx, params)
			if err != nil {
				log.WithError(err).WithFields(log.Fields{
					"user_id":         user.ID,
					"entitlement":     entitlementName,
					"subscription_id": newSubscriptionID,
				}).Error("failed to grant upgraded entitlement")
			}
		}
	}

	// Mark idempotency request as complete
	successMessage := fmt.Sprintf("Upgraded to %s. Prorated charge: %s", newProduct.DisplayName, moneyutil.FormatUSD(prorationAmount))
	cachedResult, _ := json.Marshal(upgradeIdempotencyResult{
		SubscriptionID:         newSubscriptionID.String(),
		ProrationTransactionID: prorationTransactionID,
		Message:                successMessage,
	})
	_ = s.IdempotencyService.Complete(ctx, idempOp, idempotencyKey, cachedResult)

	return &CheckoutResponse{
		Status:         "success",
		Action:         "upgrade",
		Message:        successMessage,
		SubscriptionID: &newSubscriptionID,
		TransactionID:  prorationTransactionID,
	}, nil
}

// processDowngrade handles tier downgrades (scheduled for end of period)
// Downgrade = user moving to a lower tier (lower TierRank)
// Behavior: Keep current tier until period ends, then switch to new tier at next renewal
func (s *CheckoutService) processDowngrade(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	newPrice *models.Price,
	newProduct *models.Product,
	existingSub *models.Subscription,
	processor string,
) (*CheckoutResponse, error) {
	// CCBill handles downgrades via their own flow
	if processor == "ccbill" {
		return &CheckoutResponse{
			Status:  "blocked",
			Message: "CCBill subscription downgrades are not supported. Please cancel your current subscription and wait for it to expire, then subscribe to the lower tier.",
		}, nil
	}

	// Solana doesn't support subscriptions
	if processor == "solana" {
		return nil, errors.New("solana does not support subscription downgrades")
	}

	// Only NMI-backed processors support programmatic downgrades
	if processor != "mobius" {
		return nil, fmt.Errorf("unsupported processor for downgrades: %s", processor)
	}

	// Validate the new price has NMI configuration
	nmiPlanID, _, hasNMI := newPrice.GetNMIConfig()
	if !hasNMI || nmiPlanID == "" {
		return nil, fmt.Errorf("new price %s is missing NMI plan configuration", newPrice.ID)
	}

	// Check if there's already a scheduled downgrade
	if existingSub.ScheduledPriceID != nil {
		return &CheckoutResponse{
			Status:  "blocked",
			Message: "You already have a tier change scheduled. Please wait for the current period to end or cancel the scheduled change first.",
		}, nil
	}

	// Schedule the downgrade for end of current period
	// The actual price switch happens in the renewal webhook handler
	existingSub.ScheduledPriceID = &newPrice.ID

	if err := s.SubscriptionService.Update(ctx, existingSub); err != nil {
		return nil, fmt.Errorf("failed to schedule downgrade: %w", err)
	}

	effectiveDate := "the end of your current billing period"
	if existingSub.CurrentPeriodEndsAt != nil {
		effectiveDate = existingSub.CurrentPeriodEndsAt.Format("January 2, 2006")
	}

	log.WithFields(log.Fields{
		"user_id":            user.ID,
		"subscription_id":    existingSub.ID,
		"current_price_id":   existingSub.PriceID,
		"scheduled_price_id": newPrice.ID,
		"effective_date":     effectiveDate,
	}).Info("scheduled downgrade for end of period")

	return &CheckoutResponse{
		Status:         "success",
		Action:         "downgrade",
		Message:        fmt.Sprintf("Downgrade to %s scheduled. Your current plan will remain active until %s.", newProduct.DisplayName, effectiveDate),
		SubscriptionID: &existingSub.ID,
		DelayedStart:   existingSub.CurrentPeriodEndsAt,
	}, nil
}

// CalculateProration calculates the prorated amount for an upgrade
// Returns: prorationAmount (in cents), daysRemaining, cycleDays
func (s *CheckoutService) CalculateProration(
	oldPriceAmount int64,
	newPriceAmount int64,
	periodEndsAt *time.Time,
	billingCycleDays *int,
	now time.Time,
) (int64, int, int) {
	// Default to 30-day cycle if not specified
	cycleDays := 30
	if billingCycleDays != nil && *billingCycleDays > 0 {
		cycleDays = *billingCycleDays
	}

	// Calculate days remaining in current period
	daysRemaining := 0
	if periodEndsAt != nil && periodEndsAt.After(now) {
		hoursRemaining := periodEndsAt.Sub(now).Hours()
		daysRemaining = int(hoursRemaining / 24)
		if daysRemaining < 0 {
			daysRemaining = 0
		}
	}

	// Proration = (newPrice - oldPrice) * (daysRemaining / cycleDays)
	priceDiff := newPriceAmount - oldPriceAmount
	if priceDiff <= 0 {
		// This is a downgrade, not an upgrade - no proration charge
		return 0, daysRemaining, cycleDays
	}

	// Calculate prorated amount
	// Use integer math to avoid floating point issues: (diff * daysRemaining) / cycleDays
	prorationAmount := (priceDiff * int64(daysRemaining)) / int64(cycleDays)

	return prorationAmount, daysRemaining, cycleDays
}

// cancelNMISubscription cancels a subscription at NMI
func (s *CheckoutService) cancelNMISubscription(ctx context.Context, sub *models.Subscription, provider string) error {
	client, ok := s.NMIClients[provider]
	if !ok {
		return fmt.Errorf("NMI provider '%s' is not configured", provider)
	}

	return client.DeleteRecurringSubscription(sub.ProcessorSubscriptionID)
}

// TierChange processes a subscription tier change (upgrade or downgrade).
// This is the unified entry point that routes to processor-specific implementations.
func (s *CheckoutService) TierChange(ctx context.Context, req *TierChangeRequest, user *UserIdentity) (*TierChangeResponse, error) {
	// 1. Parse and validate price
	priceID, err := api.ParsePriceID(req.PriceID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "invalid price_id"}
	}

	newPrice, err := s.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "price not found"}
	}
	if !newPrice.IsActive {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "price is not available"}
	}

	newProduct, err := s.ProductService.GetByID(ctx, newPrice.ProductID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "product not found"}
	}
	if !newProduct.IsActive {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "product is not available"}
	}

	// 2. Get subscription (by ID if provided, otherwise active subscription)
	var existingSub *models.Subscription
	if req.SubscriptionID != uuid.Nil {
		existingSub, err = s.SubscriptionService.GetByID(ctx, req.SubscriptionID)
		if err != nil {
			return nil, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "subscription not found"}
		}
		// Verify ownership
		if existingSub.UserID != user.ID {
			return nil, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "subscription not found"}
		}
	} else {
		existingSub, err = s.SubscriptionService.GetActiveSubscription(ctx, user.ID)
		if err != nil {
			return nil, ErrTierChangeNoSubscription
		}
	}

	// 3. Load current price and product
	currentPrice, err := s.PriceService.GetByID(ctx, existingSub.PriceID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "current price not found"}
	}
	existingSub.Price = currentPrice // Attach for downstream use

	currentProduct, err := s.ProductService.GetByID(ctx, currentPrice.ProductID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "current product not found"}
	}

	// 4. Validate tier group compatibility
	if currentProduct.ID == newProduct.ID {
		return nil, ErrTierChangeSameProduct
	}
	if currentProduct.TierGroup != nil && newProduct.TierGroup != nil {
		if strings.TrimSpace(*currentProduct.TierGroup) != strings.TrimSpace(*newProduct.TierGroup) {
			return nil, ErrTierChangeDifferentGroup
		}
	}

	// 5. Determine action (upgrade vs downgrade)
	action := "upgrade"
	if newProduct.TierRank < currentProduct.TierRank {
		action = "downgrade"
	}

	// 6. Route to processor-specific handler based on config type detection
	// This allows adding new NMI providers via config without code changes
	processor := string(existingSub.Processor)

	switch {
	case processor == "stripe":
		return s.processTierChangeStripe(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	case processors.IsNMIBacked(processor):
		return s.processTierChangeMobius(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	case processor == "ccbill":
		return s.processTierChangeCCBill(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	case processor == "solana":
		return nil, &TierChangeError{
			HTTPStatus: http.StatusBadRequest,
			Message:    "Solana subscriptions do not support tier changes",
		}
	default:
		return nil, &TierChangeError{
			HTTPStatus: http.StatusBadRequest,
			Message:    fmt.Sprintf("unsupported processor: %s", processor),
		}
	}
}

// processTierChangeStripe handles Stripe subscription tier changes.
// Both upgrades and downgrades are processed immediately via Stripe's API.
func (s *CheckoutService) processTierChangeStripe(
	ctx context.Context,
	req *TierChangeRequest,
	user *UserIdentity,
	newPrice *models.Price,
	newProduct *models.Product,
	existingSub *models.Subscription,
	currentProduct *models.Product,
	action string,
) (*TierChangeResponse, error) {
	// Validate Stripe configuration
	stripePriceID, ok := newPrice.GetStripeConfig()
	if !ok || strings.TrimSpace(stripePriceID) == "" {
		return nil, &TierChangeError{
			HTTPStatus: http.StatusBadRequest,
			Message:    "target price not configured for Stripe",
		}
	}
	if strings.TrimSpace(existingSub.ProcessorSubscriptionID) == "" {
		return nil, &TierChangeError{
			HTTPStatus: http.StatusBadRequest,
			Message:    "subscription missing Stripe reference",
		}
	}

	// Configure proration based on action
	proration := "create_prorations"
	billingAnchor := "now"
	if action == "downgrade" {
		proration = "none"
		billingAnchor = "unchanged"
	}

	// Call Stripe API
	stripeService := &StripeSubscriptionService{Config: s.Config}
	itemID, err := stripeService.GetSubscriptionItemID(ctx, existingSub.ProcessorSubscriptionID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
	}
	if err := stripeService.UpdateSubscriptionPrice(ctx, existingSub.ProcessorSubscriptionID, itemID, stripePriceID, proration, billingAnchor); err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
	}

	// Update local subscription record
	existingSub.PriceID = newPrice.ID
	existingSub.ProductID = newPrice.ProductID
	existingSub.ScheduledPriceID = nil
	if err := s.SubscriptionService.Update(ctx, existingSub); err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "failed to update subscription"}
	}

	subID := api.FormatSubscriptionID(existingSub.ID)
	msg := "Plan updated"
	if action == "downgrade" {
		msg = "Plan downgraded"
	}

	return &TierChangeResponse{
		Object:         "tier_change",
		Status:         "succeeded",
		Mode:           "tier_change",
		Action:         action,
		PriceID:        api.FormatPriceID(newPrice.ID),
		Payment:        CheckoutSessionPaymentResponse{Processor: "stripe"},
		SubscriptionID: &subID,
		Message:        msg,
	}, nil
}

// processTierChangeMobius handles Mobius/NMI subscription tier changes.
// Upgrades: immediate proration charge + new subscription
// Downgrades: scheduled for end of billing period
func (s *CheckoutService) processTierChangeMobius(
	ctx context.Context,
	req *TierChangeRequest,
	user *UserIdentity,
	newPrice *models.Price,
	newProduct *models.Product,
	existingSub *models.Subscription,
	currentProduct *models.Product,
	action string,
) (*TierChangeResponse, error) {
	// Create a synthetic CheckoutRequest for reuse of existing upgrade/downgrade logic
	checkoutReq := &CheckoutRequest{
		PriceID:        req.PriceID,
		Processor:      string(existingSub.Processor),
		IdempotencyKey: req.IdempotencyKey,
	}

	// Route to existing methods which handle the heavy lifting
	var checkoutResp *CheckoutResponse
	var err error

	if action == "upgrade" {
		checkoutResp, err = s.processUpgrade(ctx, checkoutReq, user, newPrice, newProduct, existingSub, string(existingSub.Processor))
	} else {
		checkoutResp, err = s.processDowngrade(ctx, checkoutReq, user, newPrice, newProduct, existingSub, string(existingSub.Processor))
	}

	if err != nil {
		return nil, err
	}

	// Map CheckoutResponse to TierChangeResponse
	return s.mapCheckoutToTierChangeResponse(checkoutResp, newPrice, action), nil
}

// processTierChangeCCBill handles CCBill subscription tier changes.
// Upgrades: returns redirect URL to CCBill upgrade FlexForm
// Downgrades: blocked (CCBill doesn't support programmatic downgrades)
func (s *CheckoutService) processTierChangeCCBill(
	ctx context.Context,
	req *TierChangeRequest,
	user *UserIdentity,
	newPrice *models.Price,
	newProduct *models.Product,
	existingSub *models.Subscription,
	currentProduct *models.Product,
	action string,
) (*TierChangeResponse, error) {
	if action == "downgrade" {
		return &TierChangeResponse{
			Object:  "tier_change",
			Status:  "blocked",
			Mode:    "tier_change",
			Action:  action,
			PriceID: api.FormatPriceID(newPrice.ID),
			Payment: CheckoutSessionPaymentResponse{Processor: "ccbill"},
			Message: "CCBill subscription downgrades are not supported. Please cancel your current subscription and wait for it to expire, then subscribe to the lower tier.",
		}, nil
	}

	// Use existing CCBill upgrade logic
	checkoutResp, err := s.processCCBillUpgrade(ctx, user, newPrice, existingSub)
	if err != nil {
		return nil, err
	}

	// Map to TierChangeResponse
	subID := api.FormatSubscriptionID(existingSub.ID)
	resp := &TierChangeResponse{
		Object:         "tier_change",
		Status:         "requires_action",
		Mode:           "tier_change",
		Action:         action,
		PriceID:        api.FormatPriceID(newPrice.ID),
		SubscriptionID: &subID,
		Payment: CheckoutSessionPaymentResponse{
			Processor:   "ccbill",
			RedirectURL: checkoutResp.RedirectURL,
		},
		Message: "Redirect to CCBill to complete upgrade",
	}

	// Build NextAction for redirect
	if checkoutResp.RedirectURL != "" {
		resp.NextAction = &CheckoutSessionNextAction{
			Type: "redirect_to_url",
			RedirectToURL: &CheckoutSessionRedirectToURL{
				URL: checkoutResp.RedirectURL,
			},
		}
	}

	return resp, nil
}

// mapCheckoutToTierChangeResponse converts a CheckoutResponse to TierChangeResponse
func (s *CheckoutService) mapCheckoutToTierChangeResponse(resp *CheckoutResponse, newPrice *models.Price, action string) *TierChangeResponse {
	tierResp := &TierChangeResponse{
		Object:  "tier_change",
		Mode:    "tier_change",
		Action:  action,
		PriceID: api.FormatPriceID(newPrice.ID),
		Payment: CheckoutSessionPaymentResponse{
			TransactionID: resp.TransactionID,
		},
		Message:      resp.Message,
		DelayedStart: resp.DelayedStart,
	}

	// Map status
	switch resp.Status {
	case "success":
		tierResp.Status = "succeeded"
	case "blocked":
		tierResp.Status = "blocked"
	case "redirect_required":
		tierResp.Status = "requires_action"
	default:
		tierResp.Status = resp.Status
	}

	// Map subscription ID
	if resp.SubscriptionID != nil {
		subID := api.FormatSubscriptionID(*resp.SubscriptionID)
		tierResp.SubscriptionID = &subID
	}

	// Map redirect
	if resp.RedirectURL != "" {
		tierResp.Payment.RedirectURL = resp.RedirectURL
		tierResp.NextAction = &CheckoutSessionNextAction{
			Type: "redirect_to_url",
			RedirectToURL: &CheckoutSessionRedirectToURL{
				URL: resp.RedirectURL,
			},
		}
	}

	return tierResp
}
