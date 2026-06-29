package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	log "github.com/sirupsen/logrus"
)

type checkoutSubscriptionAccess interface {
	GetActiveOrPendingByUserIDAndTierGroup(ctx context.Context, userID, tierGroup string) (*models.Subscription, error)
	GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error)
	GetByUserIDAndPriceID(ctx context.Context, userID string, priceID uuid.UUID) (*models.Subscription, error)
}

// nonTerminalSubscriptionStatuses is the single source of truth for which
// subscription statuses still bill or grant access (issue #269). A user must
// never hold two of these concurrently in the same product/tier-group — that is
// double-billing; the correct operation is change-tier, not a second subscribe.
//
// Terminal statuses (cancelled — which the model also uses for expired/failed/
// max-retries per its own docs) are excluded: a user with only a terminal
// subscription is free to subscribe again.
var nonTerminalSubscriptionStatuses = []models.SubscriptionStatus{
	models.StatusPending, // created, awaiting first payment confirmation
	models.StatusActive,  // good standing, rebill scheduled
	models.StatusPastDue, // payment failed but still in dunning/grace (will retry)
}

// IsNonTerminalSubscriptionStatus reports whether a subscription in this status
// still bills or grants access and therefore blocks a second subscribe in the
// same product/tier-group (issue #269).
func IsNonTerminalSubscriptionStatus(status models.SubscriptionStatus) bool {
	for _, s := range nonTerminalSubscriptionStatuses {
		if s == status {
			return true
		}
	}
	return false
}

type CheckoutPurchaseService struct {
	PriceService        *catalog.PriceService
	ProductService      *catalog.ProductService
	PaymentService      *payments.PaymentService
	EntitlementService  *entitlements.EntitlementService
	SubscriptionService checkoutSubscriptionAccess
	// ProductAccessService records durable product ownership/access grants
	// (issue #250) alongside feature entitlements on a successful one-time
	// purchase. Optional: when nil, purchases still grant entitlements (grants
	// are an additive, separate model). Wired post-construction via
	// SetProductAccessService so existing constructor call sites are unchanged.
	ProductAccessService productAccessGranter
	// MoneyService deposits a one-off purchase's credit/currency balances (#472) —
	// the other half of "what you get" alongside entitlements. Optional; wired
	// post-construction so existing constructor call sites are unchanged.
	MoneyService purchaseCreditGranter
	clock        clockwork.Clock
}

// purchaseCreditGranter is the subset of MoneyService the purchase flow needs to
// deposit a product's credit/currency grants (#472).
type purchaseCreditGranter interface {
	GrantPurchaseCredits(ctx context.Context, params money.GrantPurchaseCreditsParams) error
}

// productAccessGranter is the subset of the product-access service the purchase
// flow needs. An interface keeps the checkout module decoupled from the
// productaccess module's concrete type (and avoids an import cycle risk).
type productAccessGranter interface {
	GrantProductAccess(ctx context.Context, params productaccess.GrantParams) (*models.ProductAccessGrant, bool, error)
}

func NewCheckoutPurchaseService(
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	paymentService *payments.PaymentService,
	entitlementService *entitlements.EntitlementService,
	subscriptionService checkoutSubscriptionAccess,
	clocks ...clockwork.Clock,
) *CheckoutPurchaseService {
	return &CheckoutPurchaseService{
		PriceService:        priceService,
		ProductService:      productService,
		PaymentService:      paymentService,
		EntitlementService:  entitlementService,
		SubscriptionService: subscriptionService,
		clock:               timeutil.FirstClock(clocks...),
	}
}

func (s *CheckoutPurchaseService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func (s *CheckoutPurchaseService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
}

// SetProductAccessService wires the durable product-access grant service (issue
// #250). Wired post-construction so existing NewCheckoutPurchaseService call
// sites stay unchanged.
func (s *CheckoutPurchaseService) SetProductAccessService(g productAccessGranter) {
	s.ProductAccessService = g
}

// SetMoneyService wires the credit/currency grant service (#472) into the one-time
// purchase flow. Additive to feature entitlements; nil-safe.
func (s *CheckoutPurchaseService) SetMoneyService(g purchaseCreditGranter) {
	s.MoneyService = g
}

func (s *CheckoutPurchaseService) Clock() clockwork.Clock {
	return s.clock
}

func (s *CheckoutPurchaseService) CheckPurchaseEligibility(ctx context.Context, userID string, priceID uuid.UUID) (*EligibilityResult, error) {
	price, err := s.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsPurchasable() {
		return &EligibilityResult{Status: EligibilityBlocked, Reason: "price is not available for purchase"}, nil
	}

	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found: %w", err)
	}
	if !product.IsPurchasable() {
		return &EligibilityResult{Status: EligibilityBlocked, Reason: "product is not available for purchase"}, nil
	}

	if product.TierGroup != nil && *product.TierGroup != "" {
		existingSub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndTierGroup(ctx, userID, *product.TierGroup)
		if err != nil && !repo.IsNotFound(err) {
			return nil, fmt.Errorf("failed to check tier group: %w", err)
		}

		if existingSub != nil {
			existingProduct := existingSub.Price.Product
			if existingProduct == nil {
				return nil, errors.New("failed to load existing product for tier comparison")
			}

			switch {
			case existingProduct.ID == product.ID:
			case existingProduct.TierRank < product.TierRank:
				return &EligibilityResult{Status: EligibilityUpgrade, Reason: fmt.Sprintf("Upgrading from %s to %s", existingProduct.DisplayName, product.DisplayName), ExistingSubscription: existingSub, ExistingProduct: existingProduct}, nil
			case existingProduct.TierRank > product.TierRank:
				return &EligibilityResult{Status: EligibilityDowngrade, Reason: fmt.Sprintf("Downgrading from %s to %s", existingProduct.DisplayName, product.DisplayName), ExistingSubscription: existingSub, ExistingProduct: existingProduct}, nil
			default:
				return &EligibilityResult{Status: EligibilityBlocked, Reason: fmt.Sprintf("You already have an equivalent product (%s) in this tier", existingProduct.DisplayName)}, nil
			}
		}
	}

	coverage, err := s.GetUserProductCoverage(ctx, userID, product)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing coverage: %w", err)
	}
	if coverage.HasCoverage && coverage.IsIndefinite {
		return &EligibilityResult{Status: EligibilityBlocked, Reason: "You already have active access to this product", Coverage: coverage}, nil
	}

	return &EligibilityResult{Status: EligibilityAllowed, Reason: "Purchase allowed", Coverage: coverage}, nil
}

// SubscriptionConflict describes an existing non-terminal subscription that
// blocks a new subscribe for the same product/tier-group (issue #269).
type SubscriptionConflict struct {
	// Blocked is true when a second subscribe must be rejected and the caller
	// directed to change-tier instead.
	Blocked bool
	// SamePrice is true when the user already holds a non-terminal subscription
	// to this exact price (idempotent re-subscribe — no second charge).
	SamePrice bool
	// Existing is the conflicting subscription, when one was found.
	Existing *models.Subscription
	// Message is a clear, user-facing explanation of the conflict.
	Message string
}

// CheckSubscriptionConflict is the shared duplicate-billing guard (issue #269).
// It must run at subscribe time for every rail BEFORE any charge or
// on-chain action. It blocks when the user already holds a NON-terminal
// subscription (active/pending/past_due) that either:
//   - is to this exact price (idempotent re-subscribe; no second sub/charge), or
//   - shares the target product's tier-group (any tier — stacking a $20 and a
//     $50 sub for the same product is double-billing; the correct operation is
//     upgrade/downgrade via change-tier).
//
// It returns Blocked=false (no conflict) when the user has no such subscription,
// so a first-time subscribe and a DIFFERENT tier-group are both allowed.
func (s *CheckoutPurchaseService) CheckSubscriptionConflict(ctx context.Context, userID string, price *models.Price, product *models.Product) (*SubscriptionConflict, error) {
	if s.SubscriptionService == nil {
		return &SubscriptionConflict{}, nil
	}
	if price == nil || product == nil {
		return nil, errors.New("price and product are required for conflict check")
	}

	// Exact-same-price re-subscribe: idempotent, never charge twice (issue #269).
	existingSamePrice, err := s.SubscriptionService.GetByUserIDAndPriceID(ctx, userID, price.ID)
	if err != nil && !repo.IsNotFound(err) {
		return nil, fmt.Errorf("failed to check existing price subscription: %w", err)
	}
	if existingSamePrice != nil && IsNonTerminalSubscriptionStatus(existingSamePrice.Status) {
		return &SubscriptionConflict{
			Blocked:   true,
			SamePrice: true,
			Existing:  existingSamePrice,
			Message:   "You already have an active subscription to this plan",
		}, nil
	}

	// Tier-group conflict: any non-terminal sub in the same group (the repo query
	// already filters to the non-terminal status set) blocks a second subscribe.
	if product.TierGroup != nil && strings.TrimSpace(*product.TierGroup) != "" {
		existing, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndTierGroup(ctx, userID, *product.TierGroup)
		if err != nil && !repo.IsNotFound(err) {
			return nil, fmt.Errorf("failed to check tier group: %w", err)
		}
		if existing != nil {
			existingProduct := existing.Price.Product
			switch {
			case existingProduct != nil && existingProduct.ID == product.ID:
				return &SubscriptionConflict{
					Blocked:  true,
					Existing: existing,
					Message:  "You already have an active subscription to this plan",
				}, nil
			case existingProduct != nil && existingProduct.TierRank < product.TierRank:
				return &SubscriptionConflict{
					Blocked:  true,
					Existing: existing,
					Message:  "Use POST /v1/me/subscriptions/change-tier for tier upgrades",
				}, nil
			case existingProduct != nil && existingProduct.TierRank > product.TierRank:
				return &SubscriptionConflict{
					Blocked:  true,
					Existing: existing,
					Message:  "Use POST /v1/me/subscriptions/change-tier for tier downgrades",
				}, nil
			default:
				return &SubscriptionConflict{
					Blocked:  true,
					Existing: existing,
					Message:  "You already have an active subscription in this tier group. Use POST /v1/me/subscriptions/change-tier to change tiers.",
				}, nil
			}
		}
	}

	return &SubscriptionConflict{}, nil
}

func (s *CheckoutPurchaseService) GetUserProductCoverage(ctx context.Context, userID string, product *models.Product) (*CoverageInfo, error) {
	now := s.now()
	coverage := &CoverageInfo{HasCoverage: false}

	if s.SubscriptionService != nil {
		sub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndProductID(ctx, userID, product.ID)
		if err != nil && !repo.IsNotFound(err) {
			return nil, fmt.Errorf("failed to check subscription: %w", err)
		}
		if sub != nil {
			if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
				coverage.HasCoverage = true
				coverage.SourceType = "subscription"
				coverage.SourceID = &sub.ID
				coverage.IsIndefinite = true
				return coverage, nil
			}
			if !sub.CurrentPeriodEndsAt.After(now) {
				log.WithFields(log.Fields{
					"subscription_id": sub.ID,
					"user_id":         userID,
					"product_id":      product.ID,
					"period_end":      sub.CurrentPeriodEndsAt,
				}).Warn("ignoring stale subscription coverage that has already ended")
			} else {
				coverage.HasCoverage = true
				coverage.SourceType = "subscription"
				coverage.SourceID = &sub.ID
				coverage.EndDate = sub.CurrentPeriodEndsAt
			}
		}
	}

	if s.EntitlementService != nil && product.EntitlementsSpec != nil {
		for entitlementName := range product.EntitlementsSpec {
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

			ent, err := s.EntitlementService.LatestFiniteWindow(ctx, userID, entitlementName, now)
			if err != nil && !repo.IsNotFound(err) {
				return nil, fmt.Errorf("failed to check finite entitlement: %w", err)
			}
			if ent != nil {
				coverage.HasCoverage = true
				coverage.SourceType = "entitlement"
				if ent.EndAt != nil && (coverage.EndDate == nil || ent.EndAt.After(*coverage.EndDate)) {
					coverage.EndDate = ent.EndAt
				}
			}
		}
	}

	return coverage, nil
}

func (s *CheckoutPurchaseService) RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error) {
	if req.UserID == "" {
		return nil, errors.New("user_id is required")
	}
	if req.TransactionID == "" {
		return nil, errors.New("transaction_id is required")
	}
	if req.Rail == "" {
		return nil, errors.New("rail is required")
	}

	price, err := s.PriceService.GetByID(ctx, req.PriceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found: %w", err)
	}

	eligibility, err := s.CheckPurchaseEligibility(ctx, req.UserID, req.PriceID)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{"user_id": req.UserID, "price_id": req.PriceID}).Warn("failed to check eligibility during RegisterPurchase")
		eligibility = &EligibilityResult{Status: EligibilityAllowed}
	}

	coverage := eligibility.Coverage
	if coverage == nil {
		coverage = &CoverageInfo{}
	}

	amount := req.Amount
	if !req.AmountProvided && amount == 0 {
		amount = price.Amount
	}
	currency := req.Currency
	if currency == "" {
		currency = price.Currency
	}

	now := s.now()
	purchasedAt := now
	if req.PurchasedAt != nil {
		purchasedAt = req.PurchasedAt.UTC()
	}

	customerID, err := customerIDFromUser(req.UserID)
	if err != nil {
		return nil, err
	}
	paymentID := uuidutil.NewV7()
	payment := &models.Payment{
		ID:                       paymentID,
		CustomerID:               customerID,
		PriceID:                  price.ID,
		SubscriptionID:           req.SubscriptionID,
		Rail:                     models.Rail(req.Rail),
		TransactionID:            req.TransactionID,
		Amount:                   amount,
		ListAmount:               price.Amount,
		Currency:                 currency,
		PurchasedAt:              purchasedAt,
		CreatedAt:                now,
		DiscountCode:             req.DiscountCode,
		DiscountReason:           req.DiscountReason,
		DiscountMetadata:         req.DiscountMetadata,
		Metadata:                 req.Metadata,
		EntitlementsSpecSnapshot: models.CloneEntitlementsSpec(product.EntitlementsSpec),
		CreditsSpecSnapshot:      models.CloneCreditsSpec(product.CreditsSpec),
	}

	created, err := s.PaymentService.CreateIfNotExists(ctx, payment)
	if err != nil {
		return nil, fmt.Errorf("failed to create payment record: %w", err)
	}
	if !created {
		existingPayment, err := s.PaymentService.GetByTransactionID(ctx, models.Rail(req.Rail), req.TransactionID)
		if err != nil {
			return nil, fmt.Errorf("failed to load existing payment record: %w", err)
		}
		if existingPayment.CustomerID.String() != req.UserID {
			return nil, fmt.Errorf("payment transaction belongs to a different user")
		}
		if existingPayment.PriceID != req.PriceID {
			return nil, fmt.Errorf("payment transaction belongs to a different price")
		}
		if (existingPayment.SubscriptionID == nil) != (req.SubscriptionID == nil) {
			return nil, fmt.Errorf("payment transaction subscription linkage mismatch")
		}
		if existingPayment.SubscriptionID != nil && *existingPayment.SubscriptionID != *req.SubscriptionID {
			return nil, fmt.Errorf("payment transaction belongs to a different subscription")
		}
		if !payments.PaymentStatusCompleted(existingPayment.Status) {
			return nil, fmt.Errorf("payment transaction is not completed")
		}
		if amount > 0 && existingPayment.Amount != amount {
			return nil, fmt.Errorf("payment transaction amount mismatch")
		}
		if currency != "" && !strings.EqualFold(strings.TrimSpace(existingPayment.Currency), currency) {
			return nil, fmt.Errorf("payment transaction currency mismatch")
		}

		entitlementsSpec := existingPayment.EntitlementsSpecSnapshot
		if len(entitlementsSpec) == 0 {
			entitlementsSpec = product.EntitlementsSpec
		}
		sourceID := existingPayment.ID
		if existingPayment.SubscriptionID != nil {
			sourceID = *existingPayment.SubscriptionID
		}
		if err := s.grantProductEntitlements(ctx, req.UserID, entitlementsSpec, sourceID, coverage, existingPayment.SubscriptionID != nil, price.AccessDurationHours, true); err != nil {
			return nil, fmt.Errorf("failed to repair entitlements for existing payment: %w", err)
		}

		// Idempotently (re)record the product access grant for one-time purchases
		// (issue #250) so a replayed webhook/poll repairs a missing grant the same
		// way it repairs entitlements.
		s.grantProductAccess(ctx, req.UserID, product.ID, existingPayment.ID, existingPayment.SubscriptionID != nil, price.AutoRenew, price.AccessDurationHours)

		// Re-deposit the purchase credit/currency grants (#472); idempotent per
		// (payment, label) so re-delivery repairs without double-granting.
		creditsSpec := existingPayment.CreditsSpecSnapshot
		if len(creditsSpec) == 0 {
			creditsSpec = product.CreditsSpec
		}
		s.grantPurchaseCredits(ctx, req.UserID, creditsSpec, existingPayment.ID, existingPayment.SubscriptionID != nil)

		grantedEntitlements := make([]string, 0, len(entitlementsSpec))
		for entName := range entitlementsSpec {
			grantedEntitlements = append(grantedEntitlements, entName)
		}
		log.WithFields(log.Fields{
			"payment_id": existingPayment.ID, "user_id": req.UserID, "price_id": req.PriceID,
			"rail": req.Rail, "transaction_id": req.TransactionID,
		}).Info("purchase already registered; repaired entitlement state if needed")
		return &payments.RegisterPurchaseResponse{PaymentID: existingPayment.ID, Entitlements: grantedEntitlements, Eligibility: string(eligibility.Status)}, nil
	}

	sourceID := paymentID
	if req.SubscriptionID != nil {
		log.WithFields(log.Fields{"payment_id": paymentID, "user_id": req.UserID, "price_id": req.PriceID, "subscription_id": req.SubscriptionID}).Info("registered subscription payment")
		sourceID = *req.SubscriptionID
	}

	var grantedEntitlements []string
	if err := s.grantProductEntitlements(ctx, req.UserID, product.EntitlementsSpec, sourceID, coverage, req.SubscriptionID != nil && req.SubscriptionID.String() != "", price.AccessDurationHours, false); err != nil {
		log.WithError(err).WithField("payment_id", sourceID).Error("failed to grant entitlements after payment")
		return nil, fmt.Errorf("failed to grant entitlements after payment: %w", err)
	} else if product.EntitlementsSpec != nil {
		for entName := range product.EntitlementsSpec {
			grantedEntitlements = append(grantedEntitlements, entName)
		}
	}

	// Durable product ownership/access grant (issue #250) for one-time product
	// purchases — additive to the feature entitlements granted above. Keyed on the
	// payment id so it is idempotent; skipped for subscription purchases.
	s.grantProductAccess(ctx, req.UserID, product.ID, paymentID, req.SubscriptionID != nil, price.AutoRenew, price.AccessDurationHours)

	// Credit/currency balance grants (#472) — the other half of "what you get"
	// alongside entitlements. Keyed on payment id for idempotency; skipped for
	// subscription purchases (those grant credits per period).
	s.grantPurchaseCredits(ctx, req.UserID, product.CreditsSpec, paymentID, req.SubscriptionID != nil)

	var delayedStart *time.Time
	if coverage.HasCoverage && coverage.EndDate != nil {
		delayedStart = coverage.EndDate
	}

	log.WithFields(log.Fields{
		"payment_id": paymentID, "user_id": req.UserID, "price_id": req.PriceID, "product_id": product.ID,
		"rail": req.Rail, "transaction_id": req.TransactionID, "entitlements": grantedEntitlements,
		"delayed_start": delayedStart, "eligibility": eligibility.Status,
	}).Info("registered purchase")

	return &payments.RegisterPurchaseResponse{PaymentID: paymentID, Entitlements: grantedEntitlements, DelayedStart: delayedStart, Eligibility: string(eligibility.Status)}, nil
}

// grantProductAccess records a product ownership/access grant (issue #250) for a
// successful ONE-TIME product purchase, in ADDITION to (and distinct from)
// feature entitlements. It is idempotent: the grant is keyed on the payment id,
// so re-processing the same purchase is a no-op. Skipped for auto-renewing /
// subscription purchases — those are access-by-subscription, not ownership.
// The access window comes from the price's access_duration_hours (#622): a finite
// value sets ends_at (rental, possibly sub-day); nil = durable ownership. A nil
// ProductAccessService makes this a no-op so existing call sites/tests are unaffected.
func (s *CheckoutPurchaseService) grantProductAccess(ctx context.Context, userID string, productID, paymentID uuid.UUID, isSubscription, autoRenew bool, accessDurationHours *int) {
	if s.ProductAccessService == nil {
		return
	}
	if isSubscription || autoRenew {
		return
	}
	var endsAt *time.Time
	if accessDurationHours != nil && *accessDurationHours > 0 {
		e := s.now().Add(time.Duration(*accessDurationHours) * time.Hour)
		endsAt = &e
	}
	pid := paymentID
	if _, _, err := s.ProductAccessService.GrantProductAccess(ctx, productaccess.GrantParams{
		UserID:     userID,
		ProductID:  productID,
		SourceType: models.ProductAccessSourcePurchase,
		SourceID:   paymentID.String(),
		PaymentID:  &pid,
		EndsAt:     endsAt,
	}); err != nil {
		// Non-fatal: the payment + entitlements already succeeded. Log and
		// continue so a grant write failure never blocks fulfilment; refunds
		// revoke by payment id regardless.
		log.WithError(err).WithFields(log.Fields{
			"user_id": userID, "product_id": productID, "payment_id": paymentID,
		}).Error("failed to record product access grant after purchase")
	}
}

// grantPurchaseCredits deposits a one-time purchase's credit/currency balances
// (#472) — the other half of "what you get" alongside entitlements. Idempotent
// per (payment, grant label) inside MoneyService, so a replayed webhook/poll
// never double-grants. Skipped for subscription purchases (those grant credits
// via GrantSubscriptionCredits per period). A nil MoneyService makes this a no-op.
func (s *CheckoutPurchaseService) grantPurchaseCredits(ctx context.Context, userID string, creditsSpec models.CreditsSpec, paymentID uuid.UUID, isSubscription bool) {
	if s.MoneyService == nil || len(creditsSpec) == 0 || isSubscription {
		return
	}
	payer := identity.CustomerIDFromString(userID)
	if payer.IsZero() {
		log.WithField("user_id", userID).Error("skip purchase credit grant: payer is not a UUID")
		return
	}
	if err := s.MoneyService.GrantPurchaseCredits(ctx, money.GrantPurchaseCreditsParams{
		Payer:     payer,
		PaymentID: paymentID,
		Spec:      creditsSpec,
		Source:    "purchase",
	}); err != nil {
		// Non-fatal: the payment + entitlements already succeeded. Log and continue
		// so a grant write failure never blocks fulfilment; re-delivery repairs it.
		log.WithError(err).WithFields(log.Fields{
			"user_id": userID, "payment_id": paymentID,
		}).Error("failed to grant purchase credits")
	}
}

// grantProductEntitlements grants the product's entitlements for the access
// window the price bought (#622). The window is the price's access_duration_hours
// (the same window that drives product_access ends_at — G3): a finite value sets
// a finite entitlement end_at; nil = indefinite. Re-purchase stacks: a new window
// starts at the existing coverage end.
func (s *CheckoutPurchaseService) grantProductEntitlements(ctx context.Context, userID string, entitlementsSpec map[string]*int, paymentID uuid.UUID, coverage *CoverageInfo, subscription bool, accessDurationHours *int, skipExistingSource bool) error {
	if s.EntitlementService == nil || entitlementsSpec == nil {
		return nil
	}

	now := s.now()
	for entitlementName, entDurationHours := range entitlementsSpec {
		startAt := now
		if coverage.HasCoverage && coverage.EndDate != nil {
			startAt = *coverage.EndDate
		}

		// The price's access window is authoritative (#622) and is in HOURS. When
		// the price is indefinite, a product's per-entitlement duration (also in
		// HOURS, a separate pre-existing feature) still scopes that entitlement to a
		// shorter window. They are applied separately.
		var endAt *time.Time
		switch {
		case accessDurationHours != nil && *accessDurationHours > 0:
			end := startAt.Add(time.Duration(*accessDurationHours) * time.Hour)
			endAt = &end
		case accessDurationHours == nil && entDurationHours != nil && *entDurationHours > 0:
			end := startAt.Add(time.Duration(*entDurationHours) * time.Hour)
			endAt = &end
		}

		sourceType := models.EntitlementSourceOneOff
		if subscription {
			sourceType = models.EntitlementSourceSubscription
		}
		if skipExistingSource {
			exists, err := s.EntitlementService.ExistsBySource(ctx, sourceType, paymentID, entitlementName)
			if err != nil {
				return fmt.Errorf("check existing entitlement source: %w", err)
			}
			if exists {
				continue
			}
		}
		notBefore := startAt
		params := entitlements.PushNewEntitlementParams{UserID: userID, Entitlement: entitlementName, NotBefore: &notBefore, SourceType: sourceType, SourceID: paymentID}
		if endAt == nil {
			params.Indefinite = true
		} else {
			e := endAt.UTC()
			params.EndAt = &e
		}

		if _, err := s.EntitlementService.PushNewEntitlement(ctx, params); err != nil {
			log.WithError(err).WithFields(log.Fields{"user_id": userID, "entitlement": entitlementName, "payment_id": paymentID}).Error("failed to grant entitlement")
			return err
		}
		log.WithFields(log.Fields{"user_id": userID, "entitlement": entitlementName, "payment_id": paymentID, "start_at": startAt, "end_at": endAt}).Info(fmt.Sprintf("granted entitlement from %s purchase", sourceType))
	}

	return nil
}
