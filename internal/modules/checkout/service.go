package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	identity "github.com/open-rails/openrails/internal/billingidentity"
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
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/cardholdername"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
	log "github.com/sirupsen/logrus"
)

// TierChangeResponse represents the response from a tier change operation.
// This reuses the CheckoutSessionResponse envelope pattern for API consistency.
type TierChangeResponse = openrails.TierChangeResponse

// TierChangePreviewResponse is the non-mutating dry-run of a tier change: it
// reports what a confirm WOULD charge now and at the next renewal, without
// touching Stripe or the local subscription. The frontend renders it as a
// "Right now: $X / On <date>: $Y" confirmation before calling change-tier.
type TierChangePreviewResponse = openrails.TierChangePreviewResponse

// CheckoutService handles unified checkout for subscriptions and one-time purchases
type CheckoutService struct {
	SubscriptionService      *subscriptions.SubscriptionService
	ProductService           *catalog.ProductService
	PriceService             *catalog.PriceService
	PaymentService           *payments.PaymentService
	EntitlementService       *entitlements.EntitlementService
	PurchaseService          *CheckoutPurchaseService
	PaymentMethodResolver    *CheckoutPaymentMethodResolver
	NMISaleService           *CheckoutNMISaleService
	CustodianSaleService     *CheckoutCustodianSaleService
	PaymentMethodService     *paymentmethods.PaymentMethodService
	RailPaymentMethodService *paymentmethods.RailPaymentMethodService
	IdempotencyService       checkoutIdempotencyStore
	MerchantSecrets          merchants.MerchantSecretReader
	ProviderSecrets          merchants.PSPSecretResolver
	// RailCustomerService maps app users to rail customer ids so we
	// reuse a single Stripe customer per user (issue #212) and can record the
	// mapping at checkout time instead of relying solely on webhooks.
	RailCustomerService *payments.RailCustomerService
	// StripeService is used to resolve/create the Stripe customer and to run the
	// webhook-independent duplicate guard (issue #213).
	StripeService *subscriptions.StripeService
	// Intents executes durable write-ahead provider intents (#674). Every NMI
	// recurring create in this flow goes through it.
	Intents   intentExecutor
	Lifecycle *subscriptions.SubscriptionLifecycleService
	clock     clockwork.Clock
	Config    *config.Config
	Rails     railresolve.Source
	// NMIEndpointOverride points store-armed NMI clients at a fake gateway
	// (test seam; empty = real endpoints).
	NMIEndpointOverride string
	// ResolveNMIClientOverride replaces the store-armed NMI client resolution
	// entirely (test seam; nil = the scoped resolution).
	ResolveNMIClientOverride func(context.Context, string) (*nmi.NMIClient, error)
}

func (s *CheckoutService) SetSubscriptionLifecycleService(l *subscriptions.SubscriptionLifecycleService) {
	s.Lifecycle = l
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
	railPMService *paymentmethods.RailPaymentMethodService,
	idempotencyService checkoutIdempotencyStore,
	railCustomerService *payments.RailCustomerService,
	cfg *config.Config,
	railSet railresolve.Source,
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
		PaymentMethodResolver:    NewCheckoutPaymentMethodResolver(paymentMethodService, railPMService),
		PaymentMethodService:     paymentMethodService,
		RailPaymentMethodService: railPMService,
		IdempotencyService:       idempotencyService,
		RailCustomerService:      railCustomerService,
		StripeService:            &subscriptions.StripeService{Config: cfg, Rails: railSet},
		clock:                    clock,
		Config:                   cfg,
		Rails:                    railSet,
	}
	service.NMISaleService = NewCheckoutNMISaleService(service.PurchaseService, service.PaymentMethodResolver, railPMService)
	// The scoped resolver is the ONLY NMI client source (#788); armed for
	// real once SetMerchantSecretStore wires the merchant secret store.
	service.NMISaleService.ResolveNMIClient = service.resolveNMIClient
	service.CustodianSaleService = &CheckoutCustodianSaleService{
		PurchaseService:      service.PurchaseService,
		PaymentMethodService: paymentMethodService,
		IdempotencyStore:     idempotencyService,
		Rails:                railSet,
		Config:               cfg,
	}
	if railPMService != nil {
		service.CustodianSaleService.DB = railPMService.DB
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
	if response, found, err := s.replayInitialMembership(ctx, req, user); found || err != nil {
		return response, err
	}
	if s.NMISaleService != nil {
		if response, found, err := s.NMISaleService.replayAcceptedRequest(ctx, req, user); found || err != nil {
			return response, err
		}
	}
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

	// #704: pin the active PSP for this rail so payment /
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
	if s.Config != nil && s.Config.NewSubscriptionCollectionPolicy == "engine" {
		return nil, errors.New("new engine subscriptions require a saved-method checkout session and explicit agreement confirmation")
	}
	price = priceForCheckoutTarget(price, target)
	switch {
	case target.Rail == "ccbill":
		return s.processCCBillSubscription(ctx, req, user, price)
	case rails.IsNMI(models.Rail(target.Rail)):
		if custodianHeld(target) {
			// #795: a custodian-held card has no provider-side recurring engine
			// (the NMI vault subscription needs NMI to hold the card) and the
			// OpenRails-driven renewal worker for seam charges is not built yet
			// — enrolling would strand renewals. Loud error, never a silent accept.
			return nil, errors.New("subscriptions are not supported on custodian-held cards yet (renewals are engine-driven; see #795)")
		}
		return s.processNMISubscription(ctx, req, user, price, product, coverage, target)
	case target.Rail == "stripe":
		return s.processStripeSubscription(ctx, req, user, price, coverage)
	case target.Rail == "solana":
		return nil, errors.New("solana does not support recurring subscriptions; use a one-time price instead")
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
	price = priceForCheckoutTarget(price, target)
	switch {
	case rails.IsNMI(models.Rail(target.Rail)):
		if custodianHeld(target) {
			// or#879: same rail, same gateway — the card is held by a custodian,
			// so the sale goes through its detokenizing proxy. The PSP decides
			// this, not a separate rail value.
			if s.CustodianSaleService == nil {
				return nil, errors.New("custodian-held card checkout is not configured")
			}
			ctx = db.WithCustodianID(ctx, *target.Scope.CustodianID)
			idempotencyKey := s.getIdempotencyKey(req, user.ID, price.ID, "custodian_sale")
			return s.CustodianSaleService.Process(ctx, req, user, price, product, idempotencyKey)
		}
		return s.processNMISale(ctx, req, user, price, product, coverage, target)
	case target.Rail == "solana":
		return s.processSolanaPurchase(ctx, req, user, price, product, coverage)
	case target.Rail == "ccbill":
		return nil, errors.New("ccbill does not support one-time purchases; use a subscription price instead")
	case target.Rail == "stripe":
		return s.processStripePayment(ctx, req, user, price, product)
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

	canonicalName := cardholdername.Canonical(req.NameOnCard, req.FirstName, req.LastName)
	if err := validateCCBillBillingIdentity(canonicalName, req.Zip, req.Country, user); err != nil {
		return nil, err
	}

	// User must have a username for CCBill (used for webhook resolution via profiles.users)
	if user.Username == "" {
		return nil, errors.New("username required for CCBill payments")
	}

	firstName, lastName := cardholdername.Parts(canonicalName, "", "")
	flexFormParams := &ccbill.GenerateFlexFormURLParams{
		Username:      user.Username,
		Email:         strings.TrimSpace(*user.Email),
		CustomerFName: firstName,
		CustomerLName: lastName,
		Address1:      strings.TrimSpace(req.Address1),
		City:          strings.TrimSpace(req.City),
		State:         strings.TrimSpace(req.State),
		ZipCode:       strings.TrimSpace(req.Zip),
		Country:       strings.ToUpper(strings.TrimSpace(req.Country)),
		FlexID:        flexID,
		FormName:      formName,
		ReservationID: req.CheckoutSessionID,
		// #819: bill the PRICE's currency. An unbillable/absent currency errors
		// below — before a form exists, therefore before any charge.
		Currency: price.Currency,
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
		Currency:               newPrice.Currency, // #819
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
func (s *CheckoutService) processNMISubscription(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, coverage *CoverageInfo, target railTarget) (*CheckoutResponse, error) {
	if s.Intents == nil {
		return nil, errors.New("checkout enrollment executor unavailable")
	}
	key := s.getIdempotencyKey(req, user.ID, price.ID, "nmi_subscription")
	_, _, method, created, err := s.PaymentMethodResolver.ResolvePaymentMethod(ctx, req, user, target)
	if err != nil {
		return nil, err
	}
	accepted, err := s.admitInitialMembership(ctx, req, user, price.ID, method, target, key)
	if err != nil {
		return nil, err
	}
	fingerprint := saleRequestFingerprint(req, user, price.ID, target)
	intent, err := s.Intents.EnqueueOwnedAndExecute(ctx, initialMembershipReplayParams(accepted), func(in gen.OpenrailsRailIntent) error {
		return ownsInitialMembership(in, user.ID, price.ID, fingerprint, nil)
	})
	if err != nil {
		return nil, err
	}
	if intent.Status == intents.StatusFailedTerminal && created && method != nil && s.RailPaymentMethodService != nil {
		_ = s.RailPaymentMethodService.CleanupPaymentMethodBestEffort(ctx, method)
	}
	return initialMembershipResponseFromIntent(intent)
}

// initialMembershipResponseFromIntent rebuilds the checkout response from a
// succeeded create intent's evidence.
func initialMembershipResponseFromIntent(intent gen.OpenrailsRailIntent) (*CheckoutResponse, error) {
	if intent.Status == intents.StatusSucceeded {
		if err := intents.ValidateInitialMembershipTerminal(intent); err != nil {
			return nil, err
		}
	}
	if intent.Status == intents.StatusFailedTerminal {
		return nil, terminalCheckoutError(intent, "initial enrollment refused")
	}
	if intent.Status != intents.StatusSucceeded {
		return nil, ErrCheckoutProcessing
	}

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
	accepted, err := subscriptions.DecodeInitialMembershipPayload(intent)
	if err != nil {
		return nil, err
	}
	resp := &CheckoutResponse{
		Status:        evidence.Status,
		Action:        "new",
		Message:       evidence.Message,
		TransactionID: evidence.TransactionID,
	}
	if accepted.Terms.PaymentID != uuid.Nil {
		resp.PaymentID = &accepted.Terms.PaymentID
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

func nmiSubscriptionStartDate(coverage *CoverageInfo, now time.Time) (string, *time.Time) {
	if coverage == nil || !coverage.HasCoverage || coverage.EndDate == nil || !coverage.EndDate.After(now) {
		return "", nil
	}
	startDate, startAt := buildNMIFutureStartDate(*coverage.EndDate, now)
	return startDate, &startAt
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
	product *models.Product,
) (*CheckoutResponse, error) {
	_, _, err := subscriptions.RequireStripeSecretKey(ctx, s.Rails)
	if err != nil {
		return nil, err
	}
	var stripePriceID string
	var inline *stripeCheckoutInlinePrice
	if s.Config != nil && s.Config.NewSubscriptionCollectionPolicy == "engine" {
		minor, err := moneyutil.NativeToRailMinorExact(price.Currency, price.Amount)
		if err != nil {
			return nil, err
		}
		if product == nil || product.ID != price.ProductID {
			return nil, errors.New("checkout product does not match accepted price")
		}
		name := strings.TrimSpace(product.DisplayName)
		if name == "" {
			name = product.Key
		}
		inline = &stripeCheckoutInlinePrice{Name: name, Currency: price.Currency, AmountMinor: minor}
	} else {
		stripePriceID, err = getStripePriceID(price)
		if err != nil {
			return nil, err
		}
	}
	successURL := strings.TrimSpace(req.SuccessURL)
	cancelURL := strings.TrimSpace(req.CancelURL)
	if successURL == "" || cancelURL == "" {
		return nil, errors.New("stripe success_url and cancel_url are required")
	}

	urlStr, err := s.createStripeCheckoutSession(ctx, stripeCheckoutParams{
		Mode:              "payment",
		PriceID:           stripePriceID,
		InlinePrice:       inline,
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

type stripeCheckoutInlinePrice struct {
	Name        string
	Currency    string
	AmountMinor moneyutil.Cents
}

type stripeCheckoutParams struct {
	InlinePrice       *stripeCheckoutInlinePrice
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
	if params.InlinePrice != nil {
		if params.Mode != "payment" || params.PriceID != "" || params.InlinePrice.AmountMinor < 0 || strings.TrimSpace(params.InlinePrice.Name) == "" {
			return "", errors.New("invalid inline checkout price")
		}
		values.Set("line_items[0][price_data][currency]", strings.ToLower(params.InlinePrice.Currency))
		values.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(int64(params.InlinePrice.AmountMinor), 10))
		values.Set("line_items[0][price_data][product_data][name]", params.InlinePrice.Name)
	} else {
		values.Set("line_items[0][price]", params.PriceID)
	}
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

// ResolvePaymentMethod gets an existing payment method or creates one from a payment token
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

// processUpgrade runs an NMI tier upgrade (a higher TierRank) as one durable
// nmi_upgrade operation: an immediate successor enrollment plus the prorated
// charge for the remaining period, answered on the tier change contract
// (tier_change_operation.go).
func (s *CheckoutService) processUpgrade(ctx context.Context, req *CheckoutRequest, user *UserIdentity, newPrice *models.Price, newProduct *models.Product, existingSub *models.Subscription, target railTarget) (*TierChangeResponse, error) {
	queryMerchant, queryScopeErr := merchant.Require(ctx)
	if queryScopeErr != nil {
		return nil, queryScopeErr
	}

	newPrice = priceForCheckoutTarget(newPrice, target)
	if !rails.IsNMI(models.Rail(target.Rail)) {
		return nil, fmt.Errorf("unsupported rail for upgrades: %s", target.Rail)
	}
	if s.Intents == nil || s.Lifecycle == nil {
		return nil, errors.New("durable upgrade service unavailable")
	}
	if target.Scope == nil || target.Scope.ID != existingSub.PspID {
		return nil, errors.New("upgrade must use the predecessor's PSP account")
	}
	ctx = db.WithPSPID(ctx, existingSub.PspID)
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return nil, tierChangeKeyRequired()
	}
	key := tierChangeIdempotencyKey(req.IdempotencyKey)
	// Replays use the original durable payload, even if pricing or time changed.
	database := s.SubscriptionService.Database()
	prior, err := intents.NewStore(database).GetByIdempotencyKey(ctx, key)
	if err == nil {
		return s.replayTierChangeOperation(ctx, prior, &TierChangeRequest{SubscriptionID: existingSub.ID, PriceID: openrails.PriceID(newPrice.ID).String()}, user)
	}
	if !db.IsNotFound(err) {
		return nil, err
	}
	if existingSub.Price == nil || existingSub.CurrentPeriodEndsAt == nil {
		return nil, errors.New("existing subscription missing price or period")
	}
	customerID, err := customerIDFromUser(user.ID)
	if err != nil {
		return nil, err
	}
	if existingSub.CustomerID != customerID {
		return nil, errors.New("upgrade predecessor belongs to another customer")
	}
	now := s.now().UTC()
	cycle := newPrice.RecurringCycleHours()
	if cycle == nil {
		cycle = existingSub.Price.RecurringCycleHours()
	}
	amount, hours, err := CalculateModelBUpgradeCharge(PriceAmountOf(existingSub.Price), PriceAmountOf(newPrice), existingSub.CurrentPeriodEndsAt, cycle, now)
	if err != nil {
		return nil, err
	}
	if _, err = moneyutil.NativeToRailMinorExact(newPrice.Currency, amount); err != nil {
		return nil, err
	}
	if _, err = moneyutil.NativeToRailMinorExact(newPrice.Currency, newPrice.Amount); err != nil {
		return nil, err
	}
	plan, err := requireNMIPlanForTarget(newPrice, target)
	if err != nil {
		return nil, err
	}
	vault, billing, method, _, err := s.PaymentMethodResolver.ResolvePaymentMethod(ctx, req, user, target)
	if err != nil {
		return nil, err
	}
	if method == nil {
		return nil, errors.New("upgrade requires a stored payment method")
	}
	methodRow, err := s.SubscriptionService.Database().Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: queryMerchant.UUID(), ID: method.ID})
	if err != nil {
		return nil, err
	}
	if methodRow.CustomerID != customerID || methodRow.PspID != existingSub.PspID || methodRow.Custodian != models.CustodianPSP || methodRow.RailCustomerRef != vault || methodRow.RailMethodRef != billing {
		return nil, errors.New("upgrade instrument does not match accepted customer and provider account")
	}
	end := now.Add(time.Duration(hours) * time.Hour)
	startDate, _ := buildNMIFutureStartDate(end, now)
	payload := subscriptions.NMIUpgradePayload{RequestedPrice: strings.TrimSpace(req.PriceID), PSP: target.PSP, UserID: user.ID, Email: req.Email, OldSubscriptionID: existingSub.ID, OldPriceID: existingSub.PriceID, OldProviderSubscriptionID: existingSub.RailSubscriptionID, NewSubscriptionID: uuidutil.NewV7(), NewPaymentID: uuidutil.NewV7(), PriceID: newPrice.ID, ProductID: newProduct.ID, ProductName: newProduct.DisplayName, PlanID: plan, Instrument: charge.FreezeInstrument(methodRow), PaymentMethodID: method.ID, RecurringAmount: newPrice.Amount, ProrationAmount: amount, Currency: newPrice.Currency, PeriodStart: now, PeriodEnd: end, StartDate: startDate, Entitlements: models.CloneEntitlementsSpec(newProduct.EntitlementsSpec), Card: nmi.CardUserData{FirstName: ResolveCheckoutFirstName(req, user), LastName: ResolveCheckoutLastName(req), Address1: DefaultIfEmpty(req.Address1, "N/A"), City: DefaultIfEmpty(req.City, "N/A"), State: DefaultIfEmpty(req.State, "N/A"), Zip: DefaultIfEmpty(req.Zip, "00000"), Country: DefaultIfEmpty(req.Country, "US")}}
	intent, err := s.Intents.EnqueueOwnedAndExecute(ctx, intents.EnqueueParams{MerchantID: existingSub.MerchantID, Provider: target.Rail, PspID: existingSub.PspID, IntentType: TypeNMIUpgrade, SubscriptionID: &existingSub.ID, PriceID: &newPrice.ID, Payload: payload, IdempotencyKey: key, NextAttemptAt: now, Origin: intents.OriginUser, OriginReason: "customer tier upgrade"},
		func(row gen.OpenrailsRailIntent) error { return tierChangeOwnedBy(row, nmiUpgradeSubject(payload)) })
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == tierChangeSubjectConstraint {
		// Another unresolved tier change owns this predecessor's provider steps.
		return nil, s.tierChangeInFlight(ctx, existingSub.ID)
	}
	if err != nil {
		return nil, err
	}
	return tierChangeResponse(intent)
}

// shortHash returns a stable 16-hex-char digest of s, used to build
// deterministic rail order references from an idempotency key.
func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
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

	// A short subscription transaction serializes the change with accepted
	// recurring recovery. It must not overwrite a prepared quote or a newer
	// subscription snapshot from checkout's earlier preflight.
	var err error
	existingSub, err = subscriptions.NewSubscriptionRepo(s.SubscriptionService.Database()).SchedulePriceChange(ctx, existingSub.ID, existingSub.PriceID, newPrice.ID)
	if err != nil {
		return nil, err
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
// CURRENCY (#820): `newFull - oldUnused` is only meaningful inside ONE
// currency — across an FX boundary the subtraction silently invents a rate of
// 1.0. Both operands therefore arrive as PriceAmount (amount + currency) and a
// mismatched or absent currency returns ErrTierChangeCrossCurrency with NO
// amount, matching how reprice and plan migration refuse an FX crossing.
//
// UNITS: the amounts and the returned first charge are MICROS. The unused
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
	old PriceAmount,
	new PriceAmount,
	periodEndsAt *time.Time,
	billingCycleHours *int,
	now time.Time,
) (firstChargeMicros int64, cycleHours int, err error) {
	if err := RequireSameCurrency(old, new); err != nil {
		return 0, 0, err
	}
	oldFull, newFull := old.Micros, new.Micros

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
	// The product can exceed int64 even though the quotient cannot: remaining
	// hours are clamped to the positive cycle, so valid nonnegative prices
	// yield 0 <= oldUnused <= oldFull. Widen before multiplying, keep the
	// existing integer truncation, and check before narrowing.
	var unused big.Int
	unused.Mul(big.NewInt(oldFull), big.NewInt(int64(hoursRemaining)))
	unused.Quo(&unused, big.NewInt(int64(cycleHours)))
	if !unused.IsInt64() {
		return 0, 0, fmt.Errorf("unused subscription value exceeds int64 precision")
	}
	oldUnused := unused.Int64()
	// or#863: ceil to a whole RAIL MINOR unit at this price's own currency
	// scale, then back to internal units — not an inline /10_000 that assumes
	// every currency is 2-decimal. RequireSameCurrency above already proved the
	// currency is registered and shared by both operands.
	oldUnusedMinor, err := moneyutil.NativeToRailMinor(old.Currency, oldUnused)
	if err != nil {
		return 0, 0, err
	}
	oldUnused, err = moneyutil.RailMinorToNative(old.Currency, oldUnusedMinor)
	if err != nil {
		return 0, 0, err
	}

	firstChargeMicros = newFull - oldUnused
	if firstChargeMicros < 0 {
		// Defensive clamp. For a genuine upgrade newFull > oldFull so this is
		// only reachable with bad inputs (e.g. a "downgrade" routed here).
		firstChargeMicros = 0
	}
	return firstChargeMicros, cycleHours, nil
}

// TierChange processes a subscription tier change (upgrade or downgrade).
// This is the unified entry point that routes to rail-specific implementations.
func (s *CheckoutService) TierChange(ctx context.Context, req *TierChangeRequest, user *UserIdentity) (*TierChangeResponse, error) {
	if response, found, err := s.ReplayTierChange(ctx, req, user); found || err != nil {
		return response, err
	}
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
	if err := validateTierChangeSubscriptionStatus(existingSub); err != nil {
		return nil, err
	}
	// One unresolved tier change owns the subscription on every rail: a
	// request under another key is pointed at it.
	if err := s.refuseTierChangeInFlight(ctx, existingSub.ID); err != nil {
		return nil, err
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
	// A tier group may mix currencies (the catalog allows a per-currency price
	// per plan), but a SUBSCRIPTION cannot move across one: proration would
	// subtract across an FX boundary on the way up, and the schedule would
	// silently change currency on the way down (#820).
	if err := RequireSameCurrency(PriceAmountOf(currentPrice), PriceAmountOf(newPrice)); err != nil {
		return nil, err
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
	if err := validateTierChangeSubscriptionStatus(existingSub); err != nil {
		return nil, err
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
	// Same FX refusal as TierChange (#820) — the preview must never quote a
	// number the charge would refuse to honour.
	if err := RequireSameCurrency(PriceAmountOf(currentPrice), PriceAmountOf(newPrice)); err != nil {
		return nil, err
	}

	rail := string(existingSub.Rail)
	now := s.now()
	resp := &TierChangePreviewResponse{
		Object:           "tier_change_preview",
		PriceID:          openrails.PriceID(newPrice.ID),
		Rail:             rail,
		Currency:         newPrice.Currency,
		NextChargeAmount: newPrice.Amount,
	}

	if newProduct.TierRank < currentProduct.TierRank {
		if err := s.validateTierChangePreviewTarget(ctx, existingSub, currentPrice, newPrice, user, "downgrade"); err != nil {
			return nil, err
		}
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

	if err := s.validateTierChangePreviewTarget(ctx, existingSub, currentPrice, newPrice, user, "upgrade"); err != nil {
		return nil, err
	}

	// Upgrade: Model B reset-period — charge now, rebill the full price at now+cycle.
	firstCharge, cycleHours, err := CalculateModelBUpgradeCharge(PriceAmountOf(currentPrice), PriceAmountOf(newPrice), existingSub.CurrentPeriodEndsAt, newPrice.RecurringCycleHours(), now)
	if err != nil {
		return nil, err
	}
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

func validateTierChangeSubscriptionStatus(subscription *models.Subscription) error {
	if subscription.Status == models.StatusActive || subscription.Status == models.StatusPastDue {
		return nil
	}
	return &TierChangeError{
		HTTPStatus: http.StatusConflict,
		Message:    "only active or past-due subscriptions can change tier",
	}
}

func (s *CheckoutService) validateTierChangePreviewTarget(
	ctx context.Context,
	existingSub *models.Subscription,
	currentPrice, newPrice *models.Price,
	user *UserIdentity,
	action string,
) error {
	switch {
	case existingSub.Rail == models.RailStripe:
		if _, ok := newPrice.GetStripeConfig(); !ok {
			return &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "target price not configured for Stripe"}
		}
		if action == "downgrade" {
			if _, ok := currentPrice.GetStripeConfig(); !ok {
				return &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "current price not configured for Stripe"}
			}
		}
	case rails.IsNMI(existingSub.Rail):
		target, err := s.resolveRailTargetForPSP(ctx, string(existingSub.Rail), existingSub.PspID)
		if err != nil {
			return err
		}
		if _, err := requireNMIPlanForTarget(newPrice, target); err != nil {
			return err
		}
	case existingSub.Rail == models.RailCCBill:
		if action == "downgrade" {
			return &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "CCBill subscription downgrades are not supported"}
		}
		if _, _, ok := newPrice.GetCCBillFlexForm(); !ok {
			return &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "target price is not configured for CCBill"}
		}
		if user.Email == nil || strings.TrimSpace(*user.Email) == "" || strings.TrimSpace(user.Username) == "" {
			return &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "verified customer email and username are required for CCBill"}
		}
	case existingSub.Rail == models.RailSolana:
		if !priceHasSolanaRecurring(newPrice) {
			return &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "target price is not configured for Solana recurring billing"}
		}
	default:
		return &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: fmt.Sprintf("unsupported rail: %s", existingSub.Rail)}
	}
	return nil
}

// formatMinorAmount renders an internal micro-unit amount as currency copy,
// e.g. (60_010_000,"usd") -> "$60.010000".
func formatMinorAmount(micros int64, currency string) string {
	symbol := "$"
	if !strings.EqualFold(strings.TrimSpace(currency), "usd") {
		symbol = strings.ToUpper(strings.TrimSpace(currency)) + " "
	}
	amount := moneyutil.FormatMicrosDecimal(moneyutil.Micros(micros))
	if strings.HasPrefix(amount, "-") {
		return "-" + symbol + strings.TrimPrefix(amount, "-")
	}
	return symbol + amount
}

// processTierChangeStripe freezes a Stripe tier change and runs it as a
// durable operation (stripe_tier_change_intent.go). Upgrades are billed
// immediately with a reset cycle; downgrades are scheduled for period end so
// the customer keeps the tier already paid for.
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
	stripePriceID, ok := newPrice.GetStripeConfig()
	if !ok || strings.TrimSpace(stripePriceID) == "" {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "target price not configured for Stripe"}
	}
	if strings.TrimSpace(existingSub.RailSubscriptionID) == "" {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "subscription missing Stripe reference"}
	}
	currentPrice := existingSub.Price
	if currentPrice == nil {
		var err error
		if currentPrice, err = s.PriceService.GetByID(ctx, existingSub.PriceID); err != nil {
			return nil, &TierChangeError{HTTPStatus: http.StatusInternalServerError, Message: "current price not found"}
		}
	}
	currentStripePriceID, ok := currentPrice.GetStripeConfig()
	if !ok || strings.TrimSpace(currentStripePriceID) == "" {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "current price not configured for Stripe"}
	}
	if existingSub.ScheduledPriceID != nil {
		return &TierChangeResponse{
			Object: "tier_change", Status: "blocked", Mode: "tier_change", Action: action,
			PriceID: openrails.PriceID(newPrice.ID), Payment: CheckoutSessionPaymentResponse{Rail: "stripe"},
			Message: "You already have a tier change scheduled. Please wait for the current period to end or cancel the scheduled change first.",
		}, nil
	}
	// The Stripe object is read once before anything is frozen: the item the
	// change targets, and proof that Stripe bills the price the local
	// subscription says it does. Nothing is written here.
	stripeService := &subscriptions.StripeService{Config: s.Config, Rails: s.Rails}
	state, found, err := stripeService.GetSubscriptionState(ctx, existingSub.RailSubscriptionID)
	if err != nil {
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: err.Error()}
	}
	switch {
	case !found:
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "stripe subscription not found"}
	case state.ItemID == "":
		return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "stripe subscription item not found"}
	case state.ScheduleID != "":
		return nil, &TierChangeError{HTTPStatus: http.StatusConflict, Message: "subscription is managed by a Stripe schedule; release it before changing tier"}
	case state.PriceID != currentStripePriceID:
		return nil, &TierChangeError{HTTPStatus: http.StatusConflict, Message: "stripe bills a different price than the local subscription; reconcile before changing tier"}
	}
	now := s.now().UTC()
	payload := StripeTierChangePayload{
		RequestedPrice: strings.TrimSpace(req.PriceID), UserID: user.ID, SubscriptionID: existingSub.ID,
		StripeSubscriptionID: existingSub.RailSubscriptionID, StripeItemID: state.ItemID, Action: action,
		OldPriceID: existingSub.PriceID, OldStripePriceID: currentStripePriceID,
		PriceID: newPrice.ID, ProductID: newPrice.ProductID, ProductName: newProduct.DisplayName, StripePriceID: stripePriceID,
		Currency: newPrice.Currency, RecurringAmount: newPrice.Amount,
	}
	if action == "downgrade" {
		if existingSub.CurrentPeriodEndsAt == nil || existingSub.CurrentPeriodEndsAt.IsZero() {
			return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "subscription missing current period end"}
		}
		periodStart := existingSub.StartedAt
		if existingSub.CurrentPeriodStartsAt != nil && !existingSub.CurrentPeriodStartsAt.IsZero() {
			periodStart = *existingSub.CurrentPeriodStartsAt
		}
		if !existingSub.CurrentPeriodEndsAt.After(periodStart) {
			return nil, &TierChangeError{HTTPStatus: http.StatusBadRequest, Message: "subscription current period is not open"}
		}
		payload.ProrationBehavior, payload.PeriodStart, payload.PeriodEnd = "none", periodStart.UTC(), existingSub.CurrentPeriodEndsAt.UTC()
		payload.BillingCycleDays = newPrice.RecurringCycleDays()
	} else {
		// #268 Model B: Stripe resets the cycle to now and invoices the
		// proration immediately. The frozen now-amount is the local estimate
		// (matching the preview); Stripe finalizes the exact proration.
		estimatedNow, cycleHours, err := CalculateModelBUpgradeCharge(PriceAmountOf(currentPrice), PriceAmountOf(newPrice), existingSub.CurrentPeriodEndsAt, newPrice.RecurringCycleHours(), now)
		if err != nil {
			return nil, err
		}
		payload.AmountDueNow, payload.ProrationBehavior, payload.BillingCycleAnchor = estimatedNow, "always_invoice", "now"
		payload.PaymentBehavior = stripePaymentBehaviorPaidOrRefused
		payload.PeriodStart, payload.PeriodEnd = now, now.Add(time.Duration(cycleHours)*time.Hour)
	}
	return s.enqueueStripeTierChange(ctx, existingSub, payload, tierChangeIdempotencyKey(req.IdempotencyKey))
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
	subIDStr := openrails.SubscriptionID(subID)

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
		PriceID:        openrails.PriceID(newPrice.ID),
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
	if user.Email != nil {
		checkoutReq.Email = strings.TrimSpace(*user.Email)
	}
	if existingSub.PaymentMethodID != nil {
		checkoutReq.PaymentMethodID = openrails.PaymentMethodID(*existingSub.PaymentMethodID).String()
	}

	// Route to existing methods which handle the heavy lifting. The existing
	// subscription's persisted PSP identity resolves its exact account; a sibling
	// account on the same rail must never receive its saved payment method.
	target, err := s.resolveRailTargetForPSP(ctx, string(existingSub.Rail), existingSub.PspID)
	if err != nil {
		return nil, err
	}
	if action == "upgrade" {
		return s.processUpgrade(ctx, checkoutReq, user, newPrice, newProduct, existingSub, target)
	}
	checkoutResp, err := s.processDowngrade(ctx, checkoutReq, user, newPrice, newProduct, existingSub, target)
	if err != nil {
		return nil, err
	}
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
			PriceID: openrails.PriceID(newPrice.ID),
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
	subID := openrails.SubscriptionID(existingSub.ID)
	resp := &TierChangeResponse{
		Object:         "tier_change",
		Status:         "requires_action",
		Mode:           "tier_change",
		Action:         action,
		PriceID:        openrails.PriceID(newPrice.ID),
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
		PriceID: openrails.PriceID(newPrice.ID),
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
		subID := openrails.SubscriptionID(*resp.SubscriptionID)
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
// charges for this price. It never scans or falls back across a rail: with
// several accounts on one rail, each provider's plan is its own.
func requireNMIPlanForTarget(price *models.Price, target railTarget) (string, error) {
	if price == nil {
		return "", errors.New("price is required")
	}
	if id := strings.TrimSpace(checkoutPSPLinkForTarget(price, target)[models.RailKeyPlanID]); id != "" {
		return id, nil
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
	if s.RailPaymentMethodService == nil || s.RailPaymentMethodService.DB == nil {
		log.WithContext(ctx).Warn("checkout: no DB handle to persist stored-credential reference (#297)")
		return
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("checkout: no merchant context to persist stored-credential reference (#297)")
		return
	}
	if _, err := s.RailPaymentMethodService.DB.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{
		MerchantID: tid.UUID(),
		ID:         pm.ID,
		Agreement:  string(agreement),
		Ref:        ref,
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithField("payment_method_id", pm.ID).
			Warn("checkout: failed to persist stored-credential reference (#297); next charge re-captures")
	}
}
