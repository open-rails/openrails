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
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
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
	Payment        CheckoutSessionPaymentResponse `json:"payment"`                   // Rail info
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
	Rail             string     `json:"rail"`
	Currency         string     `json:"currency"`
	AmountDueNow     int64      `json:"amount_due_now"`     // cents charged immediately (0 for downgrade)
	NextChargeAmount int64      `json:"next_charge_amount"` // cents at next renewal (new plan price)
	NextChargeDate   *time.Time `json:"next_charge_date,omitempty"`
	Effective        string     `json:"effective"`   // "now" (upgrade) | "period_end" (downgrade)
	IsEstimate       bool       `json:"is_estimate"` // true when the rail finalizes the exact amount (Stripe upgrades)
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
	VaultedCardService   *CheckoutVaultedCardService
	PaymentMethodService *paymentmethods.PaymentMethodService
	VaultService         *paymentmethods.VaultService
	IdempotencyService   checkoutIdempotencyStore
	MerchantSecrets      merchants.MerchantSecretReader
	ProviderSecrets      merchants.PSPSecretResolver
	// RailCustomerService maps app users to rail customer ids so we
	// reuse a single Stripe customer per user (issue #212) and can record the
	// mapping at checkout time instead of relying solely on webhooks.
	RailCustomerService *payments.RailCustomerService
	// StripeService is used to resolve/create the Stripe customer and to run the
	// webhook-independent duplicate guard (issue #213).
	StripeService *subscriptions.StripeService
	// Intents executes durable write-ahead provider intents (#674). Every NMI
	// recurring create in this flow goes through it.
	Intents intentExecutor
	clock   clockwork.Clock
	Config  *config.Config
	Rails   railresolve.Source
	// NMIEndpointOverride points store-armed NMI clients at a fake gateway
	// (test seam; empty = real endpoints).
	NMIEndpointOverride string
	// ResolveNMIClientOverride replaces the store-armed NMI client resolution
	// entirely (test seam; nil = the scoped resolution).
	ResolveNMIClientOverride func(context.Context, string) (*nmi.NMIClient, error)
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
	paymentMethodService *paymentmethods.PaymentMethodService,
	vaultService *paymentmethods.VaultService,
	idempotencyService checkoutIdempotencyStore,
	railCustomerService *payments.RailCustomerService,
	cfg *config.Config,
	railSet railresolve.Source,
	clocks ...clockwork.Clock,
) *CheckoutService {
	clock := timeutil.FirstClock(clocks...)
	service := &CheckoutService{
		SubscriptionService:  subscriptionService,
		ProductService:       productService,
		PriceService:         priceService,
		PaymentService:       paymentService,
		EntitlementService:   entitlementService,
		PurchaseService:      NewCheckoutPurchaseService(priceService, productService, paymentService, entitlementService, subscriptionService, clock),
		VaultResolver:        NewCheckoutVaultService(paymentMethodService, vaultService),
		PaymentMethodService: paymentMethodService,
		VaultService:         vaultService,
		IdempotencyService:   idempotencyService,
		RailCustomerService:  railCustomerService,
		StripeService:        &subscriptions.StripeService{Config: cfg, Rails: railSet},
		clock:                clock,
		Config:               cfg,
		Rails:                railSet,
	}
	service.NMISaleService = NewCheckoutNMISaleService(
		service.PurchaseService,
		service.VaultResolver,
		vaultService,
		idempotencyService,
	)
	// The scoped resolver is the ONLY NMI client source (#788); armed for
	// real once SetMerchantSecretStore wires the merchant secret store.
	service.NMISaleService.ResolveNMIClient = service.resolveNMIClient
	service.VaultedCardService = &CheckoutVaultedCardService{
		PurchaseService:      service.PurchaseService,
		PaymentMethodService: paymentMethodService,
		IdempotencyStore:     idempotencyService,
		Rails:                railSet,
		Config:               cfg,
	}
	if vaultService != nil {
		service.VaultedCardService.DB = vaultService.DB
	}
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
	// #774: price_id accepts either a price UUID/opaque id or a price_key.
	price, err := catalog.ResolveReference(ctx, s.PriceService, req.PriceID)
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

	// Normalize rail
	rail := strings.TrimSpace(strings.ToLower(req.Rail))
	if rail == "" {
		return nil, errors.New("rail is required")
	}

	// #704: pin the active provider account for this rail so payment /
	// subscription / payment-method rows created by this flow carry
	// psp_id provenance (nil when unresolvable — never invented).
	ctx = s.stampPSP(ctx, rail)

	// Check for tier group conflicts (upgrade/downgrade scenarios)
	// This must happen BEFORE the general coverage check
	if product.TierGroup != nil && *product.TierGroup != "" {
		existingSub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndTierGroup(ctx, user.ID, *product.TierGroup)
		if err != nil && !db.IsNotFound(err) {
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
		if rail == "stripe" {
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
		if rail == "ccbill" {
			return &CheckoutResponse{
				Status:  "blocked",
				Message: "You already have active access. CCBill subscriptions cannot be scheduled for future start. Please try again when your current access expires.",
			}, nil
		}

		// Other rails: allow with delayed start
	}

	// Determine if this is a subscription or one-time purchase
	isSubscription := price.AutoRenew

	if isSubscription {
		// #691 checkout guard: an `unknown` sub for this product/tier-group means
		// an existing membership pending provider verification — its real
		// provider-side sub may still be alive and billing, so a re-purchase
		// double-bills. Reject with the machine-readable code; verify/resume
		// instead of repurchase. (past_due already holds the lifecycle slot and is
		// blocked by the tier-group/coverage guards above.)
		if s.PurchaseService != nil {
			conflict, err := s.PurchaseService.checkUnknownSubscriptionConflict(ctx, user.ID, product)
			if err != nil {
				return nil, err
			}
			if conflict != nil && conflict.Blocked {
				return &CheckoutResponse{
					Status:  "blocked",
					Code:    conflict.Code,
					Message: conflict.Message,
				}, nil
			}
		}
		return s.processSubscription(ctx, req, user, price, product, coverage, rail)
	}
	return s.processOneTimePurchase(ctx, req, user, price, product, coverage, rail)
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
	rail string,
) (*CheckoutResponse, error) {
	// The requested name is a payment PROVIDER (account key, e.g. "mobius") or
	// a rail; dispatch on the resolved rail, hand the provider to the leg.
	target, err := s.resolveRailTarget(ctx, rail)
	if err != nil {
		return nil, err
	}
	switch {
	case target.Rail == "ccbill":
		return s.processCCBillSubscription(ctx, req, user, price)
	case rails.IsNMI(models.Rail(target.Rail)):
		return s.processNMISubscription(ctx, req, user, price, product, coverage, target)
	case target.Rail == "stripe":
		return s.processStripeSubscription(ctx, req, user, price, coverage)
	case target.Rail == "solana":
		return nil, errors.New("solana does not support recurring subscriptions; use a one-time price instead")
	case rail == string(models.RailVaultedCard):
		// #795: the vaulted_card rail has no provider-side recurring engine and
		// the OpenRails-driven renewal worker for seam rails is not built yet —
		// enrolling would strand renewals. Loud error, never a silent accept.
		return nil, errors.New("vaulted_card subscriptions are not supported yet (renewals are engine-driven; see #795)")
	default:
		return nil, fmt.Errorf("unsupported rail: %s", target.Rail)
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
	rail string,
) (*CheckoutResponse, error) {
	// The requested name is a payment PROVIDER (account key) or a rail;
	// dispatch on the resolved rail, hand the provider to the leg.
	target, err := s.resolveRailTarget(ctx, rail)
	if err != nil {
		return nil, err
	}
	switch {
	case rails.IsNMI(models.Rail(target.Rail)):
		return s.processNMISale(ctx, req, user, price, product, coverage, target)
	case target.Rail == "solana":
		return s.processSolanaPurchase(ctx, req, user, price, product, coverage)
	case target.Rail == "ccbill":
		return nil, errors.New("ccbill does not support one-time purchases; use a subscription price instead")
	case target.Rail == "stripe":
		return s.processStripePayment(ctx, req, user, price)
	case rail == string(models.RailVaultedCard):
		if s.VaultedCardService == nil {
			return nil, errors.New("vaulted_card checkout is not configured")
		}
		idempotencyKey := s.getIdempotencyKey(req, user.ID, price.ID, "vaulted_card_sale")
		return s.VaultedCardService.Process(ctx, req, user, price, product, idempotencyKey)
	default:
		return nil, fmt.Errorf("unsupported rail for one-time purchases: %s", target.Rail)
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
	if existingSub.Rail != models.RailCCBill {
		return nil, errors.New("existing subscription is not a CCBill subscription")
	}
	if existingSub.RailSubscriptionID == "" {
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
		OriginalSubscriptionID: existingSub.RailSubscriptionID,
	}

	response, err := ccbillClient.GenerateUpgradeFlexFormURL(upgradeParams)
	if err != nil {
		return nil, fmt.Errorf("failed to generate CCBill upgrade FlexForm URL: %w", err)
	}

	log.WithFields(log.Fields{
		"user_id":              user.ID,
		"subscription_id":      existingSub.ID,
		"current_price_id":     existingSub.PriceID,
		"target_price_id":      newPrice.ID,
		"rail_subscription_id": existingSub.RailSubscriptionID,
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
	target railTarget,
) (*CheckoutResponse, error) {
	// Rows and intents speak rail vocabulary; the provider (account key) picks
	// the plan link and pins the NMI client.
	provider := target.Rail
	nmiPlanID, err := requireNMIPlanForTarget(price, target)
	if err != nil {
		return nil, err
	}

	// Fail fast on misconfiguration instead of parking a user-facing checkout.
	if _, err := s.resolveNMIClient(ctx, target.PSP); err != nil {
		return nil, fmt.Errorf("NMI provider '%s' is not configured: %w", target.PSP, err)
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
			// Fall through: the durable intent below is the source of truth —
			// it replays a success, reports a decline, or keeps verifying an
			// ambiguous create under the ORIGINAL order id (#674).
		}
	}

	// Get or create vault (payment method)
	customerVaultID, vaultBillingID, resolvedMethod, createdVault, err := s.VaultResolver.ResolveVault(ctx, req, user, target)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}
	// #297: the instrument's recurring-sequence stored-credential anchor. ""
	// (fresh vault or legacy instrument) makes this enrollment the sequence's
	// initial CIT; the intent's finalize captures the first-charge txn id.
	storedCredentialRef := ""
	if resolvedMethod != nil {
		storedCredentialRef = strings.TrimSpace(resolvedMethod.StoredCredentialRecurringRef)
	}

	// Determine start date for delayed start
	now := s.now().UTC()
	startDate, delayedStart := nmiSubscriptionStartDate(coverage, now)

	// Local subscription ID for a FRESH intent; a replayed request maps onto
	// the existing intent, whose stored payload (and so its original local
	// subscription id) wins — enqueue conflicts never overwrite the payload.
	subscriptionID := uuidutil.NewV7()
	var paymentMethodID *uuid.UUID
	if resolvedMethod != nil {
		paymentMethodID = &resolvedMethod.ID
	}

	if _, err := customerIDFromUser(user.ID); err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}
	if s.Intents == nil {
		return nil, errors.New("checkout intent executor not wired")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// #674 write-through: durable intent first, inline execution, NMI order id
	// derived from the intent id. A crash/timeout at ANY point leaves an intent
	// the executor/verifier resolves (sale search + roster scan) — never an
	// orphaned live remote subscription, never a blind re-create.
	intent, err := s.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{
		MerchantID: tid.UUID(),
		Provider:   provider,
		IntentType: TypeNMISubscriptionCreate,
		PriceID:    &price.ID,
		Payload: NMISubscriptionCreatePayload{
			Provider:               provider,
			PSP:                    target.PSP,
			PlanID:                 nmiPlanID,
			CustomerVaultID:        customerVaultID,
			BillingID:              vaultBillingID,
			AmountMicros:           price.Amount,
			Currency:               price.Currency,
			Email:                  req.Email,
			UserID:                 user.ID,
			PriceID:                price.ID,
			LocalSubscriptionID:    subscriptionID,
			PaymentMethodID:        paymentMethodID,
			StartDate:              startDate,
			DelayedStart:           delayedStart,
			StoredCredentialRef:    storedCredentialRef,
			E2ERunID:               strings.TrimSpace(req.Metadata["e2e_run_id"]),
			CheckoutIdempotencyKey: idempotencyKey,
			FirstName:              ResolveCheckoutFirstName(req, user),
			LastName:               ResolveCheckoutLastName(req),
			Address1:               DefaultIfEmpty(req.Address1, "N/A"),
			City:                   DefaultIfEmpty(req.City, "N/A"),
			State:                  DefaultIfEmpty(req.State, "N/A"),
			Zip:                    DefaultIfEmpty(req.Zip, "00000"),
			Country:                DefaultIfEmpty(req.Country, "US"),
		},
		IdempotencyKey: NMISubscriptionCreateIdempotencyKey(idempotencyKey),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginUser,
		OriginReason:   "checkout subscription create",
	})
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("post subscription create intent: %w", err)
	}

	switch intent.Status {
	case intents.StatusSucceeded:
		// finalize already completed the idempotency record.
		return nmiSubscriptionResponseFromIntent(intent)
	case intents.StatusFailedTerminal:
		// Verified-clean decline/rejection. Direct best-effort cleanup of the
		// vault created for THIS attempt, NOT an intent (#674 tail): it is
		// referenced nowhere — harmless if lost.
		if createdVault && resolvedMethod != nil && s.VaultService != nil {
			if cleanupErr := s.VaultService.CleanupVaultBestEffort(ctx, resolvedMethod); cleanupErr != nil {
				log.WithError(cleanupErr).WithField("vault_id", customerVaultID).Warn("failed to cleanup payment method after subscription error")
			}
		}
		reason := "failed to create subscription"
		if intent.LastFailureReason != nil && *intent.LastFailureReason != "" {
			reason = "failed to create subscription: " + *intent.LastFailureReason
		}
		var failErr error = errors.New(reason)
		if locID := terminalEvidenceLocalization(intent); locID != "" {
			failErr = &paymentmethods.VaultError{Err: failErr, LocalizationID: locID, Message: reason}
		}
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, failErr)
		return nil, failErr
	default:
		// pending (parked), in_flight, unknown_needs_verify, failed_retryable:
		// the intent ledger finishes it out-of-band.
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, ErrCheckoutProcessing)
		return nil, ErrCheckoutProcessing
	}
}

// nmiSubscriptionResponseFromIntent rebuilds the checkout response from a
// succeeded create intent's evidence.
func nmiSubscriptionResponseFromIntent(intent gen.OpenrailsRailIntent) (*CheckoutResponse, error) {
	var evidence struct {
		SubscriptionID string `json:"subscription_id"`
		TransactionID  string `json:"transaction_id"`
		DelayedStart   string `json:"delayed_start"`
		Status         string `json:"status"`
		Message        string `json:"message"`
	}
	if len(intent.ResultEvidence) == 0 {
		return nil, errors.New("subscription created but evidence unreadable")
	}
	if err := json.Unmarshal(intent.ResultEvidence, &evidence); err != nil {
		return nil, fmt.Errorf("subscription created but evidence unreadable: %w", err)
	}
	resp := &CheckoutResponse{
		Status:        evidence.Status,
		Action:        "new",
		Message:       evidence.Message,
		TransactionID: evidence.TransactionID,
	}
	if resp.Status == "" {
		resp.Status = "success"
	}
	if resp.Message == "" {
		resp.Message = "Subscription created successfully"
	}
	if evidence.SubscriptionID != "" {
		if id, err := uuid.Parse(evidence.SubscriptionID); err == nil {
			resp.SubscriptionID = &id
		}
	}
	if evidence.DelayedStart != "" {
		if t, err := time.Parse(time.RFC3339, evidence.DelayedStart); err == nil {
			resp.DelayedStart = &t
		}
	}
	return resp, nil
}

// terminalEvidenceLocalization reads the decline localization id off a
// terminally-failed intent's evidence (for client-facing error rendering).
func terminalEvidenceLocalization(intent gen.OpenrailsRailIntent) string {
	if len(intent.ResultEvidence) == 0 {
		return ""
	}
	var evidence struct {
		LocalizationID string `json:"localization_id"`
	}
	if err := json.Unmarshal(intent.ResultEvidence, &evidence); err != nil {
		return ""
	}
	return evidence.LocalizationID
}

func (s *CheckoutService) completeNMISubscriptionRegistration(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, provider string, subscriptionID uuid.UUID, providerSubscriptionID string, transactionID string, delayedStart *time.Time, orderID string, paymentMethodID *uuid.UUID, idempOp string, idempotencyKey string) (*CheckoutResponse, error) {
	if existing, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, provider, providerSubscriptionID); err == nil {
		if delayedStart == nil && (existing.Status == models.StatusPending || existing.Status == models.StatusActive) {
			return s.activateImmediateNMISubscription(ctx, req, user, price, existing.ID, provider, providerSubscriptionID, transactionID, orderID, idempOp, idempotencyKey)
		}
		return s.nmiSubscriptionPendingResponse(ctx, existing.ID, transactionID, delayedStart, idempOp, idempotencyKey)
	} else if !db.IsNotFound(err) {
		return nil, fmt.Errorf("load existing subscription: %w", err)
	}

	customerID, err := customerIDFromUser(user.ID)
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	var emailPtr *string
	if req.Email != "" {
		emailPtr = &req.Email
	}
	subscription := &models.Subscription{
		ID:                       subscriptionID,
		CustomerID:               customerID,
		ProductID:                price.ProductID,
		PriceID:                  price.ID,
		EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(product.EntitlementsSpec),
		CreditsSpecSnapshot:      models.CloneCreditsSpec(product.CreditsSpec),
		RailSubscriptionID:       providerSubscriptionID,
		Status:                   models.StatusPending,
		Rail:                     models.Rail(provider),
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
			if existing, loadErr := s.SubscriptionService.GetByRailSubscriptionID(ctx, provider, providerSubscriptionID); loadErr == nil {
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
		if subscription.RailSubscriptionID == "" {
			subscription.RailSubscriptionID = providerSubscriptionID
		}
		subscription.Rail = models.Rail(provider)
		if err := s.SubscriptionService.Update(ctx, subscription); err != nil {
			_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
			return nil, fmt.Errorf("failed to activate NMI subscription: %w", err)
		}
	}
	if err := s.grantImmediateNMISubscriptionEntitlements(ctx, user.ID, subscription); err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	if _, err := s.PaymentService.GetByTransactionID(ctx, models.Rail(provider), transactionID); err != nil {
		if !db.IsNotFound(err) {
			_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
			return nil, fmt.Errorf("failed to check NMI subscription payment: %w", err)
		}
		now := s.now().UTC()
		payment := &models.Payment{
			ID:                       uuidutil.NewV7(),
			CustomerID:               subscription.CustomerID,
			PriceID:                  price.ID,
			SubscriptionID:           &subscription.ID,
			Rail:                     models.Rail(provider),
			TransactionID:            transactionID,
			Amount:                   price.Amount,
			ListAmount:               price.Amount,
			Currency:                 price.Currency,
			Status:                   payments.PaymentStatusCompletedValue,
			Metadata:                 metadata,
			EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(subscription.EntitlementsSpecSnapshot),
			CreditsSpecSnapshot:      models.CloneCreditsSpec(subscription.CreditsSpecSnapshot),
			AttemptKind:              func() *string { k := payments.AttemptInitial; return &k }(),
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
			UserID:      userID,
			CustomerID:  subscription.CustomerID,
			Entitlement: entitlementName,
			NotBefore:   &notBefore,
			EndAt:       &endAt,
			SourceType:  models.EntitlementSourceSubscription,
			SourceID:    subscription.ID,
		}); err != nil {
			return fmt.Errorf("failed to grant entitlement %s: %w", entitlementName, err)
		}
	}
	return nil
}

func nmiSubscriptionPeriodEnd(start time.Time, price *models.Price) time.Time {
	if ch := price.RecurringCycleHours(); ch != nil {
		return start.Add(time.Duration(*ch) * time.Hour)
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
	target railTarget,
) (*CheckoutResponse, error) {
	_ = coverage
	if s.NMISaleService == nil {
		return nil, errors.New("NMI sale service unavailable")
	}
	idempotencyKey := s.getIdempotencyKey(req, user.ID, price.ID, "nmi_sale")
	return s.NMISaleService.Process(ctx, req, user, price, product, idempotencyKey, target)
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
	_, _, err := subscriptions.RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return nil, err
	}
	stripePriceID, err := getStripePriceID(price)
	if err != nil {
		return nil, err
	}
	successURL := strings.TrimSpace(req.SuccessURL)
	cancelURL := strings.TrimSpace(req.CancelURL)
	if successURL == "" || cancelURL == "" {
		return nil, errors.New("stripe success_url and cancel_url are required")
	}

	// Resolve a single, durable Stripe customer for this user (issue #212) so we
	// stop minting a fresh customer on every checkout. The mapping is recorded
	// here, at checkout time, not only via webhook.
	customerID, err := s.resolveStripeCustomer(ctx, user)
	if err != nil {
		return nil, err
	}

	if stripePaidIntroUnsupported(price) {
		return nil, errors.New("stripe paid introductory pricing is not supported")
	}
	trialAnchor := req.CheckoutStartedAt
	if trialAnchor.IsZero() {
		trialAnchor = s.now()
	}
	trialEnd := stripeCheckoutTrialEnd(price, coverage, trialAnchor)
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
		IdempotencyKey:    req.IdempotencyKey,
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

func stripePaidIntroUnsupported(price *models.Price) bool {
	if price == nil {
		return false
	}
	trialAmount, _, ok := price.GetTrial()
	return ok && trialAmount != 0
}

func stripeCheckoutTrialEnd(price *models.Price, coverage *CoverageInfo, now time.Time) int64 {
	if coverage != nil && coverage.HasCoverage && coverage.EndDate != nil && coverage.EndDate.After(now.Add(5*time.Minute)) {
		return coverage.EndDate.Unix()
	}
	if price != nil {
		trialAmount, trialHours, ok := price.GetTrial()
		if ok && trialAmount == 0 && trialHours > 0 {
			return now.UTC().Add(time.Duration(trialHours) * time.Hour).Unix()
		}
	}
	return 0
}

func (s *CheckoutService) processStripePayment(
	ctx context.Context,
	req *CheckoutRequest,
	user *UserIdentity,
	price *models.Price,
) (*CheckoutResponse, error) {
	_, _, err := subscriptions.RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return nil, err
	}
	stripePriceID, err := getStripePriceID(price)
	if err != nil {
		return nil, err
	}
	successURL := strings.TrimSpace(req.SuccessURL)
	cancelURL := strings.TrimSpace(req.CancelURL)
	if successURL == "" || cancelURL == "" {
		return nil, errors.New("stripe success_url and cancel_url are required")
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
		IdempotencyKey:    req.IdempotencyKey,
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

// railCustomerStore is the slice of RailCustomerService used for
// Stripe customer resolution. Defined as an interface so the resolution logic
// is unit-testable without a database.
type railCustomerStore interface {
	GetCustomerID(ctx context.Context, userID, rail string) (string, error)
	Upsert(ctx context.Context, userID, rail, customerID string) error
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

func (s *CheckoutService) customerStore() railCustomerStore {
	if s.RailCustomerService == nil {
		return nil
	}
	return s.RailCustomerService
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
//  1. local mapping (RailCustomerService.GetCustomerID)
//  2. Stripe Customer Search on metadata[app_user_id]
//  3. create a fresh, idempotent Stripe customer
//
// Whenever a customer is resolved (or created), the local mapping is upserted so
// the link survives even if the corresponding webhook is missed. It returns ""
// (and no error) only when the dependencies are unavailable, in which case the
// caller falls back to the legacy customer_email behavior.
func resolveStripeCustomerWith(ctx context.Context, store railCustomerStore, client stripeCustomerClient, user *UserIdentity) (string, error) {
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
	if err != nil && !db.IsNotFound(err) {
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
func stripeTierGroupConflictWith(ctx context.Context, store railCustomerStore, client stripeCustomerClient, prices stripePriceResolver, products productResolver, user *UserIdentity, tierGroup string) (bool, error) {
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
	if err != nil && !db.IsNotFound(err) {
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
			if db.IsNotFound(err) {
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
			if db.IsNotFound(err) {
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
	cfg := price.PSPLinkForRail(models.RailStripe)
	if cfg == nil {
		return "", errors.New("stripe price not configured")
	}
	id := strings.TrimSpace(cfg[models.RailKeyStripePriceID])
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
	IdempotencyKey    string
}

func (s *CheckoutService) createStripeCheckoutSession(ctx context.Context, params stripeCheckoutParams) (string, error) {
	stripeProc, _, err := subscriptions.RequireStripeSecretKey(ctx, s.Rails)
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
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(stripeProc.Stripe.SecretKey))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	stripeapi.SetIdempotencyKey(req, stripeCheckoutIdempotencyKey(params.IdempotencyKey))

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

func stripeCheckoutIdempotencyKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return "openrails-checkout-" + hex.EncodeToString(sum[:])
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
	if req != nil {
		// #704 provenance stamping for the registered payment row.
		ctx = s.stampPSP(ctx, req.Rail)
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
	target railTarget,
) (*CheckoutResponse, error) {
	now := s.now()

	// Derive the payer before any money moves (#364): a zero id must never
	// reach the subscription/payment writes below.
	customerID, err := customerIDFromUser(user.ID)
	if err != nil {
		return nil, err
	}

	// CCBill handles upgrades via their own Package Upgrade flow
	if target.Rail == "ccbill" {
		return s.processCCBillUpgrade(ctx, user, newPrice, existingSub)
	}

	// Solana doesn't support subscriptions
	if target.Rail == "solana" {
		return nil, errors.New("solana does not support subscription upgrades")
	}

	// Only NMI-backed rails support programmatic upgrades
	if !rails.IsNMI(models.Rail(target.Rail)) {
		return nil, fmt.Errorf("unsupported rail for upgrades: %s", target.Rail)
	}

	// Get idempotency key (client-provided or generated)
	const idempOp = "nmi_upgrade"
	idempotencyKey := s.getUpgradeIdempotencyKey(req, user.ID, existingSub.ID, newPrice.ID)

	// Check idempotency - have we already processed this upgrade?
	idempRec, alreadyExists, err := s.IdempotencyService.Begin(ctx, idempOp, idempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("idempotency check failed: %w", err)
	}

	// retryAfterFailure: this request retries a previously-failed attempt whose
	// proration charge may have landed (verify before charging again, #674).
	retryAfterFailure := false

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
			// Fall through and re-run: the proration order id is content-derived
			// (stable across retries), so a charge landed by the failed attempt
			// is re-found by the pre-charge verify below (#674).
			retryAfterFailure = true
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
	billingCycleHours := newPrice.RecurringCycleHours()
	if billingCycleHours == nil || *billingCycleHours <= 0 {
		billingCycleHours = oldPrice.RecurringCycleHours()
	}
	prorationAmount, cycleHours := CalculateModelBUpgradeCharge(
		oldPrice.Amount,
		newPrice.Amount,
		existingSub.CurrentPeriodEndsAt,
		billingCycleHours,
		now,
	)

	log.WithFields(log.Fields{
		"user_id":          user.ID,
		"old_price":        oldPrice.Amount,
		"new_price":        newPrice.Amount,
		"cycle_hours":      cycleHours,
		"proration_amount": prorationAmount, // Model B first charge (new_full - old_unused)
		"billing_model":    "B",
	}).Info("calculating Model-B upgrade first charge")

	// NMI charges in whole cents; prorationAmount is micros. Error (never round)
	// on a sub-cent remainder — same policy as the one-time sale path.
	prorationCents, err := moneyutil.MicrosToCentsExact(prorationAmount)
	if err != nil {
		err := fmt.Errorf("upgrade proration amount must be representable in whole cents: %w", err)
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	provider := target.Rail
	if existingSub.CurrentPeriodEndsAt == nil || existingSub.CurrentPeriodEndsAt.IsZero() {
		err := errors.New("existing subscription missing current period end")
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	nmiPlanID, err := requireNMIPlanForTarget(newPrice, target)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// #268: Model B resets the billing period to [now, now+cycle]. The immediate
	// first charge (RunSale below) pays for this fresh period, so the recurring
	// NMI subscription's first scheduled rebill must land at the NEW period end
	// (now + cycle), not the old period end.
	newPeriodStart := now
	newPeriodEnd := now.Add(time.Duration(cycleHours) * time.Hour)
	startDate, _ := buildNMIFutureStartDate(newPeriodEnd, now)

	client, err := s.resolveNMIClient(ctx, target.PSP)
	if err != nil {
		err := fmt.Errorf("NMI provider '%s' is not configured: %w", target.PSP, err)
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// Get or create vault
	customerVaultID, vaultBillingID, resolvedMethod, createdVault, err := s.VaultResolver.ResolveVault(ctx, req, user, target)
	if err != nil {
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}

	// #297 CIT contexts for the two upgrade charges: the successor enrollment
	// rides the RECURRING sequence, the proration sale the UNSCHEDULED one.
	// An instrument without an anchor makes the charge that sequence's initial
	// CIT; anchors are captured after success below.
	recurringCtx := charge.InitialRecurring()
	unscheduledCtx := charge.InitialOneTime()
	if resolvedMethod != nil {
		if ref := strings.TrimSpace(resolvedMethod.StoredCredentialRecurringRef); ref != "" {
			recurringCtx = charge.RecurringReuse(ref)
		}
		if ref := strings.TrimSpace(resolvedMethod.StoredCredentialUnscheduledRef); ref != "" {
			unscheduledCtx = charge.OneTimeReuse(ref)
		}
	}

	// Step 1: Create the successor subscription at NMI before charging/cancelling.
	newSubscriptionID := uuidutil.NewV7()

	// Successor order id is CONTENT-DERIVED (stable across retries, like the
	// proration order id below): an ambiguous create stays recoverable — the
	// roster scan re-finds the orphan instead of minting a second live
	// subscription (#674 tail).
	successorOrderID := "upgs-" + shortHash(idempotencyKey)

	// A previous failed attempt may have created the successor at NMI and lost
	// the response. Adopt it instead of creating a duplicate.
	var resp *nmi.AddSubscriptionResponse
	if retryAfterFailure {
		adopted, ok, aerr := s.findAdoptableUpgradeSuccessor(ctx, client, provider, customerVaultID, nmiPlanID, successorOrderID)
		if aerr != nil {
			_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, ErrCheckoutProcessing)
			return nil, ErrCheckoutProcessing
		}
		if ok {
			resp = adopted
		}
	}

	if resp == nil {
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
			BillingID:       vaultBillingID,
			Amount:          moneyutil.MajorUnits(moneyutil.MicrosToMajorUnits(newPrice.Amount)), // NMI recurring amount is DOLLARS
			Currency:        newPrice.Currency,
			Email:           req.Email,
			OrderID:         successorOrderID,
			PONumber:        successorOrderID,
			CustomerID:      user.ID,
			// Start date uses day precision and must be strictly in the future for NMI.
			StartDate:        startDate,
			StoredCredential: nmidirect.StoredCredentialFor(recurringCtx),
		}

		created, err := client.AddRecurringSubscription(params)
		switch {
		case err == nil:
			resp = created
		case nmi.IsTransportAmbiguous(err):
			// The successor MAY exist at NMI — never treat as a clean failure.
			// Adopt it if the roster already shows it; otherwise surface
			// "processing" so the retry (same content-derived key) re-runs the
			// adopt scan. Never a second blind create, never a live remote
			// subscription abandoned as failed (#674 tail).
			adopted, ok, aerr := s.findAdoptableUpgradeSuccessor(ctx, client, provider, customerVaultID, nmiPlanID, successorOrderID)
			if aerr != nil || !ok {
				_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, ErrCheckoutProcessing)
				return nil, ErrCheckoutProcessing
			}
			resp = adopted
		default:
			// Verified-clean decline/rejection: nothing was created.
			subErr := fmt.Errorf("failed to create upgraded subscription: %w", err)
			var nmiErr *nmi.CustomerVaultError
			if errors.As(err, &nmiErr) {
				subErr = &paymentmethods.VaultError{
					Err:            subErr,
					LocalizationID: nmiErr.LocalizationID,
					Message:        subErr.Error(),
				}
			}
			_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, subErr)
			return nil, subErr
		}
	}

	rollbackNewSubscription := func() {
		cleanupSub := &models.Subscription{RailSubscriptionID: resp.SubscriptionID}
		if cancelErr := s.cancelNMISubscription(ctx, cleanupSub, provider); cancelErr != nil {
			log.WithError(cancelErr).WithFields(log.Fields{
				"subscription_id":      newSubscriptionID,
				"rail_subscription_id": resp.SubscriptionID,
				"rail":                 provider,
			}).Error("failed to rollback successor NMI subscription after upgrade error")
		}
	}

	// Step 2: Charge prorated difference (if positive).
	var prorationTransactionID string
	if prorationAmount > 0 {
		// Derive a stable OrderID from the idempotency key so a retried upgrade
		// reuses the same order reference at NMI: a charge landed by a previous
		// ambiguous attempt is re-found by the order id, and NMI's duplicate-
		// transaction detection backstops a raced double send.
		prorationOrderID := "upg-" + shortHash(idempotencyKey)
		if retryAfterFailure {
			// A previous attempt failed; its charge may have landed. Verify by
			// the order id BEFORE sending another sale (#674).
			if txnID, found, verr := client.FindSuccessfulSaleByOrderID(prorationOrderID); verr != nil {
				rollbackNewSubscription()
				_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, ErrCheckoutProcessing)
				return nil, ErrCheckoutProcessing
			} else if found {
				prorationTransactionID = txnID
			}
		}
		if prorationTransactionID == "" {
			saleResp, err := client.RunSale(nmi.SaleParams{
				CustomerVaultID:  customerVaultID,
				BillingID:        vaultBillingID,
				Amount:           moneyutil.Cents(prorationCents), // SaleParams.Amount is CENTS
				Currency:         newPrice.Currency,
				OrderDescription: fmt.Sprintf("Upgrade proration: %s", newProduct.DisplayName),
				OrderID:          prorationOrderID,
				StoredCredential: nmidirect.StoredCredentialFor(unscheduledCtx),
			})
			switch {
			case err == nil:
				prorationTransactionID = saleResp.TransactionID
			case nmi.IsTransportAmbiguous(err):
				// The charge MAY have landed: never treat as a decline. Verify
				// by the order id; unresolved ⇒ surface "processing" so the
				// retry (same content-derived key) re-verifies — never a blind
				// re-charge, never an unrecorded charge treated as failed.
				txnID, found, verr := client.FindSuccessfulSaleByOrderID(prorationOrderID)
				if verr == nil && found {
					prorationTransactionID = txnID
				} else {
					rollbackNewSubscription()
					_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, ErrCheckoutProcessing)
					return nil, ErrCheckoutProcessing
				}
			default:
				// Verified-clean decline/rejection: no money moved. Direct
				// best-effort cleanup of the vault created for THIS attempt,
				// NOT an intent (#674 tail): referenced nowhere, harmless if lost.
				rollbackNewSubscription()
				if createdVault && resolvedMethod != nil && s.VaultService != nil {
					_ = s.VaultService.CleanupVaultBestEffort(ctx, resolvedMethod)
				}
				prorationErr := fmt.Errorf("failed to charge proration: %w", err)
				_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, prorationErr)
				return nil, prorationErr
			}
		}

		log.WithFields(log.Fields{
			"user_id":        user.ID,
			"transaction_id": prorationTransactionID,
			"amount":         prorationAmount,
			"rail":           provider,
		}).Info("charged upgrade proration")
	}

	// #297: anchor captures (write-once, best-effort). The proration sale
	// anchors the unscheduled sequence; the successor's first-charge txn (""
	// under the delayed start used here — then the first dunning MIT anchors
	// instead) the recurring one.
	s.captureStoredCredentialRef(ctx, resolvedMethod, charge.AgreementUnscheduled, prorationTransactionID)
	s.captureStoredCredentialRef(ctx, resolvedMethod, charge.AgreementRecurring, resp.TransactionID)

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
		CustomerID:               customerID,
		ProductID:                newPrice.ProductID,
		PriceID:                  newPrice.ID,
		EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(newProduct.EntitlementsSpec),
		CreditsSpecSnapshot:      models.CloneCreditsSpec(newProduct.CreditsSpec),
		RailSubscriptionID:       resp.SubscriptionID,
		Status:                   models.StatusActive, // Active immediately since user paid proration
		Rail:                     models.Rail(provider),
		UserEmail:                emailPtr,
		StartedAt:                now,
		// #268: Model B resets the period — fresh full period [now, now+cycle].
		CurrentPeriodStartsAt: &newPeriodStart,
		CurrentPeriodEndsAt:   &newPeriodEnd,
	}

	if resolvedMethod != nil {
		newSubscription.PaymentMethodID = &resolvedMethod.ID
	}

	// Persist the swap ATOMICALLY: the partial unique index
	// uq_subscriptions_customer_tier_group_active allows only one live
	// subscription per (payable subject, tier group), so the old row's cancel
	// and the new row's insert must commit together (cancel first). On failure
	// the transaction rolls back: the old subscription stays active locally and
	// at NMI (SEC-10 — nothing to reactivate), and compensation only refunds
	// the proration and cancels the new NMI subscription.
	cancelType := models.CancelType("upgrade")
	existingSub.Status = models.StatusCancelled
	existingSub.CancelledAt = &now
	existingSub.CancelType = &cancelType
	existingSub.CancelFeedback = nil
	existingSub.ClearRetrySchedule()
	if err := s.SubscriptionService.ReplaceForTierChange(ctx, existingSub, newSubscription); err != nil {
		saveErr := fmt.Errorf("failed to save upgraded subscription: %w", err)
		// Post-charge DB failure: the user was charged the proration and a new
		// subscription is live at NMI, but we cannot persist it locally. Compensate
		// by refunding the proration and cancelling the new NMI subscription so the
		// rail state matches the (unchanged) local state.
		s.compensateFailedUpgrade(ctx, provider, prorationTransactionID, newSubscriptionID, resp.SubscriptionID, user.ID, &existingSub.ID, rollbackNewSubscription, saveErr)
		_ = s.IdempotencyService.Fail(ctx, idempOp, idempotencyKey, saveErr)
		return nil, saveErr
	}

	// Step 4: Update entitlements immediately (grant new tier entitlements)
	if s.EntitlementService != nil && newProduct.EntitlementsSpec != nil {
		for entitlementName, durationHours := range newProduct.EntitlementsSpec {
			notBefore := now
			var params entitlements.PushNewEntitlementParams
			if durationHours != nil && *durationHours > 0 {
				d := time.Duration(*durationHours) * time.Hour
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
			"subscription_id":      existingSub.ID,
			"rail_subscription_id": existingSub.RailSubscriptionID,
			"rail":                 provider,
			"event":                "upgrade_old_subscription_cancel_failed",
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
// deterministic rail order references from an idempotency key.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// findAdoptableUpgradeSuccessor re-finds a successor subscription a previous
// (transport-ambiguous) upgrade attempt created at NMI: a live roster entry on
// (vault, plan) unknown locally. Exactly one match is adopted; zero means the
// create verifiably did not land (safe to create); more than one is refused
// (never guess which orphan to adopt — operator attention via the returned
// error, surfaced as ErrCheckoutProcessing).
func (s *CheckoutService) findAdoptableUpgradeSuccessor(ctx context.Context, client *nmi.NMIClient, provider, vaultID, planID, orderID string) (*nmi.AddSubscriptionResponse, bool, error) {
	candidates, err := findUnregisteredRemoteSubscriptions(ctx, s.SubscriptionService, client, provider, vaultID, planID, orderID)
	if err != nil {
		return nil, false, err
	}
	switch len(candidates) {
	case 0:
		return nil, false, nil
	case 1:
		log.WithFields(log.Fields{
			"rail_subscription_id": candidates[0],
			"rail":                 provider,
			"order_id":             orderID,
			"event":                "upgrade_successor_adopted",
		}).Info("adopted successor NMI subscription from a previous ambiguous upgrade attempt")
		return &nmi.AddSubscriptionResponse{SubscriptionID: candidates[0]}, true, nil
	default:
		return nil, false, fmt.Errorf("%d unregistered remote subscriptions match vault %s plan %s; operator attention required", len(candidates), vaultID, planID)
	}
}

// compensateFailedUpgrade rolls back rail-side state after a post-charge DB
// failure during an NMI tier upgrade: it refunds the proration charge and cancels
// the newly created NMI subscription so the rail matches the unchanged local
// state. Each step is best-effort; any failure is logged at error level with a
// structured event so operators can finish the repair manually.
func (s *CheckoutService) compensateFailedUpgrade(
	ctx context.Context,
	provider string,
	prorationTransactionID string,
	newSubscriptionID uuid.UUID,
	newRailSubscriptionID string,
	userID string,
	oldSubscriptionID *uuid.UUID,
	rollbackNewSubscription func(),
	cause error,
) {
	logEntry := log.WithError(cause).WithFields(log.Fields{
		"user_id":                  userID,
		"new_subscription_id":      newSubscriptionID,
		"new_rail_subscription_id": newRailSubscriptionID,
		"proration_transaction_id": prorationTransactionID,
		"rail":                     provider,
		"event":                    "upgrade_compensation",
	})
	if oldSubscriptionID != nil {
		logEntry = logEntry.WithField("old_subscription_id", *oldSubscriptionID)
	}
	logEntry.Warn("compensating failed NMI upgrade after post-charge DB error")

	// Refund the proration charge.
	if prorationTransactionID != "" {
		client, cerr := s.resolveNMIClient(ctx, provider)
		if cerr != nil || client == nil {
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
	target railTarget,
) (*CheckoutResponse, error) {
	// CCBill handles downgrades via their own flow
	if target.Rail == "ccbill" {
		return &CheckoutResponse{
			Status:  "blocked",
			Message: "CCBill subscription downgrades are not supported. Please cancel your current subscription and wait for it to expire, then subscribe to the lower tier.",
		}, nil
	}

	// Solana doesn't support subscriptions
	if target.Rail == "solana" {
		return nil, errors.New("solana does not support subscription downgrades")
	}

	// Only NMI-backed rails support programmatic downgrades
	if !rails.IsNMI(models.Rail(target.Rail)) {
		return nil, fmt.Errorf("unsupported rail for downgrades: %s", target.Rail)
	}

	// Validate the new price has NMI configuration
	if _, err := requireNMIPlanForTarget(newPrice, target); err != nil {
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
// UNITS: oldFull/newFull and the returned first charge are MICROS. The unused
// credit is rounded UP to a whole cent (customer-favored), so for whole-cent
// prices the first charge is a whole number of cents — chargeable on every
// rail (NMI cents, Stripe cents, Solana base units) with preview == charge.
//
//	oldUnused   = ceilToCent(oldFull * hoursRemaining / cycleHours) // integer math
//	firstCharge = newFull - oldUnused                               // clamped to >= 0
//
// where hoursRemaining is the number of WHOLE hours left in the current paid
// period (0 if the period has already ended or periodEndsAt is nil).
//
// Example: $20 -> $50, 2 days into a 30-day cycle => hoursRemaining=672,
// oldUnused = ceilToCent(20_000_000*672/720) = 18_670_000 micros, firstCharge =
// 50_000_000-18_670_000 = 31_330_000 micros ($31.33). The new period becomes
// [now, now+30d] and the next bill is $50.
//
// Boundary behavior:
//   - 0 hours remaining           => firstCharge = newFull
//   - full period remaining       => firstCharge = newFull - oldFull
//
// This helper is intentionally pure (no receiver state) so other rails
// (e.g. the Solana path in #267) can reuse the exact same math. cycleHours is
// returned so callers can advance the period end (now + cycleHours).
func CalculateModelBUpgradeCharge(
	oldFull int64,
	newFull int64,
	periodEndsAt *time.Time,
	billingCycleHours *int,
	now time.Time,
) (firstChargeMicros int64, cycleHours int) {
	// Default to a 30-day (720h) cycle if not specified.
	cycleHours = 30 * 24
	if billingCycleHours != nil && *billingCycleHours > 0 {
		cycleHours = *billingCycleHours
	}

	// Whole hours remaining in the current paid period.
	hoursRemaining := 0
	if periodEndsAt != nil && periodEndsAt.After(now) {
		hoursRemaining = int(periodEndsAt.Sub(now).Hours())
		if hoursRemaining < 0 {
			hoursRemaining = 0
		}
	}
	// Never credit more than a full cycle of unused time.
	if hoursRemaining > cycleHours {
		hoursRemaining = cycleHours
	}

	// Credit for the unused portion of the OLD plan (integer math to avoid
	// floating-point drift), rounded UP to a whole cent (customer-favored) so
	// the resulting charge is whole-cent for whole-cent prices.
	oldUnused := (oldFull * int64(hoursRemaining)) / int64(cycleHours)
	oldUnused = moneyutil.MicrosToCentsCeil(oldUnused) * moneyutil.MicrosPerCent

	firstChargeMicros = newFull - oldUnused
	if firstChargeMicros < 0 {
		// Defensive clamp. For a genuine upgrade newFull > oldFull so this is
		// only reachable with bad inputs (e.g. a "downgrade" routed here).
		firstChargeMicros = 0
	}
	return firstChargeMicros, cycleHours
}

// cancelNMISubscription cancels a subscription at NMI
func (s *CheckoutService) cancelNMISubscription(ctx context.Context, sub *models.Subscription, provider string) error {
	client, err := s.resolveNMIClient(ctx, provider)
	if err != nil {
		return fmt.Errorf("NMI provider '%s' is not configured: %w", provider, err)
	}

	if err := client.DeleteRecurringSubscription(sub.RailSubscriptionID); err != nil {
		return err
	}
	return nil
}

// TierChange processes a subscription tier change (upgrade or downgrade).
// This is the unified entry point that routes to rail-specific implementations.
func (s *CheckoutService) TierChange(ctx context.Context, req *TierChangeRequest, user *UserIdentity) (*TierChangeResponse, error) {
	// 1. Parse and validate price (#774: price_id accepts a price_key too)
	newPrice, err := catalog.ResolveReference(ctx, s.PriceService, req.PriceID)
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
		// Verify ownership: compare PARSED subject ids, not raw strings — the
		// caller's id is a UUID (boundary-enforced, #364) but may differ in
		// case/format from the canonical String() form.
		if payer := identity.CustomerIDFromString(user.ID); payer.IsZero() || existingSub.CustomerID != payer.UUID() {
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

	// 6. Route to rail-specific handler based on config type detection
	// This allows adding new NMI providers via config without code changes
	rail := string(existingSub.Rail)

	switch {
	case rail == "stripe":
		return s.processTierChangeStripe(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	case rails.IsNMI(models.Rail(rail)):
		return s.processTierChangeNMI(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	case rail == "ccbill":
		return s.processTierChangeCCBill(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	case rail == "solana":
		return s.processTierChangeSolana(ctx, req, user, newPrice, newProduct, existingSub, currentProduct, action)
	default:
		return nil, &TierChangeError{
			HTTPStatus: http.StatusBadRequest,
			Message:    fmt.Sprintf("unsupported rail: %s", rail),
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
	// #774: price_id accepts a price_key too.
	newPrice, err := catalog.ResolveReference(ctx, s.PriceService, req.PriceID)
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
		if payer := identity.CustomerIDFromString(user.ID); err != nil || payer.IsZero() || existingSub.CustomerID != payer.UUID() {
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

	rail := string(existingSub.Rail)
	now := s.now()
	resp := &TierChangePreviewResponse{
		Object:           "tier_change_preview",
		PriceID:          api.FormatPriceID(newPrice.ID),
		Rail:             rail,
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
	firstCharge, cycleHours := CalculateModelBUpgradeCharge(currentPrice.Amount, newPrice.Amount, existingSub.CurrentPeriodEndsAt, newPrice.RecurringCycleHours(), now)
	nextDate := now.Add(time.Duration(cycleHours) * time.Hour)
	resp.Action = "upgrade"
	resp.AmountDueNow = firstCharge
	resp.Effective = "now"
	resp.NextChargeDate = &nextDate
	resp.IsEstimate = rail == "stripe"
	resp.Message = fmt.Sprintf("You'll be charged %s now and %s on %s.",
		formatMinorAmount(firstCharge, newPrice.Currency),
		formatMinorAmount(newPrice.Amount, newPrice.Currency),
		nextDate.Format("January 2, 2006"))
	return resp, nil
}

// formatMinorAmount renders an internal micro-unit amount as currency copy,
// e.g. (60_010_000,"usd") -> "$60.010000".
func formatMinorAmount(micros int64, currency string) string {
	symbol := "$"
	if !strings.EqualFold(strings.TrimSpace(currency), "usd") {
		symbol = strings.ToUpper(strings.TrimSpace(currency)) + " "
	}
	amount := moneyutil.FormatMicrosDecimal(micros)
	if strings.HasPrefix(amount, "-") {
		return "-" + symbol + strings.TrimPrefix(amount, "-")
	}
	return symbol + amount
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
	if strings.TrimSpace(existingSub.RailSubscriptionID) == "" {
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
				Payment: CheckoutSessionPaymentResponse{Rail: "stripe"},
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

		stripeService := &subscriptions.StripeService{Config: s.Config, Rails: s.Rails}
		if _, err := stripeService.ScheduleSubscriptionPriceChange(ctx, existingSub.RailSubscriptionID, currentStripePriceID, stripePriceID, currentPeriodStart, *existingSub.CurrentPeriodEndsAt, newPrice.RecurringCycleDays()); err != nil {
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
			Payment:          CheckoutSessionPaymentResponse{Rail: "stripe"},
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
	stripeService := &subscriptions.StripeService{Config: s.Config, Rails: s.Rails}
	itemID, err := stripeService.GetSubscriptionItemID(ctx, existingSub.RailSubscriptionID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
	}
	// Pass newPrice.ID so the subscription's metadata[internal_price_id] is
	// rewritten to the new tier; otherwise the proration invoice and every future
	// renewal would resolve the stale old price in the invoice.paid webhook (#268).
	if err := stripeService.UpdateSubscriptionPrice(ctx, existingSub.RailSubscriptionID, itemID, stripePriceID, newPrice.ID.String(), "always_invoice", "now"); err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
	}

	// Update local subscription record.
	// #268: Model B reset the billing cycle at Stripe (anchor "now"), so reflect
	// the fresh period [now, now+cycle] locally. Stripe webhooks remain the
	// source of truth and will reconcile the exact period boundaries.
	stripeNow := s.now()
	stripeCycleHours := 30 * 24
	if ch := newPrice.RecurringCycleHours(); ch != nil {
		stripeCycleHours = *ch
	}
	stripePeriodEnd := stripeNow.Add(time.Duration(stripeCycleHours) * time.Hour)

	// Capture the OLD price + period BEFORE the local reset below overwrites them,
	// so the success-toast estimate matches the preview's now-amount. Stripe
	// finalizes the exact prorated total on its side, so this is an estimate.
	var oldFull int64
	if existingSub.Price != nil {
		oldFull = existingSub.Price.Amount
	}
	oldPeriodEnd := existingSub.CurrentPeriodEndsAt
	estimatedNow, _ := CalculateModelBUpgradeCharge(oldFull, newPrice.Amount, oldPeriodEnd, newPrice.RecurringCycleHours(), stripeNow)

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
		Payment:          CheckoutSessionPaymentResponse{Rail: "stripe"},
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
	endpoint := fmt.Sprintf("POST /v1/me/subscriptions/%s/solana-tier-change", subIDStr)
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
		Payment:        CheckoutSessionPaymentResponse{Rail: "solana"},
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
		Rail:           string(existingSub.Rail),
		IdempotencyKey: req.IdempotencyKey,
	}
	if existingSub.PaymentMethodID != nil {
		checkoutReq.PaymentMethodID = api.FormatPaymentMethodID(*existingSub.PaymentMethodID)
	}

	// Route to existing methods which handle the heavy lifting. The existing
	// subscription's rail resolves through arming to its payment provider.
	target, err := s.resolveRailTarget(ctx, string(existingSub.Rail))
	if err != nil {
		return nil, err
	}
	var checkoutResp *CheckoutResponse
	if action == "upgrade" {
		checkoutResp, err = s.processUpgrade(ctx, checkoutReq, user, newPrice, newProduct, existingSub, target)
	} else {
		checkoutResp, err = s.processDowngrade(ctx, checkoutReq, user, newPrice, newProduct, existingSub, target)
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
			Payment: CheckoutSessionPaymentResponse{Rail: "ccbill"},
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
			Rail:        "ccbill",
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

// requireNMIPlanForTarget returns the NMI plan the resolved payment provider
// charges for this price: the provider's own link entry (by account key), or
// the rail-named entry when the manifest declared it under the rail. Never a
// rail-wide scan — with several accounts on a rail, each provider's plan is
// its own.
func requireNMIPlanForTarget(price *models.Price, target railTarget) (string, error) {
	if price == nil {
		return "", errors.New("price is required")
	}
	lookup := func(key string) (string, bool) {
		cfg := price.PSPLinks[key]
		if cfg == nil || !strings.EqualFold(strings.TrimSpace(cfg[models.RailKeyRail]), target.Rail) {
			return "", false
		}
		id := strings.TrimSpace(cfg[models.RailKeyPlanID])
		return id, id != ""
	}
	if id, ok := lookup(target.PSP); ok {
		return id, nil
	}
	if target.Scope == nil && target.PSP != target.Rail {
		if id, ok := lookup(target.Rail); ok {
			return id, nil
		}
	}
	return "", fmt.Errorf("price %s is missing NMI plan configuration for payment provider %s (rail %s)", price.ID, target.PSP, target.Rail)
}

// captureStoredCredentialRef persists a stored-credential sequence anchor for
// an instrument (#297), write-once and best-effort — a miss just means the
// next successful charge on that agreement type re-captures.
func (s *CheckoutService) captureStoredCredentialRef(ctx context.Context, pm *models.PaymentMethod, agreement charge.Agreement, ref string) {
	ref = strings.TrimSpace(ref)
	if pm == nil || ref == "" {
		return
	}
	if s.VaultService == nil || s.VaultService.DB == nil {
		log.WithContext(ctx).Warn("checkout: no DB handle to persist stored-credential reference (#297)")
		return
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("checkout: no merchant context to persist stored-credential reference (#297)")
		return
	}
	if _, err := s.VaultService.DB.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{
		MerchantID: tid.UUID(),
		ID:         pm.ID,
		Agreement:  string(agreement),
		Ref:        ref,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithField("payment_method_id", pm.ID).
			Warn("checkout: failed to persist stored-credential reference (#297); next charge re-captures")
	}
}
