package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/modules/vault"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/identity"
	log "github.com/sirupsen/logrus"
)

// TierChangeResponse represents the response from a tier change operation.
// This reuses the CheckoutSessionResponse envelope pattern for API consistency.
type TierChangeResponse struct {
	Object         string                         `json:"object"`                    // "tier_change"
	ID             string                         `json:"id,omitempty"`              // Operation ID for tracking
	Status         string                         `json:"status"`                    // succeeded, requires_action, blocked
	Mode           string                         `json:"mode"`                      // "tier_change"
	Action         string                         `json:"action,omitempty"`          // upgrade, downgrade
	PriceID        string                         `json:"price_id"`                  // Target price ID
	URL            string                         `json:"url,omitempty"`             // Hosted redirect URL when required
	Payment        CheckoutSessionPaymentResponse `json:"payment"`                   // Processor info
	SubscriptionID *string                        `json:"subscription_id,omitempty"` // Affected subscription
	NextAction     *CheckoutSessionNextAction     `json:"next_action,omitempty"`     // For redirects
	Message        string                         `json:"message,omitempty"`         // User-friendly message
	DelayedStart   *time.Time                     `json:"delayed_start,omitempty"`   // For scheduled downgrades
	// Money summary so the client can confirm/announce what actually happened.
	// AmountDueNow is what was charged immediately (0 for a scheduled downgrade);
	// NextChargeAmount/NextChargeDate describe the next renewal at the new price.
	// For Stripe upgrades AmountDueNow is the local Model B estimate (Stripe
	// finalizes the exact proration on its side), so treat it as approximate.
	Currency         string     `json:"currency,omitempty"`
	AmountDueNow     int64      `json:"amount_due_now"`
	NextChargeAmount int64      `json:"next_charge_amount"`
	NextChargeDate   *time.Time `json:"next_charge_date,omitempty"`
}

// TierChangePreviewResponse is the non-mutating dry-run of a tier change: it
// reports what a confirm WOULD charge now and at the next renewal, without
// touching Stripe or the local subscription. The frontend renders it as a
// "Right now: $X / On <date>: $Y" confirmation before calling change-tier.
type TierChangePreviewResponse struct {
	Object           string     `json:"object"` // "tier_change_preview"
	Action           string     `json:"action"` // upgrade | downgrade
	PriceID          string     `json:"price_id"`
	Processor        string     `json:"processor"`
	Currency         string     `json:"currency"`
	AmountDueNow     int64      `json:"amount_due_now"`     // cents charged immediately (0 for downgrade)
	NextChargeAmount int64      `json:"next_charge_amount"` // cents at next renewal (new plan price)
	NextChargeDate   *time.Time `json:"next_charge_date,omitempty"`
	Effective        string     `json:"effective"`   // "now" (upgrade) | "period_end" (downgrade)
	IsEstimate       bool       `json:"is_estimate"` // true when the processor finalizes the exact amount (Stripe upgrades)
	Message          string     `json:"message,omitempty"`
}

// CheckoutService handles unified checkout for subscriptions and one-time purchases
type CheckoutService struct {
	SubscriptionService  *subscriptions.SubscriptionService
	ProductService       *catalog.ProductService
	PriceService         *catalog.PriceService
	PaymentService       *payments.PaymentService
	EntitlementService   *entitlements.EntitlementService
	PurchaseService      *CheckoutPurchaseService
	VaultResolver        *CheckoutVaultService
	NMISaleService       *CheckoutNMISaleService
	PaymentMethodService *vault.PaymentMethodService
	VaultService         *vault.VaultService
	IdempotencyService   checkoutIdempotencyStore
	NMIClients           map[string]*nmi.NMIClient
	TenantSecrets        tenantSecretGetter
	// ProcessorCustomerService maps app users to processor customer ids so we
	// reuse a single Stripe customer per user (issue #212) and can record the
	// mapping at checkout time instead of relying solely on webhooks.
	ProcessorCustomerService *payments.ProcessorCustomerService
	// StripeService is used to resolve/create the Stripe customer and to run the
	// webhook-independent duplicate guard (issue #213).
	StripeService *subscriptions.StripeService
	clock         clockwork.Clock
	Config        *config.Config
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *CheckoutService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *CheckoutService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
	if s.PurchaseService != nil {
		s.PurchaseService.SetClock(s.clock)
	}
}

func (s *CheckoutService) Clock() clockwork.Clock {
	return s.clock
}

// NewCheckoutService creates a new CheckoutService
func NewCheckoutService(
	subscriptionService *subscriptions.SubscriptionService,
	productService *catalog.ProductService,
	priceService *catalog.PriceService,
	paymentService *payments.PaymentService,
	entitlementService *entitlements.EntitlementService,
	paymentMethodService *vault.PaymentMethodService,
	vaultService *vault.VaultService,
	idempotencyService checkoutIdempotencyStore,
	nmiClients map[string]*nmi.NMIClient,
	processorCustomerService *payments.ProcessorCustomerService,
	cfg *config.Config,
	clocks ...clockwork.Clock,
) *CheckoutService {
	clock := timeutil.FirstClock(clocks...)
	service := &CheckoutService{
		SubscriptionService:      subscriptionService,
		ProductService:           productService,
		PriceService:             priceService,
		PaymentService:           paymentService,
		EntitlementService:       entitlementService,
		PurchaseService:          NewCheckoutPurchaseService(priceService, productService, paymentService, entitlementService, subscriptionService, clock),
		VaultResolver:            NewCheckoutVaultService(paymentMethodService, vaultService),
		PaymentMethodService:     paymentMethodService,
		VaultService:             vaultService,
		IdempotencyService:       idempotencyService,
		NMIClients:               nmiClients,
		ProcessorCustomerService: processorCustomerService,
		StripeService:            &subscriptions.StripeService{Config: cfg},
		clock:                    clock,
		Config:                   cfg,
	}
	service.NMISaleService = NewCheckoutNMISaleService(
		service.PurchaseService,
		service.VaultResolver,
		vaultService,
		idempotencyService,
		nmiClients,
	)
	return service
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
	if s.PurchaseService == nil {
		return nil, errors.New("purchase service unavailable")
	}
	return s.PurchaseService.CheckPurchaseEligibility(ctx, userID, priceID)
}

// CheckSubscriptionConflict is the shared duplicate-billing guard (issue #269):
// it reports whether the user already holds a non-terminal subscription that
// blocks a new subscribe for this price/product (same exact price, or same
// tier-group at any tier). Callers must run it BEFORE charging or preparing any
// on-chain action and reject when Blocked.
func (s *CheckoutService) CheckSubscriptionConflict(ctx context.Context, userID string, price *models.Price, product *models.Product) (*SubscriptionConflict, error) {
	if s.PurchaseService == nil {
		return nil, errors.New("purchase service unavailable")
	}
	return s.PurchaseService.CheckSubscriptionConflict(ctx, userID, price, product)
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
	if !price.IsPurchasable() {
		return nil, errors.New("price is not available for purchase")
	}

	// Get product
	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found: %w", err)
	}
	if !product.IsPurchasable() {
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
		if err != nil && !repo.IsNotFound(err) {
			return nil, fmt.Errorf("failed to check tier group: %w", err)
		}

		if existingSub != nil {
			// User has an active subscription in the same tier group
			existingProduct := existingSub.Price.Product
			if existingProduct == nil {
				return nil, errors.New("failed to load existing product for tier comparison")
			}

			if existingProduct.ID == product.ID {
				// Same product - the user already has this exact plan. Recurring
				// tier subscriptions must not be bought twice; block it rather
				// than create a second parallel subscription in the tier group.
				return &CheckoutResponse{
					Status:  "blocked",
					Message: "You already have an active subscription to this plan",
				}, nil
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

		// Webhook-independent guard (issue #213): the local check above only sees
		// what webhooks have written. If a webhook was missed, the local DB can be
		// empty and the guard would let a second parallel subscription through.
		// For Stripe, additionally ask Stripe directly whether this customer
		// already holds an active/trialing subscription in the same tier group.
		if processor == "stripe" {
			blocked, err := s.stripeTierGroupConflict(ctx, user, *product.TierGroup)
			if err != nil {
				return nil, fmt.Errorf("failed to check stripe tier group: %w", err)
			}
			if blocked {
				return &CheckoutResponse{
					Status:  "blocked",
					Message: "You already have an active subscription in this tier",
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
	if s.PurchaseService == nil {
		return nil, errors.New("purchase service unavailable")
	}
	return s.PurchaseService.GetUserProductCoverage(ctx, userID, product)
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
		return s.processNMISubscription(ctx, req, user, price, product, coverage, processor)
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
		return s.processNMISale(ctx, req, user, price, product, coverage, processor)
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
	ccbillClient, err := s.resolveCCBillClient(ctx)
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
		ReservationID: req.CheckoutSessionID,
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
	ccbillClient, err := s.resolveCCBillClient(ctx)
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

// processNMISubscription handles NMI-backed subscription creation.
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
	processor string,
) (*CheckoutResponse, error) {
	provider := normalize.Lower(processor)
	if provider == "" {
		return nil, errors.New("processor is required")
	}
	nmiPlanID, err := requireNMIPlanForProcessor(price, provider)
	if err != nil {
		return nil, err
	}

	client, err := s.resolveNMIClient(ctx, provider)
	if err != nil {
		return nil, fmt.Errorf("NMI provider '%s' is not configured: %w", provider, err)
	}

	// Get idempotency key (client-provided or generated)
	const idempOp = "nmi_subscription"
	idempotencyKey := s.getIdempotencyKey(req, user.ID, price.ID, idempOp)
	orderID := nmiSubscriptionOrderID(idempotencyKey, req.Metadata)
	poNumber := orderID

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
			if recovered, recoverErr := s.recoverNMISubscriptionAttempt(ctx, req, user, price, product, coverage, provider, orderID, idempOp, idempotencyKey); recoverErr == nil && recovered != nil {
				return recovered, nil
			} else if recoverErr != nil && !repo.IsNotFound(recoverErr) {
				return nil, recoverErr
			}
			return nil, errors.New("previous subscription attempt failed, please try again")
		}
	}

	// Get or create vault (payment method)
	customerVaultID, createdPaymentMethod, err := s.VaultResolver.ResolveVault(ctx, req, user, provider)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// Determine start date for delayed start
	now := s.now().UTC()
	startDate, delayedStart := nmiSubscriptionStartDate(coverage, now)

	if recovered, recoverErr := s.recoverNMISubscriptionAttempt(ctx, req, user, price, product, coverage, provider, orderID, idempOp, idempotencyKey); recoverErr == nil && recovered != nil {
		return recovered, nil
	} else if recoverErr != nil && !repo.IsNotFound(recoverErr) {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, recoverErr)
		return nil, fmt.Errorf("recover NMI subscription attempt: %w", recoverErr)
	}

	// Build subscription ID
	subscriptionID := uuidutil.NewV7()
	var paymentMethodID *uuid.UUID
	if createdPaymentMethod != nil {
		paymentMethodID = &createdPaymentMethod.ID
	} else if req.PaymentMethodID != "" {
		if pmID, err := api.ParsePaymentMethodID(req.PaymentMethodID); err == nil {
			paymentMethodID = &pmID
		} else {
			log.WithError(err).Warn("failed to parse payment_method_id while preparing subscription attempt")
		}
	}

	attempt, err := s.PaymentService.ReserveProviderAttempt(ctx, &models.Payment{
		ID:              uuidutil.NewV7(),
		TenantSubjectID: identity.TenantSubjectIDFromString(user.ID).UUID(),
		PriceID:         price.ID,
		Processor:       models.Processor(provider),
		TransactionID:   nmiSubscriptionAttemptTransactionID(orderID),
		Amount:          price.Amount,
		ListAmount:      price.Amount,
		Currency:        price.Currency,
		Status:          payments.PaymentStatusPendingValue,
		Metadata:        nmiSubscriptionAttemptMetadata(idempotencyKey, orderID, "pending", "", "", subscriptionID, paymentMethodID, delayedStart, req.Metadata),
	})
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("reserve NMI subscription attempt: %w", err)
	}

	// Build NMI params
	params := nmi.RecurringPaymentData{
		CardUserData: nmi.CardUserData{
			FirstName: ResolveCheckoutFirstName(req, user),
			LastName:  ResolveCheckoutLastName(req),
			Address1:  DefaultIfEmpty(req.Address1, "N/A"),
			City:      DefaultIfEmpty(req.City, "N/A"),
			State:     DefaultIfEmpty(req.State, "N/A"),
			Zip:       DefaultIfEmpty(req.Zip, "00000"),
			Country:   DefaultIfEmpty(req.Country, "US"),
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
		_ = s.PaymentService.MarkFailed(ctx, attempt.ID)
		wrappedErr := fmt.Errorf("failed to create subscription: %w", err)
		var nmiErr *nmi.CustomerVaultError
		if errors.As(err, &nmiErr) {
			wrappedErr = &vault.VaultError{
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
	completedAttempt, err := s.PaymentService.CompleteProviderAttemptInPlace(ctx, attempt.ID, nmiSubscriptionAttemptMetadata(idempotencyKey, orderID, "completed", resp.SubscriptionID, resp.TransactionID, subscriptionID, paymentMethodID, delayedStart, req.Metadata))
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("subscription created but failed to record provider attempt: %w", err)
	}
	providerSubscriptionID := metadataString(completedAttempt.Metadata, "provider_subscription_id")
	if providerSubscriptionID == "" {
		providerSubscriptionID = resp.SubscriptionID
	}

	return s.completeNMISubscriptionRegistration(ctx, req, user, price, product, provider, subscriptionID, providerSubscriptionID, resp.TransactionID, delayedStart, orderID, paymentMethodID, idempOp, idempotencyKey)
}

func (s *CheckoutService) recoverNMISubscriptionAttempt(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, coverage *CoverageInfo, provider string, orderID string, idempOp string, idempotencyKey string) (*CheckoutResponse, error) {
	attempt, err := s.PaymentService.GetByMetadataValue(ctx, "nmi_subscription_order_id", orderID)
	if err != nil {
		return nil, err
	}
	attemptStatus := nmiSubscriptionAttemptStatusFromPayment(attempt)
	if attemptStatus != payments.PaymentStatusCompletedValue {
		if attemptStatus == payments.PaymentStatusPendingValue {
			return nil, errors.New("NMI subscription attempt is already pending, please wait")
		}
		return nil, errors.New("previous NMI subscription attempt failed, retry with a new idempotency key")
	}
	providerSubscriptionID := metadataString(attempt.Metadata, "provider_subscription_id")
	if providerSubscriptionID == "" {
		return nil, errors.New("completed NMI subscription attempt is missing provider subscription id")
	}
	transactionID := metadataString(attempt.Metadata, "provider_transaction_id")
	if transactionID == "" {
		transactionID = attempt.TransactionID
	}
	subscriptionID := uuidutil.NewV7()
	if rawID := metadataString(attempt.Metadata, "local_subscription_id"); rawID != "" {
		if parsedID, err := uuid.Parse(rawID); err == nil {
			subscriptionID = parsedID
		}
	}
	var paymentMethodID *uuid.UUID
	if rawID := metadataString(attempt.Metadata, "payment_method_id"); rawID != "" {
		if parsedID, err := uuid.Parse(rawID); err == nil {
			paymentMethodID = &parsedID
		}
	}
	delayedStart := nmiSubscriptionDelayedStartFromMetadata(attempt.Metadata)
	if delayedStart == nil {
		_, delayedStart = nmiSubscriptionStartDate(coverage, s.now().UTC())
	}
	return s.completeNMISubscriptionRegistration(ctx, req, user, price, product, provider, subscriptionID, providerSubscriptionID, transactionID, delayedStart, orderID, paymentMethodID, idempOp, idempotencyKey)
}

func (s *CheckoutService) completeNMISubscriptionRegistration(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, provider string, subscriptionID uuid.UUID, providerSubscriptionID string, transactionID string, delayedStart *time.Time, orderID string, paymentMethodID *uuid.UUID, idempOp string, idempotencyKey string) (*CheckoutResponse, error) {
	if existing, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, provider, providerSubscriptionID); err == nil {
		if delayedStart == nil && (existing.Status == models.StatusPending || existing.Status == models.StatusActive) {
			return s.activateImmediateNMISubscription(ctx, req, user, price, existing.ID, provider, providerSubscriptionID, transactionID, orderID, idempOp, idempotencyKey)
		}
		return s.nmiSubscriptionPendingResponse(ctx, existing.ID, transactionID, delayedStart, idempOp, idempotencyKey)
	} else if !repo.IsNotFound(err) {
		return nil, fmt.Errorf("load existing subscription: %w", err)
	}

	now := s.now().UTC()
	var emailPtr *string
	if req.Email != "" {
		emailPtr = &req.Email
	}
	subscription := &models.Subscription{
		ID:                       subscriptionID,
		TenantSubjectID:          identity.TenantSubjectIDFromString(user.ID).UUID(),
		ProductID:                price.ProductID,
		PriceID:                  price.ID,
		EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(product.EntitlementsSpec),
		CreditsSpecSnapshot:      models.CloneCreditsSpec(product.CreditsSpec),
		ProcessorSubscriptionID:  providerSubscriptionID,
		Status:                   models.StatusPending,
		Processor:                models.Processor(provider),
		UserEmail:                emailPtr,
		StartedAt:                *timePtr(now),
		PaymentMethodID:          paymentMethodID,
	}
	metadata := map[string]any{"order_id": orderID, "provider_transaction_id": transactionID}
	if delayedStart != nil {
		metadata["delayed_start"] = delayedStart.UTC().Format(time.RFC3339)
	}
	if req.Metadata != nil {
		if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
			metadata["e2e_run_id"] = runID
		}
	}
	if payload, err := json.Marshal(metadata); err == nil {
		subscription.Metadata = payload
	}

	if err := s.SubscriptionService.Create(ctx, subscription); err != nil {
		if errors.Is(err, subscriptions.ErrActiveSubscriptionExists) {
			if existing, loadErr := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, provider, providerSubscriptionID); loadErr == nil {
				if delayedStart == nil && (existing.Status == models.StatusPending || existing.Status == models.StatusActive) {
					return s.activateImmediateNMISubscription(ctx, req, user, price, existing.ID, provider, providerSubscriptionID, transactionID, orderID, idempOp, idempotencyKey)
				}
				return s.nmiSubscriptionPendingResponse(ctx, existing.ID, transactionID, delayedStart, idempOp, idempotencyKey)
			}
		}
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("failed to save subscription: %w", err)
	}
	if delayedStart == nil {
		return s.activateImmediateNMISubscription(ctx, req, user, price, subscriptionID, provider, providerSubscriptionID, transactionID, orderID, idempOp, idempotencyKey)
	}
	return s.nmiSubscriptionPendingResponse(ctx, subscriptionID, transactionID, delayedStart, idempOp, idempotencyKey)
}

func (s *CheckoutService) activateImmediateNMISubscription(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, subscriptionID uuid.UUID, provider string, providerSubscriptionID string, transactionID string, orderID string, idempOp string, idempotencyKey string) (*CheckoutResponse, error) {
	if s.SubscriptionService == nil {
		return nil, errors.New("subscription service unavailable")
	}
	if s.EntitlementService == nil {
		return nil, errors.New("entitlement service unavailable")
	}
	if s.PaymentService == nil {
		return nil, errors.New("payment service unavailable")
	}

	metadata := map[string]any{
		"order_id":                orderID,
		"provider_transaction_id": transactionID,
	}
	if req.Metadata != nil {
		if runID := strings.TrimSpace(req.Metadata["e2e_run_id"]); runID != "" {
			metadata["e2e_run_id"] = runID
		}
	}

	subscription, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("failed to load NMI subscription for activation: %w", err)
	}
	if subscription.Status != models.StatusActive || subscription.CurrentPeriodStartsAt == nil || subscription.CurrentPeriodEndsAt == nil {
		now := s.now().UTC()
		periodStart := now
		periodEnd := nmiSubscriptionPeriodEnd(now, price)
		subscription.Status = models.StatusActive
		subscription.CurrentPeriodStartsAt = &periodStart
		subscription.CurrentPeriodEndsAt = &periodEnd
		subscription.StartedAt = periodStart
		if subscription.ProcessorSubscriptionID == "" {
			subscription.ProcessorSubscriptionID = providerSubscriptionID
		}
		subscription.Processor = models.Processor(provider)
		if err := s.SubscriptionService.Update(ctx, subscription); err != nil {
			_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
			return nil, fmt.Errorf("failed to activate NMI subscription: %w", err)
		}
	}
	if err := s.grantImmediateNMISubscriptionEntitlements(ctx, user.ID, subscription); err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	if _, err := s.PaymentService.GetByTransactionID(ctx, models.Processor(provider), transactionID); err != nil {
		if !repo.IsNotFound(err) {
			_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
			return nil, fmt.Errorf("failed to check NMI subscription payment: %w", err)
		}
		now := s.now().UTC()
		payment := &models.Payment{
			ID:                       uuidutil.NewV7(),
			TenantSubjectID:          subscription.TenantSubjectID,
			PriceID:                  price.ID,
			SubscriptionID:           &subscription.ID,
			Processor:                models.Processor(provider),
			TransactionID:            transactionID,
			Amount:                   price.Amount,
			ListAmount:               price.Amount,
			Currency:                 price.Currency,
			Status:                   payments.PaymentStatusCompletedValue,
			Metadata:                 metadata,
			EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(subscription.EntitlementsSpecSnapshot),
			CreditsSpecSnapshot:      models.CloneCreditsSpec(subscription.CreditsSpecSnapshot),
			PurchasedAt:              now,
			CreatedAt:                now,
		}
		if err := s.PaymentService.Create(ctx, payment); err != nil {
			_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
			return nil, fmt.Errorf("failed to record NMI subscription payment: %w", err)
		}
	}
	return s.nmiSubscriptionSuccessResponse(ctx, subscriptionID, transactionID, idempOp, idempotencyKey)
}

func (s *CheckoutService) grantImmediateNMISubscriptionEntitlements(ctx context.Context, userID string, subscription *models.Subscription) error {
	if subscription == nil {
		return errors.New("subscription is required")
	}
	if subscription.CurrentPeriodStartsAt == nil || subscription.CurrentPeriodEndsAt == nil {
		return errors.New("active subscription period is required")
	}
	entitlementsSpec := subscription.EntitlementsSpecSnapshot
	if len(entitlementsSpec) == 0 {
		entitlementsSpec = map[string]*int{"premium": nil}
	}
	for entitlementName := range entitlementsSpec {
		exists, err := s.EntitlementService.ExistsBySource(ctx, models.EntitlementSourceSubscription, subscription.ID, entitlementName)
		if err != nil {
			return fmt.Errorf("failed entitlement check: %w", err)
		}
		if exists {
			continue
		}
		notBefore := subscription.CurrentPeriodStartsAt.UTC()
		endAt := subscription.CurrentPeriodEndsAt.UTC()
		if _, err := s.EntitlementService.PushNewEntitlement(ctx, entitlements.PushNewEntitlementParams{
			UserID:          userID,
			TenantSubjectID: subscription.TenantSubjectID,
			Entitlement:     entitlementName,
			NotBefore:       &notBefore,
			EndAt:           &endAt,
			SourceType:      models.EntitlementSourceSubscription,
			SourceID:        subscription.ID,
		}); err != nil {
			return fmt.Errorf("failed to grant entitlement %s: %w", entitlementName, err)
		}
	}
	return nil
}

func nmiSubscriptionPeriodEnd(start time.Time, price *models.Price) time.Time {
	if price != nil && price.BillingCycleDays != nil && *price.BillingCycleDays > 0 {
		return start.Add(time.Duration(*price.BillingCycleDays) * 24 * time.Hour)
	}
	return start.Add(30 * 24 * time.Hour)
}

func (s *CheckoutService) nmiSubscriptionPendingResponse(ctx context.Context, subscriptionID uuid.UUID, transactionID string, delayedStart *time.Time, idempOp string, idempotencyKey string) (*CheckoutResponse, error) {
	var delayedStartStr *string
	if delayedStart != nil {
		ds := delayedStart.Format(time.RFC3339)
		delayedStartStr = &ds
	}
	cachedResult, _ := json.Marshal(subscriptionIdempotencyResult{
		SubscriptionID: subscriptionID.String(),
		TransactionID:  transactionID,
		DelayedStart:   delayedStartStr,
	})
	_ = s.IdempotencyService.Complete(ctx, idempOp, idempotencyKey, cachedResult)

	message := "Subscription created successfully"
	if delayedStart != nil {
		message = fmt.Sprintf("Subscription scheduled to start on %s", delayedStart.Format("2006-01-02"))
	}
	return &CheckoutResponse{
		Status:         "pending",
		Action:         "new",
		Message:        message,
		SubscriptionID: &subscriptionID,
		TransactionID:  transactionID,
		DelayedStart:   delayedStart,
	}, nil
}

func (s *CheckoutService) nmiSubscriptionSuccessResponse(ctx context.Context, subscriptionID uuid.UUID, transactionID string, idempOp string, idempotencyKey string) (*CheckoutResponse, error) {
	cachedResult, _ := json.Marshal(subscriptionIdempotencyResult{
		SubscriptionID: subscriptionID.String(),
		TransactionID:  transactionID,
	})
	_ = s.IdempotencyService.Complete(ctx, idempOp, idempotencyKey, cachedResult)

	return &CheckoutResponse{
		Status:         "success",
		Action:         "new",
		Message:        "Subscription created successfully",
		SubscriptionID: &subscriptionID,
		TransactionID:  transactionID,
	}, nil
}

func nmiSubscriptionOrderID(idempotencyKey string, metadata map[string]string) string {
	orderID := nmiIdempotentOrderID("sub", idempotencyKey)
	if orderID == "" {
		orderID = uuid.New().String()
	}
	if runID := strings.TrimSpace(metadata["e2e_run_id"]); runID != "" {
		orderID = fmt.Sprintf("%s_e2e_%s", orderID, nmiOrderIDSuffix(runID))
	}
	return orderID
}

func nmiSubscriptionAttemptTransactionID(orderID string) string {
	return "nmi_sub_attempt:" + strings.TrimSpace(orderID)
}

func nmiSubscriptionStartDate(coverage *CoverageInfo, now time.Time) (string, *time.Time) {
	if coverage == nil || !coverage.HasCoverage || coverage.EndDate == nil || !coverage.EndDate.After(now) {
		return "", nil
	}
	startDate, startAt := buildNMIFutureStartDate(*coverage.EndDate, now)
	return startDate, &startAt
}

func nmiSubscriptionAttemptMetadata(idempotencyKey string, orderID string, status string, providerSubscriptionID string, transactionID string, subscriptionID uuid.UUID, paymentMethodID *uuid.UUID, delayedStart *time.Time, requestMetadata map[string]string) map[string]any {
	metadata := map[string]any{
		"checkout_idempotency_key":  strings.TrimSpace(idempotencyKey),
		"nmi_subscription_order_id": strings.TrimSpace(orderID),
		"nmi_attempt_status":        status,
		"local_subscription_id":     subscriptionID.String(),
	}
	if paymentMethodID != nil && *paymentMethodID != uuid.Nil {
		metadata["payment_method_id"] = paymentMethodID.String()
	}
	if delayedStart != nil {
		metadata["delayed_start"] = delayedStart.Format(time.RFC3339)
	}
	if providerSubscriptionID != "" {
		metadata["provider_subscription_id"] = providerSubscriptionID
	}
	if transactionID != "" {
		metadata["provider_transaction_id"] = transactionID
	}
	if runID := strings.TrimSpace(requestMetadata["e2e_run_id"]); runID != "" {
		metadata["e2e_run_id"] = runID
	}
	return metadata
}

func nmiSubscriptionDelayedStartFromMetadata(metadata map[string]any) *time.Time {
	raw := metadataString(metadata, "delayed_start")
	if raw == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	return &parsed
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	value, ok := metadata[key]
	if !ok {
		return ""
	}
	s, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

func nmiSubscriptionAttemptStatusFromPayment(attempt *models.Payment) string {
	if attempt == nil {
		return ""
	}
	if status := strings.ToLower(metadataString(attempt.Metadata, "nmi_attempt_status")); status != "" {
		return status
	}
	return strings.ToLower(strings.TrimSpace(attempt.Status))
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
	processor string,
) (*CheckoutResponse, error) {
	_ = coverage
	if s.NMISaleService == nil {
		return nil, errors.New("NMI sale service unavailable")
	}
	idempotencyKey := s.getIdempotencyKey(req, user.ID, price.ID, "nmi_sale")
	provider := normalize.Lower(processor)
	if provider == "" {
		return nil, errors.New("processor is required")
	}
	return s.NMISaleService.Process(ctx, req, user, price, product, idempotencyKey, provider)
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
	stripeProc, _, err := subscriptions.RequireStripeSecretKey(s.Config)
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

	// Resolve a single, durable Stripe customer for this user (issue #212) so we
	// stop minting a fresh customer on every checkout. The mapping is recorded
	// here, at checkout time, not only via webhook.
	customerID, err := s.resolveStripeCustomer(ctx, user)
	if err != nil {
		return nil, err
	}

	trialEnd := int64(0)
	if coverage != nil && coverage.HasCoverage && coverage.EndDate != nil && coverage.EndDate.After(s.now().Add(5*time.Minute)) {
		trialEnd = coverage.EndDate.Unix()
	}
	urlStr, err := s.createStripeCheckoutSession(ctx, stripeCheckoutParams{
		Mode:              "subscription",
		PriceID:           stripePriceID,
		SuccessURL:        successURL,
		CancelURL:         cancelURL,
		UserID:            user.ID,
		CustomerID:        customerID,
		CustomerEmail:     userEmail(user),
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
	stripeProc, _, err := subscriptions.RequireStripeSecretKey(s.Config)
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
		CustomerEmail:     userEmail(user),
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

// processorCustomerStore is the slice of ProcessorCustomerService used for
// Stripe customer resolution. Defined as an interface so the resolution logic
// is unit-testable without a database.
type processorCustomerStore interface {
	GetCustomerID(ctx context.Context, userID, processor string) (string, error)
	Upsert(ctx context.Context, userID, processor, customerID string) error
}

// stripeCustomerClient is the slice of StripeService used for customer
// resolution and the duplicate guard.
type stripeCustomerClient interface {
	CreateCustomer(ctx context.Context, email, appUserID string) (string, error)
	FindCustomerIDByAppUserID(ctx context.Context, appUserID string) (string, error)
	ListActiveSubscriptionsForCustomer(ctx context.Context, customerID string) ([]subscriptions.StripeSubscriptionSummary, error)
}

// stripePriceResolver maps a Stripe price id back to the local product's tier
// group. Backed by PriceService + ProductService at runtime.
type stripePriceResolver interface {
	GetByStripePriceID(ctx context.Context, stripePriceID string) (*models.Price, error)
}

type productResolver interface {
	GetByID(ctx context.Context, id uuid.UUID) (*models.Product, error)
}

func (s *CheckoutService) customerStore() processorCustomerStore {
	if s.ProcessorCustomerService == nil {
		return nil
	}
	return s.ProcessorCustomerService
}

func (s *CheckoutService) stripeClient() stripeCustomerClient {
	if s.StripeService == nil {
		return nil
	}
	return s.StripeService
}

// resolveStripeCustomer returns the durable Stripe customer id for a user. See
// resolveStripeCustomerWith for the resolution order; this is the production
// wiring.
func (s *CheckoutService) resolveStripeCustomer(ctx context.Context, user *UserIdentity) (string, error) {
	return resolveStripeCustomerWith(ctx, s.customerStore(), s.stripeClient(), user)
}

// resolveStripeCustomerWith returns the durable Stripe customer id for a user,
// resolving in priority order (issue #212):
//
//  1. local mapping (ProcessorCustomerService.GetCustomerID)
//  2. Stripe Customer Search on metadata[app_user_id]
//  3. create a fresh, idempotent Stripe customer
//
// Whenever a customer is resolved (or created), the local mapping is upserted so
// the link survives even if the corresponding webhook is missed. It returns ""
// (and no error) only when the dependencies are unavailable, in which case the
// caller falls back to the legacy customer_email behavior.
func resolveStripeCustomerWith(ctx context.Context, store processorCustomerStore, client stripeCustomerClient, user *UserIdentity) (string, error) {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return "", nil
	}
	if store == nil || client == nil {
		// Without these wired we cannot manage a durable customer; fall back to
		// the email-only path rather than failing the checkout.
		return "", nil
	}

	// 1. Local mapping.
	customerID, err := store.GetCustomerID(ctx, user.ID, "stripe")
	if err != nil && !repo.IsNotFound(err) {
		return "", fmt.Errorf("lookup stripe customer mapping: %w", err)
	}
	customerID = strings.TrimSpace(customerID)

	// 2. Stripe Customer Search by metadata.
	if customerID == "" {
		found, err := client.FindCustomerIDByAppUserID(ctx, user.ID)
		if err != nil {
			return "", fmt.Errorf("search stripe customer: %w", err)
		}
		customerID = strings.TrimSpace(found)
	}

	// 3. Create a new (idempotent) customer.
	if customerID == "" {
		created, err := client.CreateCustomer(ctx, userEmail(user), user.ID)
		if err != nil {
			return "", fmt.Errorf("create stripe customer: %w", err)
		}
		customerID = strings.TrimSpace(created)
	}

	if customerID == "" {
		return "", nil
	}

	// Record the mapping at checkout time, not only via webhook.
	if err := store.Upsert(ctx, user.ID, "stripe", customerID); err != nil {
		return "", fmt.Errorf("record stripe customer mapping: %w", err)
	}
	return customerID, nil
}

// stripeTierGroupConflict is the production wiring for the webhook-independent
// duplicate guard (issue #213).
func (s *CheckoutService) stripeTierGroupConflict(ctx context.Context, user *UserIdentity, tierGroup string) (bool, error) {
	var prices stripePriceResolver
	if s.PriceService != nil {
		prices = s.PriceService
	}
	var products productResolver
	if s.ProductService != nil {
		products = s.ProductService
	}
	return stripeTierGroupConflictWith(ctx, s.customerStore(), s.stripeClient(), prices, products, user, tierGroup)
}

// stripeTierGroupConflictWith reports whether the user already has an active or
// trialing Stripe subscription whose price maps to the requested tier group
// (issue #213). It consults Stripe directly so a missed webhook (which would
// leave the local DB empty) cannot allow a second parallel subscription. It
// never creates a customer: if no customer is mapped/found, there is by
// definition no Stripe-side subscription to conflict with.
func stripeTierGroupConflictWith(ctx context.Context, store processorCustomerStore, client stripeCustomerClient, prices stripePriceResolver, products productResolver, user *UserIdentity, tierGroup string) (bool, error) {
	tierGroup = strings.TrimSpace(tierGroup)
	if tierGroup == "" {
		return false, nil
	}
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return false, nil
	}
	if store == nil || client == nil || prices == nil || products == nil {
		return false, nil
	}

	// Find the customer without creating one: local mapping, then Stripe search.
	customerID, err := store.GetCustomerID(ctx, user.ID, "stripe")
	if err != nil && !repo.IsNotFound(err) {
		return false, fmt.Errorf("lookup stripe customer mapping: %w", err)
	}
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		found, err := client.FindCustomerIDByAppUserID(ctx, user.ID)
		if err != nil {
			return false, fmt.Errorf("search stripe customer: %w", err)
		}
		customerID = strings.TrimSpace(found)
	}
	if customerID == "" {
		return false, nil
	}

	subs, err := client.ListActiveSubscriptionsForCustomer(ctx, customerID)
	if err != nil {
		return false, fmt.Errorf("list stripe subscriptions: %w", err)
	}
	for _, sub := range subs {
		stripePriceID := strings.TrimSpace(sub.PriceID)
		if stripePriceID == "" {
			continue
		}
		price, err := prices.GetByStripePriceID(ctx, stripePriceID)
		if err != nil {
			if repo.IsNotFound(err) {
				// Unknown price (e.g. legacy/manual sub) — cannot map to a tier
				// group, so skip rather than block.
				continue
			}
			return false, fmt.Errorf("map stripe price %s: %w", stripePriceID, err)
		}
		if price == nil {
			continue
		}
		product, err := products.GetByID(ctx, price.ProductID)
		if err != nil {
			if repo.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("load product for stripe price %s: %w", stripePriceID, err)
		}
		if product == nil || product.TierGroup == nil {
			continue
		}
		if strings.TrimSpace(*product.TierGroup) == tierGroup {
			return true, nil
		}
	}
	return false, nil
}

// userEmail returns the caller's email when present, so Stripe Checkout can
// prefill it on the hosted page and route the receipt. Empty when unknown —
// Stripe collects it on the page in that case.
func userEmail(user *UserIdentity) string {
	if user == nil || user.Email == nil {
		return ""
	}
	return strings.TrimSpace(*user.Email)
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
	CustomerID        string // resolved Stripe customer (cus_...); takes precedence over CustomerEmail
	CustomerEmail     string
	InternalPriceID   string
	TrialEnd          int64
	CheckoutSessionID string
}

func (s *CheckoutService) createStripeCheckoutSession(ctx context.Context, params stripeCheckoutParams) (string, error) {
	stripeProc, _, err := subscriptions.RequireStripeSecretKey(s.Config)
	if err != nil {
		return "", err
	}
	values := url.Values{}
	values.Set("mode", params.Mode)
	values.Set("success_url", params.SuccessURL)
	values.Set("cancel_url", params.CancelURL)
	values.Set("client_reference_id", params.UserID)
	// Stripe Checkout: `customer` and `customer_email` are mutually exclusive.
	// Prefer the resolved customer so one app user maps to exactly one Stripe
	// customer (issue #212); fall back to customer_email only when unresolved.
	if customerID := strings.TrimSpace(params.CustomerID); customerID != "" {
		values.Set("customer", customerID)
	} else if email := strings.TrimSpace(params.CustomerEmail); email != "" {
		values.Set("customer_email", email)
	}
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

	client := stripeapi.Client(s.Config, 0)
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
// grantProductEntitlements grants entitlements from product spec after a one-time or subscription purchase

func timePtr(t time.Time) *time.Time {
	return &t
}

// RegisterPurchase records a confirmed one-time purchase and grants entitlements.
// This is the single source of truth for "user paid for product" logic.
//
// Called by:
//   - NMI-backed sale (after charging card)
//   - Solana poller (after detecting on-chain payment)
//   - CCBill webhook (after receiving payment confirmation)
//   - Admin API (for manual grants)
//
// It handles:
//  1. Creating the Payment record
//  2. Looking up Product from Price
//  3. Checking coverage for delayed start
//  4. Granting entitlements from Product.EntitlementsSpec
func (s *CheckoutService) RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error) {
	if s.PurchaseService == nil {
		return nil, errors.New("purchase service unavailable")
	}
	return s.PurchaseService.RegisterPurchase(ctx, req)
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
	if !processors.IsNMIBacked(processor) {
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

	// #268: Model B (reset-period) upgrade. Charge `newFull - oldUnused` NOW for
	// a FRESH full period, then rebill the full new price at now + cycle. The
	// billing cycle comes from the NEW price (the period being started), with a
	// fallback to the old price's cycle for legacy prices that omit it.
	billingCycleDays := newPrice.BillingCycleDays
	if billingCycleDays == nil || *billingCycleDays <= 0 {
		billingCycleDays = oldPrice.BillingCycleDays
	}
	prorationAmount, cycleDays := CalculateModelBUpgradeCharge(
		oldPrice.Amount,
		newPrice.Amount,
		existingSub.CurrentPeriodEndsAt,
		billingCycleDays,
		now,
	)

	log.WithFields(log.Fields{
		"user_id":          user.ID,
		"old_price":        oldPrice.Amount,
		"new_price":        newPrice.Amount,
		"cycle_days":       cycleDays,
		"proration_amount": prorationAmount, // Model B first charge (new_full - old_unused)
		"billing_model":    "B",
	}).Info("calculating Model-B upgrade first charge")

	provider := normalize.Lower(processor)
	if provider == "" {
		err := errors.New("processor is required for upgrades")
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}
	if existingSub.CurrentPeriodEndsAt == nil || existingSub.CurrentPeriodEndsAt.IsZero() {
		err := errors.New("existing subscription missing current period end")
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	nmiPlanID, err := requireNMIPlanForProcessor(newPrice, provider)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// #268: Model B resets the billing period to [now, now+cycle]. The immediate
	// first charge (RunSale below) pays for this fresh period, so the recurring
	// NMI subscription's first scheduled rebill must land at the NEW period end
	// (now + cycle), not the old period end.
	newPeriodStart := now
	newPeriodEnd := now.AddDate(0, 0, cycleDays)
	startDate, _ := buildNMIFutureStartDate(newPeriodEnd, now)

	client, err := s.resolveNMIClient(ctx, provider)
	if err != nil {
		err := fmt.Errorf("NMI provider '%s' is not configured: %w", provider, err)
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// Get or create vault
	customerVaultID, createdPaymentMethod, err := s.VaultResolver.ResolveVault(ctx, req, user, provider)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// Step 1: Create the successor subscription at NMI before charging/cancelling.
	newSubscriptionID := uuidutil.NewV7()

	params := nmi.RecurringPaymentData{
		CardUserData: nmi.CardUserData{
			FirstName: ResolveCheckoutFirstName(req, user),
			LastName:  ResolveCheckoutLastName(req),
			Address1:  DefaultIfEmpty(req.Address1, "N/A"),
			City:      DefaultIfEmpty(req.City, "N/A"),
			State:     DefaultIfEmpty(req.State, "N/A"),
			Zip:       DefaultIfEmpty(req.Zip, "00000"),
			Country:   DefaultIfEmpty(req.Country, "US"),
		},
		PlanID:          nmiPlanID,
		CustomerVaultID: customerVaultID,
		Amount:          float64(newPrice.Amount) / 100.0,
		Currency:        newPrice.Currency,
		Email:           req.Email,
		OrderID:         newSubscriptionID.String(),
		PONumber:        newSubscriptionID.String(),
		CustomerID:      user.ID,
		// Start date uses day precision and must be strictly in the future for NMI.
		StartDate: startDate,
	}

	resp, err := client.AddRecurringSubscription(params)
	if err != nil {
		subErr := fmt.Errorf("failed to create upgraded subscription: %w", err)
		var nmiErr *nmi.CustomerVaultError
		if errors.As(err, &nmiErr) {
			subErr = &vault.VaultError{
				Err:            subErr,
				LocalizationID: nmiErr.LocalizationID,
				Message:        subErr.Error(),
			}
		}
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, subErr)
		return nil, subErr
	}

	rollbackNewSubscription := func() {
		cleanupSub := &models.Subscription{ProcessorSubscriptionID: resp.SubscriptionID}
		if cancelErr := s.cancelNMISubscription(ctx, cleanupSub, provider); cancelErr != nil {
			log.WithError(cancelErr).WithFields(log.Fields{
				"subscription_id":           newSubscriptionID,
				"processor_subscription_id": resp.SubscriptionID,
				"processor":                 provider,
			}).Error("failed to rollback successor NMI subscription after upgrade error")
		}
	}

	// Step 2: Charge prorated difference (if positive).
	var prorationTransactionID string
	if prorationAmount > 0 {
		// Derive a stable OrderID from the idempotency key so a retried upgrade
		// reuses the same order reference at NMI, letting NMI's duplicate-
		// transaction detection prevent a double proration charge.
		prorationOrderID := "upg-" + shortHash(idempotencyKey)
		saleResp, err := client.RunSale(nmi.SaleParams{
			CustomerVaultID:  customerVaultID,
			Amount:           prorationAmount,
			Currency:         newPrice.Currency,
			OrderDescription: fmt.Sprintf("Upgrade proration: %s", newProduct.DisplayName),
			OrderID:          prorationOrderID,
		})
		if err != nil {
			rollbackNewSubscription()
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
			"processor":      provider,
		}).Info("charged upgrade proration")
	}

	// Step 3: Update local database.
	//
	// Ordering note (SEC-10): the new subscription row is created BEFORE the old
	// one is marked cancelled and BEFORE the old NMI subscription is cancelled.
	// If the create fails, the old subscription is still active both locally and
	// at NMI, so compensation only has to refund the proration and cancel the new
	// NMI subscription — no reactivation.
	//
	// Create new subscription record first.
	var emailPtr *string
	if req.Email != "" {
		emailPtr = &req.Email
	}

	newSubscription := &models.Subscription{
		ID:                       newSubscriptionID,
		TenantSubjectID:          identity.TenantSubjectIDFromString(user.ID).UUID(),
		ProductID:                newPrice.ProductID,
		PriceID:                  newPrice.ID,
		EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(newProduct.EntitlementsSpec),
		CreditsSpecSnapshot:      models.CloneCreditsSpec(newProduct.CreditsSpec),
		ProcessorSubscriptionID:  resp.SubscriptionID,
		Status:                   models.StatusActive, // Active immediately since user paid proration
		Processor:                models.Processor(provider),
		UserEmail:                emailPtr,
		StartedAt:                now,
		// #268: Model B resets the period — fresh full period [now, now+cycle].
		CurrentPeriodStartsAt: &newPeriodStart,
		CurrentPeriodEndsAt:   &newPeriodEnd,
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
		// Post-charge DB failure: the user was charged the proration and a new
		// subscription is live at NMI, but we cannot persist it locally. Compensate
		// by refunding the proration and cancelling the new NMI subscription so the
		// processor state matches the (unchanged) local state. The old subscription
		// is untouched (still active locally and at NMI), so nothing to reactivate.
		s.compensateFailedUpgrade(ctx, provider, prorationTransactionID, newSubscriptionID, resp.SubscriptionID, user.ID, &existingSub.ID, rollbackNewSubscription, saveErr)
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, saveErr)
		return nil, saveErr
	}

	// New local row committed: mark old subscription cancelled locally.
	cancelType := models.CancelType("upgrade")
	existingSub.Status = models.StatusCancelled
	existingSub.CancelledAt = &now
	existingSub.CancelType = &cancelType
	existingSub.CancelFeedback = nil
	existingSub.ClearRetrySchedule()
	if err := s.SubscriptionService.Update(ctx, existingSub); err != nil {
		log.WithError(err).WithField("subscription_id", existingSub.ID).
			Error("failed to mark old subscription as cancelled during upgrade")
	}

	// Step 4: Update entitlements immediately (grant new tier entitlements)
	if s.EntitlementService != nil && newProduct.EntitlementsSpec != nil {
		for entitlementName, durationDays := range newProduct.EntitlementsSpec {
			notBefore := now
			var params entitlements.PushNewEntitlementParams
			if durationDays != nil && *durationDays > 0 {
				d := time.Duration(*durationDays) * 24 * time.Hour
				params = entitlements.PushNewEntitlementParams{
					UserID:      user.ID,
					Entitlement: entitlementName,
					NotBefore:   &notBefore,
					Duration:    &d,
					SourceType:  models.EntitlementSourceSubscription,
					SourceID:    newSubscriptionID,
				}
			} else {
				params = entitlements.PushNewEntitlementParams{
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

	// Step 5: Cancel the old subscription at NMI now that local state is durably
	// consistent. Best-effort: if this fails the old subscription would keep
	// billing, so flag it for operator repair rather than silently dropping it.
	if err := s.cancelNMISubscription(ctx, existingSub, provider); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"subscription_id":           existingSub.ID,
			"processor_subscription_id": existingSub.ProcessorSubscriptionID,
			"processor":                 provider,
			"event":                     "upgrade_old_subscription_cancel_failed",
		}).Error("failed to cancel old NMI subscription after upgrade; manual intervention required to stop duplicate billing")
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

// shortHash returns a stable 16-hex-char digest of s, used to build
// deterministic processor order references from an idempotency key.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// compensateFailedUpgrade rolls back processor-side state after a post-charge DB
// failure during an NMI tier upgrade: it refunds the proration charge and cancels
// the newly created NMI subscription so the processor matches the unchanged local
// state. Each step is best-effort; any failure is logged at error level with a
// structured event so operators can finish the repair manually.
func (s *CheckoutService) compensateFailedUpgrade(
	ctx context.Context,
	provider string,
	prorationTransactionID string,
	newSubscriptionID uuid.UUID,
	newProcessorSubscriptionID string,
	userID string,
	oldSubscriptionID *uuid.UUID,
	rollbackNewSubscription func(),
	cause error,
) {
	logEntry := log.WithError(cause).WithFields(log.Fields{
		"user_id":                       userID,
		"new_subscription_id":           newSubscriptionID,
		"new_processor_subscription_id": newProcessorSubscriptionID,
		"proration_transaction_id":      prorationTransactionID,
		"processor":                     provider,
		"event":                         "upgrade_compensation",
	})
	if oldSubscriptionID != nil {
		logEntry = logEntry.WithField("old_subscription_id", *oldSubscriptionID)
	}
	logEntry.Warn("compensating failed NMI upgrade after post-charge DB error")

	// Refund the proration charge.
	if prorationTransactionID != "" {
		client, ok := s.NMIClients[provider]
		if !ok || client == nil {
			logEntry.Error("manual intervention required: NMI client unavailable to refund proration")
		} else if _, err := client.Refund(nmi.RefundParams{TransactionID: prorationTransactionID}); err != nil {
			logEntry.WithError(err).Error("manual intervention required: failed to refund proration during upgrade compensation")
		} else {
			logEntry.Warn("refunded proration during upgrade compensation")
		}
	}

	// Cancel the newly created NMI subscription.
	if rollbackNewSubscription != nil {
		rollbackNewSubscription()
	}
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
	if !processors.IsNMIBacked(processor) {
		return nil, fmt.Errorf("unsupported processor for downgrades: %s", processor)
	}

	provider := normalize.Lower(processor)
	if provider == "" {
		return nil, errors.New("processor is required for downgrades")
	}

	// Validate the new price has NMI configuration
	if _, err := requireNMIPlanForProcessor(newPrice, provider); err != nil {
		return nil, err
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

// CalculateModelBUpgradeCharge computes the immediate first charge for a
// "Model B" (reset-period) upgrade.
//
// Model B is the UNIVERSAL upgrade policy as of #268: every upgrade resets the
// billing period. The customer is charged `newFull - oldUnused` NOW for a FRESH
// full period, and then rebilled `newFull` at `now + cycle`.
//
//	oldUnused   = oldFull * (daysRemaining / cycleDays)   // integer math
//	firstCharge = newFull - oldUnused                     // clamped to >= 0
//
// where daysRemaining is the number of WHOLE days left in the current paid
// period (0 if the period has already ended or periodEndsAt is nil).
//
// Example: $20 -> $50, 2 days into a 30-day cycle => daysRemaining=28,
// oldUnused = 2000*28/30 = 1866c, firstCharge = 5000-1866 = 3134c. The new
// period becomes [now, now+30d] and the next bill is $50.
//
// Boundary behavior:
//   - 0 days remaining            => firstCharge = newFull
//   - full period remaining       => firstCharge = newFull - oldFull
//
// This helper is intentionally pure (no receiver state) so other processors
// (e.g. the Solana path in #267) can reuse the exact same math. cycleDays is
// returned so callers can advance the period end (now + cycleDays).
func CalculateModelBUpgradeCharge(
	oldFull int64,
	newFull int64,
	periodEndsAt *time.Time,
	billingCycleDays *int,
	now time.Time,
) (firstChargeCents int64, cycleDays int) {
	// Default to a 30-day cycle if not specified.
	cycleDays = 30
	if billingCycleDays != nil && *billingCycleDays > 0 {
		cycleDays = *billingCycleDays
	}

	// Whole days remaining in the current paid period.
	daysRemaining := 0
	if periodEndsAt != nil && periodEndsAt.After(now) {
		daysRemaining = int(periodEndsAt.Sub(now).Hours() / 24)
		if daysRemaining < 0 {
			daysRemaining = 0
		}
	}
	// Never credit more than a full cycle of unused time.
	if daysRemaining > cycleDays {
		daysRemaining = cycleDays
	}

	// Credit for the unused portion of the OLD plan (integer math to avoid
	// floating-point drift on cent amounts).
	oldUnused := (oldFull * int64(daysRemaining)) / int64(cycleDays)

	firstChargeCents = newFull - oldUnused
	if firstChargeCents < 0 {
		// Defensive clamp. For a genuine upgrade newFull > oldFull so this is
		// only reachable with bad inputs (e.g. a "downgrade" routed here).
		firstChargeCents = 0
	}
	return firstChargeCents, cycleDays
}

// CalculateProration calculates the prorated amount for a tier change.
//
// NOTE (#268): Model B (reset-period) is now the UNIVERSAL policy for
// UPGRADES — see CalculateModelBUpgradeCharge, which is what the NMI and
// Stripe upgrade paths use to charge `newFull - oldUnused` now and reset the
// billing period to [now, now+cycle]. This function still implements the older
// "Model A" prorated-DIFFERENCE math and is retained for reference/compat; do
// not use it to drive new upgrade charges. DOWNGRADES remain deferred to the
// end of the current period (see processDowngrade / processTierChangeStripe)
// and are unchanged.
//
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

	// Calculate days remaining in current period.
	//
	// Rounding policy: round UP to the nearest whole day. Any partial day
	// remaining counts as a full day of proration charge, so an upgrade with even
	// one hour left in the period is charged for one day rather than truncating to
	// zero. Truncation here previously allowed effectively free upgrades near the
	// period boundary. Computed in seconds to avoid float rounding error.
	daysRemaining := 0
	if periodEndsAt != nil && periodEndsAt.After(now) {
		const secondsPerDay = int64(24 * 60 * 60)
		remainingSeconds := int64(periodEndsAt.Sub(now) / time.Second)
		if remainingSeconds > 0 {
			daysRemaining = int((remainingSeconds + secondsPerDay - 1) / secondsPerDay)
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
	client, err := s.resolveNMIClient(ctx, provider)
	if err != nil {
		return fmt.Errorf("NMI provider '%s' is not configured: %w", provider, err)
	}

	if err := client.DeleteRecurringSubscription(sub.ProcessorSubscriptionID); err != nil {
		if errors.Is(err, nmi.ErrSubscriptionDeletesDisabled) {
			// Kill switch: the superseded subscription stays alive at NMI (a remote
			// duplicate) until reconciliation disposes of it; the tier change must
			// still proceed locally.
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id": sub.ID,
				"processor":       provider,
			}).Warn("processor subscription deletes disabled; superseded subscription left alive at NMI for reconciliation")
			return nil
		}
		return err
	}
	return nil
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
	if !newPrice.IsPurchasable() {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "price is not available"}
	}

	newProduct, err := s.ProductService.GetByID(ctx, newPrice.ProductID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "product not found"}
	}
	if !newProduct.IsPurchasable() {
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
		if existingSub.TenantSubjectID.String() != user.ID {
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
		return s.processTierChangeNMI(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	case processor == "ccbill":
		return s.processTierChangeCCBill(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	case processor == "solana":
		return s.processTierChangeSolana(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	default:
		return nil, &TierChangeError{
			HTTPStatus: http.StatusBadRequest,
			Message:    fmt.Sprintf("unsupported processor: %s", processor),
		}
	}
}

// TierChangePreview computes what a tier change WOULD charge, without mutating
// anything (no Stripe call, no DB write). It mirrors TierChange's resolution and
// validation, then derives the money summary from the universal Model B math so
// the preview and the eventual charge agree:
//
//   - upgrade:   charged now = CalculateModelBUpgradeCharge(old, new, ...); the
//     cycle resets, so the next charge (new full price) is now + cycle.
//   - downgrade: nothing now; the change applies at the current period end, where
//     the next charge is the new (lower) full price.
//
// For Stripe upgrades the returned now-amount is an ESTIMATE (IsEstimate=true):
// Stripe finalizes the exact proration per-second on its side. NMI/Solana charge
// the local math exactly, so it is not an estimate there.
func (s *CheckoutService) TierChangePreview(ctx context.Context, req *TierChangeRequest, user *UserIdentity) (*TierChangePreviewResponse, error) {
	priceID, err := api.ParsePriceID(req.PriceID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "invalid price_id"}
	}
	newPrice, err := s.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "price not found"}
	}
	if !newPrice.IsPurchasable() {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "price is not available"}
	}
	newProduct, err := s.ProductService.GetByID(ctx, newPrice.ProductID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "product not found"}
	}
	if !newProduct.IsPurchasable() {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "product is not available"}
	}

	var existingSub *models.Subscription
	if req.SubscriptionID != uuid.Nil {
		existingSub, err = s.SubscriptionService.GetByID(ctx, req.SubscriptionID)
		if err != nil || existingSub.TenantSubjectID.String() != user.ID {
			return nil, &TierChangeError{HTTPStatus: http.StatusNotFound, Message: "subscription not found"}
		}
	} else {
		existingSub, err = s.SubscriptionService.GetActiveSubscription(ctx, user.ID)
		if err != nil {
			return nil, ErrTierChangeNoSubscription
		}
	}

	currentPrice, err := s.PriceService.GetByID(ctx, existingSub.PriceID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "current price not found"}
	}
	currentProduct, err := s.ProductService.GetByID(ctx, currentPrice.ProductID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "current product not found"}
	}

	if currentProduct.ID == newProduct.ID {
		return nil, ErrTierChangeSameProduct
	}
	if currentProduct.TierGroup != nil && newProduct.TierGroup != nil {
		if strings.TrimSpace(*currentProduct.TierGroup) != strings.TrimSpace(*newProduct.TierGroup) {
			return nil, ErrTierChangeDifferentGroup
		}
	}

	processor := string(existingSub.Processor)
	now := s.now()
	resp := &TierChangePreviewResponse{
		Object:           "tier_change_preview",
		PriceID:          api.FormatPriceID(newPrice.ID),
		Processor:        processor,
		Currency:         newPrice.Currency,
		NextChargeAmount: newPrice.Amount,
	}

	if newProduct.TierRank < currentProduct.TierRank {
		// Downgrade: scheduled for period end, nothing charged now.
		if existingSub.ScheduledPriceID != nil {
			return nil, ErrTierChangePending
		}
		resp.Action = "downgrade"
		resp.AmountDueNow = 0
		resp.Effective = "period_end"
		resp.NextChargeDate = existingSub.CurrentPeriodEndsAt
		resp.IsEstimate = false
		if existingSub.CurrentPeriodEndsAt != nil {
			resp.Message = fmt.Sprintf("No charge now. Your plan changes to %s on %s, then %s.",
				newProduct.DisplayName,
				existingSub.CurrentPeriodEndsAt.Format("January 2, 2006"),
				formatMinorAmount(newPrice.Amount, newPrice.Currency))
		}
		return resp, nil
	}

	// Upgrade: Model B reset-period — charge now, rebill the full price at now+cycle.
	firstCharge, cycleDays := CalculateModelBUpgradeCharge(currentPrice.Amount, newPrice.Amount, existingSub.CurrentPeriodEndsAt, newPrice.BillingCycleDays, now)
	nextDate := now.AddDate(0, 0, cycleDays)
	resp.Action = "upgrade"
	resp.AmountDueNow = firstCharge
	resp.Effective = "now"
	resp.NextChargeDate = &nextDate
	resp.IsEstimate = processor == "stripe"
	resp.Message = fmt.Sprintf("You'll be charged %s now and %s on %s.",
		formatMinorAmount(firstCharge, newPrice.Currency),
		formatMinorAmount(newPrice.Amount, newPrice.Currency),
		nextDate.Format("January 2, 2006"))
	return resp, nil
}

// formatMinorAmount renders a minor-unit (cents) amount as a currency string for
// human-facing preview/confirmation copy, e.g. (6001,"usd") -> "$60.01".
func formatMinorAmount(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign = "-"
		minor = -minor
	}
	symbol := "$"
	if !strings.EqualFold(strings.TrimSpace(currency), "usd") {
		symbol = strings.ToUpper(strings.TrimSpace(currency)) + " "
	}
	return fmt.Sprintf("%s%s%d.%02d", sign, symbol, minor/100, minor%100)
}

// processTierChangeStripe handles Stripe subscription tier changes.
// Upgrades are processed immediately. Downgrades are scheduled for period end
// so the user keeps access to the higher tier they already paid for.
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

	if action == "downgrade" {
		if existingSub.ScheduledPriceID != nil {
			return &TierChangeResponse{
				Object:  "tier_change",
				Status:  "blocked",
				Mode:    "tier_change",
				Action:  action,
				PriceID: api.FormatPriceID(newPrice.ID),
				Payment: CheckoutSessionPaymentResponse{Processor: "stripe"},
				Message: "You already have a tier change scheduled. Please wait for the current period to end or cancel the scheduled change first.",
			}, nil
		}
		currentPrice := existingSub.Price
		if currentPrice == nil {
			var err error
			currentPrice, err = s.PriceService.GetByID(ctx, existingSub.PriceID)
			if err != nil {
				return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "current price not found"}
			}
		}
		currentStripePriceID, ok := currentPrice.GetStripeConfig()
		if !ok || strings.TrimSpace(currentStripePriceID) == "" {
			return nil, &TierChangeError{
				HTTPStatus: http.StatusBadRequest,
				Message:    "current price not configured for Stripe",
			}
		}
		if existingSub.CurrentPeriodEndsAt == nil || existingSub.CurrentPeriodEndsAt.IsZero() {
			return nil, &TierChangeError{
				HTTPStatus: http.StatusBadRequest,
				Message:    "subscription missing current period end",
			}
		}
		currentPeriodStart := existingSub.StartedAt
		if existingSub.CurrentPeriodStartsAt != nil && !existingSub.CurrentPeriodStartsAt.IsZero() {
			currentPeriodStart = *existingSub.CurrentPeriodStartsAt
		}

		stripeService := &subscriptions.StripeService{Config: s.Config}
		if _, err := stripeService.ScheduleSubscriptionPriceChange(ctx, existingSub.ProcessorSubscriptionID, currentStripePriceID, stripePriceID, currentPeriodStart, *existingSub.CurrentPeriodEndsAt, newPrice.BillingCycleDays); err != nil {
			return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
		}

		existingSub.ScheduledPriceID = &newPrice.ID
		if err := s.SubscriptionService.Update(ctx, existingSub); err != nil {
			return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "failed to schedule downgrade"}
		}

		subID := api.FormatSubscriptionID(existingSub.ID)
		effectiveDate := existingSub.CurrentPeriodEndsAt.Format("January 2, 2006")
		return &TierChangeResponse{
			Object:           "tier_change",
			Status:           "succeeded",
			Mode:             "tier_change",
			Action:           action,
			PriceID:          api.FormatPriceID(newPrice.ID),
			Payment:          CheckoutSessionPaymentResponse{Processor: "stripe"},
			SubscriptionID:   &subID,
			Message:          fmt.Sprintf("Downgrade to %s scheduled. Your current plan will remain active until %s.", newProduct.DisplayName, effectiveDate),
			DelayedStart:     existingSub.CurrentPeriodEndsAt,
			Currency:         newPrice.Currency,
			AmountDueNow:     0,
			NextChargeAmount: newPrice.Amount,
			NextChargeDate:   existingSub.CurrentPeriodEndsAt,
		}, nil
	}

	// #268: Model B (reset-period) upgrade via Stripe.
	//
	// We drive Stripe to (a) reset the billing cycle to start NOW and (b)
	// immediately collect the first charge for the fresh period:
	//
	//   - billing_cycle_anchor = "now"        -> resets the period to [now, now+cycle]
	//   - proration_behavior   = "always_invoice" -> Stripe creates AND invoices
	//     the proration right away, so the customer is charged now instead of
	//     having the credit/charge merely sit on the next invoice.
	//
	// With the anchor reset, Stripe bills a fresh full period for the new price
	// and credits the unused portion of the old price as a proration line item;
	// the invoiced total nets to `new_full - old_unused`, i.e. the Model B first
	// charge (matching CalculateModelBUpgradeCharge for the NMI path).
	//
	// ASSUMPTION / TODO(#268): Stripe computes the unused-time credit from its
	// own clock and per-second proration, so the collected amount can differ
	// from CalculateModelBUpgradeCharge by sub-cent/rounding and by Stripe's
	// second- vs whole-day granularity. This is expected — Stripe is the source
	// of truth for Stripe-billed amounts. If exact parity with the NMI math is
	// later required, switch to an explicit invoice-item + price-update flow.
	// Verify the live invoice amount on a real Stripe test upgrade before deploy.
	stripeService := &subscriptions.StripeService{Config: s.Config}
	itemID, err := stripeService.GetSubscriptionItemID(ctx, existingSub.ProcessorSubscriptionID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
	}
	// Pass newPrice.ID so the subscription's metadata[internal_price_id] is
	// rewritten to the new tier; otherwise the proration invoice and every future
	// renewal would resolve the stale old price in the invoice.paid webhook (#268).
	if err := stripeService.UpdateSubscriptionPrice(ctx, existingSub.ProcessorSubscriptionID, itemID, stripePriceID, newPrice.ID.String(), "always_invoice", "now"); err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
	}

	// Update local subscription record.
	// #268: Model B reset the billing cycle at Stripe (anchor "now"), so reflect
	// the fresh period [now, now+cycle] locally. Stripe webhooks remain the
	// source of truth and will reconcile the exact period boundaries.
	stripeNow := s.now()
	stripeCycleDays := 30
	if newPrice.BillingCycleDays != nil && *newPrice.BillingCycleDays > 0 {
		stripeCycleDays = *newPrice.BillingCycleDays
	}
	stripePeriodEnd := stripeNow.AddDate(0, 0, stripeCycleDays)

	// Capture the OLD price + period BEFORE the local reset below overwrites them,
	// so the success-toast estimate matches the preview's now-amount. Stripe
	// finalizes the exact prorated total on its side, so this is an estimate.
	var oldFull int64
	if existingSub.Price != nil {
		oldFull = existingSub.Price.Amount
	}
	oldPeriodEnd := existingSub.CurrentPeriodEndsAt
	estimatedNow, _ := CalculateModelBUpgradeCharge(oldFull, newPrice.Amount, oldPeriodEnd, newPrice.BillingCycleDays, stripeNow)

	existingSub.PriceID = newPrice.ID
	existingSub.ProductID = newPrice.ProductID
	existingSub.ScheduledPriceID = nil
	existingSub.CurrentPeriodStartsAt = &stripeNow
	existingSub.CurrentPeriodEndsAt = &stripePeriodEnd
	if err := s.SubscriptionService.Update(ctx, existingSub); err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "failed to update subscription"}
	}

	subID := api.FormatSubscriptionID(existingSub.ID)

	return &TierChangeResponse{
		Object:           "tier_change",
		Status:           "succeeded",
		Mode:             "tier_change",
		Action:           action,
		PriceID:          api.FormatPriceID(newPrice.ID),
		Payment:          CheckoutSessionPaymentResponse{Processor: "stripe"},
		SubscriptionID:   &subID,
		Message:          "Plan updated",
		Currency:         newPrice.Currency,
		AmountDueNow:     estimatedNow,
		NextChargeAmount: newPrice.Amount,
		NextChargeDate:   &stripePeriodEnd,
	}, nil
}

// processTierChangeSolana handles recurring-Solana subscription tier changes (#272).
//
// Solana plan terms are immutable and a subscription is bound to one plan PDA, so
// a tier change is mechanically cancel-old + subscribe-new — done as a SINGLE
// ATOMIC, wallet-signed on-chain transaction. The synchronous (card-style)
// TierChange API cannot collect a wallet signature, so for BOTH directions this
// returns requires_action directing the client to the dedicated prepare/confirm
// endpoints (#272):
//
//   - UPGRADE: the prepare endpoint returns a PARTIALLY-signed (cranker co-signed)
//     [cancel + subscribe + prorated transfer] tx; the wallet completes + sends it,
//     then confirms. The Model-B prorated first pull (new_full - old_unused) is
//     charged atomically with the switch. The confirm step mirrors the new
//     membership + cancels the old.
//
//   - DOWNGRADE: the prepare endpoint returns an UNSIGNED [cancel + subscribe] tx
//     (no immediate charge); the wallet signs + sends, then confirms. The confirm
//     step defers the new plan's first pull to the OLD period end, so the user
//     keeps the higher tier they already paid for until then.
//
// No charge or DB state change happens in THIS method — it only routes the client
// to the atomic endpoints (Solana is the source of truth; nothing is mirrored
// until the on-chain switch is confirmed).
func (s *CheckoutService) processTierChangeSolana(
	ctx context.Context,
	req *TierChangeRequest,
	user *UserIdentity,
	newPrice *models.Price,
	newProduct *models.Product,
	existingSub *models.Subscription,
	currentProduct *models.Product,
	action string,
) (*TierChangeResponse, error) {
	// Target price must carry a published Solana recurring plan, else the wallet
	// has nothing valid to subscribe to (upgrade) / no terms to schedule (downgrade).
	if !priceHasSolanaRecurring(newPrice) {
		return nil, &TierChangeError{
			HTTPStatus: http.StatusBadRequest,
			Message:    "target price is not configured for Solana recurring billing",
		}
	}

	subID := existingSub.ID
	subIDStr := api.FormatSubscriptionID(subID)

	// Solana tier changes are a single ATOMIC on-chain transaction the subscriber
	// signs (cancel-old + subscribe-new [+ prorated transfer for an upgrade]), so
	// they cannot be driven from this server-side card path — they go through the
	// dedicated prepare/confirm endpoints (#272), which build the co-signed tx and
	// mirror the confirmed switch into the DB. Direct the client there for BOTH an
	// upgrade (prorated first pull, charged atomically) and a downgrade (deferred
	// to the old period end, no immediate charge).
	endpoint := fmt.Sprintf("POST /v1/self/subscriptions/%s/solana-tier-change", subIDStr)
	var msg string
	if action == "downgrade" {
		msg = fmt.Sprintf(
			"To downgrade to %s, call %s with new_price_id=%s, sign the returned transaction in your wallet, then confirm it. There is no immediate charge — your current plan keeps billing until the period ends, then rebills at the lower tier.",
			newProduct.DisplayName, endpoint, req.PriceID,
		)
	} else {
		msg = fmt.Sprintf(
			"To upgrade to %s, call %s with new_price_id=%s, sign the returned (co-signed) transaction in your wallet, then confirm it. You'll be charged the prorated difference atomically on the switch; your old plan is cancelled in the same transaction.",
			newProduct.DisplayName, endpoint, req.PriceID,
		)
	}
	return &TierChangeResponse{
		Object:         "tier_change",
		Status:         "requires_action",
		Mode:           "tier_change",
		Action:         action,
		PriceID:        req.PriceID,
		Payment:        CheckoutSessionPaymentResponse{Processor: "solana"},
		SubscriptionID: &subIDStr,
		Message:        msg,
	}, nil
}

// processTierChangeNMI handles NMI-backed subscription tier changes.
// Upgrades: immediate proration charge + new subscription
// Downgrades: scheduled for end of billing period
func (s *CheckoutService) processTierChangeNMI(
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
	if existingSub.PaymentMethodID != nil {
		checkoutReq.PaymentMethodID = api.FormatPaymentMethodID(*existingSub.PaymentMethodID)
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
		URL:            checkoutResp.RedirectURL,
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
		tierResp.URL = resp.RedirectURL
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

func requireNMIPlanForProcessor(price *models.Price, provider string) (string, error) {
	provider = normalize.Lower(provider)
	if provider == "" {
		return "", errors.New("processor is required")
	}
	if price == nil {
		return "", errors.New("price is required")
	}
	planID, ok := price.GetNMIConfigForProcessor(provider)
	if !ok || strings.TrimSpace(planID) == "" {
		return "", fmt.Errorf("price %s is missing NMI plan configuration for processor %s", price.ID, provider)
	}
	return planID, nil
}
