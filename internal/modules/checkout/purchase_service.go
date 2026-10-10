package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	log "github.com/sirupsen/logrus"
)

type checkoutSubscriptionAccess interface {
	GetActiveOrPendingByUserIDAndTierGroup(ctx context.Context, userID, tierGroup string) (*models.Subscription, error)
	GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error)
	GetByUserIDAndPriceID(ctx context.Context, userID string, priceID uuid.UUID) (*models.Subscription, error)
	// #691 checkout guard: `unknown` subs don't hold the lifecycle slot but may
	// still be alive (billing) at the provider — a re-purchase double-bills.
	GetUnknownByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error)
	GetUnknownByUserIDAndTierGroup(ctx context.Context, userID, tierGroup string) (*models.Subscription, error)
}

// nonTerminalSubscriptionStatuses is the single source of truth for which
// subscription statuses still bill or grant access (issue #269). A user must
// never hold two of these concurrently in the same product/tier-group — that is
// double-billing; the correct operation is a subscription change, not a second subscribe.
//
// Terminal statuses (canceled — which the model also uses for expired/failed/
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
	database            *db.DB
	transactionDB       *db.DB
	PriceService        *catalog.PriceService
	ProductService      *catalog.ProductService
	PaymentService      *payments.PaymentService
	EntitlementService  *entitlements.EntitlementService
	SubscriptionService checkoutSubscriptionAccess
	clock               clockwork.Clock
}

func NewCheckoutPurchaseService(
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	paymentService *payments.PaymentService,
	entitlementService *entitlements.EntitlementService,
	subscriptionService checkoutSubscriptionAccess,
	clocks ...clockwork.Clock,
) *CheckoutPurchaseService {
	var database *db.DB
	if priceService != nil {
		database = priceService.Database()
	}
	return &CheckoutPurchaseService{
		database:            database,
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
	if err := s.checkPermanentOwnership(ctx, userID, price, product); err != nil {
		if errors.Is(err, ErrCheckoutAttemptConflict) {
			return &EligibilityResult{Status: EligibilityBlocked, Reason: err.Error()}, nil
		}
		return nil, err
	}

	if product.TierGroup != nil && *product.TierGroup != "" {
		existingSub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndTierGroup(ctx, userID, *product.TierGroup)
		if err != nil && !db.IsNotFound(err) {
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

	coverage, err := s.purchaseCoverage(ctx, userID, price, product)
	if err != nil {
		return nil, fmt.Errorf("failed to check existing coverage: %w", err)
	}
	if coverage.HasCoverage && coverage.IsIndefinite {
		return &EligibilityResult{Status: EligibilityBlocked, Reason: "You already have active access to this product", Coverage: coverage}, nil
	}

	return &EligibilityResult{Status: EligibilityAllowed, Reason: "Purchase allowed", Coverage: coverage}, nil
}

// Machine-readable conflict codes (#691) so clients can route the user to the
// right remedy instead of parsing prose.
const (
	// ConflictCodeDuplicateSubscription: a non-terminal subscription to the same
	// plan/tier already exists — re-purchasing would double-bill.
	ConflictCodeDuplicateSubscription = "duplicate_subscription"
	// ConflictCodeChangeTierRequired: the conflict is a tier change — use the
	// change endpoint, not a second subscribe.
	ConflictCodeChangeTierRequired = "change_tier_required"
	// ConflictCodeMembershipPendingVerification (#691): the customer holds an
	// `unknown` subscription for this product/tier-group — an existing membership
	// pending provider verification. It may still be alive and billing at the
	// provider; verify/resume it instead of purchasing again.
	ConflictCodeMembershipPendingVerification = "membership_pending_verification"
)

// SubscriptionConflict describes an existing non-terminal subscription that
// blocks a new subscribe for the same product/tier-group (issue #269).
type SubscriptionConflict struct {
	// Blocked is true when a second subscribe must be rejected and the caller
	// directed to the change endpoint instead.
	Blocked bool
	// SamePrice is true when the user already holds a non-terminal subscription
	// to this exact price (idempotent re-subscribe — no second charge).
	SamePrice bool
	// Existing is the conflicting subscription, when one was found.
	Existing *models.Subscription
	// Code is the machine-readable conflict class (Conflict* consts).
	Code string
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
//     upgrade/downgrade via the change endpoint).
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
	if err != nil && !db.IsNotFound(err) {
		return nil, fmt.Errorf("failed to check existing price subscription: %w", err)
	}
	if existingSamePrice != nil && IsNonTerminalSubscriptionStatus(existingSamePrice.Status) {
		return &SubscriptionConflict{
			Blocked:   true,
			SamePrice: true,
			Existing:  existingSamePrice,
			Code:      ConflictCodeDuplicateSubscription,
			Message:   "You already have an active subscription to this plan",
		}, nil
	}

	// Tier-group conflict: any non-terminal sub in the same group (the repo query
	// already filters to the non-terminal status set) blocks a second subscribe.
	if product.TierGroup != nil && strings.TrimSpace(*product.TierGroup) != "" {
		existing, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndTierGroup(ctx, userID, *product.TierGroup)
		if err != nil && !db.IsNotFound(err) {
			return nil, fmt.Errorf("failed to check tier group: %w", err)
		}
		if existing != nil {
			existingProduct := existing.Price.Product
			switch {
			case existingProduct != nil && existingProduct.ID == product.ID:
				return &SubscriptionConflict{
					Blocked:  true,
					Existing: existing,
					Code:     ConflictCodeDuplicateSubscription,
					Message:  "You already have an active subscription to this plan",
				}, nil
			case existingProduct != nil && existingProduct.TierRank < product.TierRank:
				return &SubscriptionConflict{
					Blocked:  true,
					Existing: existing,
					Code:     ConflictCodeChangeTierRequired,
					Message:  "Use POST /v1/me/subscriptions/{id}/change for tier upgrades",
				}, nil
			case existingProduct != nil && existingProduct.TierRank > product.TierRank:
				return &SubscriptionConflict{
					Blocked:  true,
					Existing: existing,
					Code:     ConflictCodeChangeTierRequired,
					Message:  "Use POST /v1/me/subscriptions/{id}/change for tier downgrades",
				}, nil
			default:
				return &SubscriptionConflict{
					Blocked:  true,
					Existing: existing,
					Code:     ConflictCodeChangeTierRequired,
					Message:  "You already have an active subscription in this tier group. Use POST /v1/me/subscriptions/{id}/change to change tiers.",
				}, nil
			}
		}
	}

	// #691: an `unknown` sub for the same product or tier-group blocks a new
	// subscribe — the real provider-side sub may still be alive and billing, so
	// a re-purchase double-bills. Verify/resume instead of repurchase.
	if conflict, err := s.checkUnknownSubscriptionConflict(ctx, userID, product); err != nil || conflict != nil {
		if err != nil {
			return nil, err
		}
		return conflict, nil
	}

	return &SubscriptionConflict{}, nil
}

// checkUnknownSubscriptionConflict returns the #691 verification-pending
// conflict when the customer holds an `unknown` subscription for the product or
// its tier-group, nil otherwise.
func (s *CheckoutPurchaseService) checkUnknownSubscriptionConflict(ctx context.Context, userID string, product *models.Product) (*SubscriptionConflict, error) {
	if s.SubscriptionService == nil || product == nil {
		return nil, nil
	}
	unknownSub, err := s.SubscriptionService.GetUnknownByUserIDAndProductID(ctx, userID, product.ID)
	if err != nil && !db.IsNotFound(err) {
		return nil, fmt.Errorf("failed to check unknown subscription: %w", err)
	}
	if unknownSub == nil && product.TierGroup != nil && strings.TrimSpace(*product.TierGroup) != "" {
		unknownSub, err = s.SubscriptionService.GetUnknownByUserIDAndTierGroup(ctx, userID, *product.TierGroup)
		if err != nil && !db.IsNotFound(err) {
			return nil, fmt.Errorf("failed to check unknown tier-group subscription: %w", err)
		}
	}
	if unknownSub == nil {
		return nil, nil
	}
	return &SubscriptionConflict{
		Blocked:  true,
		Existing: unknownSub,
		Code:     ConflictCodeMembershipPendingVerification,
		Message:  "You already have a membership for this product that is pending payment verification. Verify or resume it instead of purchasing again — a new purchase could bill you twice.",
	}, nil
}

func (s *CheckoutPurchaseService) GetUserProductCoverage(ctx context.Context, userID string, product *models.Product) (*CoverageInfo, error) {
	now := s.now()
	coverage := &CoverageInfo{HasCoverage: false}

	if s.SubscriptionService != nil {
		sub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndProductID(ctx, userID, product.ID)
		if err != nil && !db.IsNotFound(err) {
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

	// A new rental of the product starts after the customer's live windows
	// of the same product.
	if s.EntitlementService != nil {
		indefinite, latestEnd, err := s.EntitlementService.ProductCoverage(ctx, userID, []uuid.UUID{product.ID}, now)
		if err != nil {
			return nil, fmt.Errorf("failed to check product coverage: %w", err)
		}
		if indefinite {
			coverage.HasCoverage = true
			coverage.IsIndefinite = true
			coverage.SourceType = "product"
			return coverage, nil
		}
		if latestEnd != nil {
			coverage.HasCoverage = true
			coverage.SourceType = "product"
			if coverage.EndDate == nil || latestEnd.After(*coverage.EndDate) {
				coverage.EndDate = latestEnd
			}
		}
	}

	return coverage, nil
}

// ErrPaymentTransactionTaken: the provider transaction id is already recorded
// for a different purchase, so it cannot settle this one.
var ErrPaymentTransactionTaken = errors.New("payment transaction belongs to another purchase")

type paymentTransactionTaken string

func (e paymentTransactionTaken) Error() string        { return string(e) }
func (e paymentTransactionTaken) Is(target error) bool { return target == ErrPaymentTransactionTaken }

func (s *CheckoutPurchaseService) RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error) {
	if req == nil {
		return nil, errors.New("purchase is required")
	}
	if req.CheckoutAttemptID == uuid.Nil && s.database != nil && s.transactionDB == nil {
		var result *payments.RegisterPurchaseResponse
		err := s.database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = s.transactionBound(s.database.NewWithPgxTx(tx)).RegisterPurchase(ctx, req)
			return err
		})
		return result, err
	}
	if req != nil && req.CheckoutAttemptID != uuid.Nil {
		return s.registerSessionPurchase(ctx, req)
	}
	if req.UserID == "" {
		return nil, errors.New("user_id is required")
	}
	if req.TransactionID == "" {
		return nil, errors.New("transaction_id is required")
	}
	if req.Rail == "" && req.Channel != models.ChannelManual {
		return nil, errors.New("rail is required")
	}

	price, err := s.PriceService.GetByID(ctx, req.PriceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if req.Channel == models.ChannelManual && price.CustomerAmount != nil {
		var chosen *int64
		if req.AmountProvided {
			chosen = &req.Amount
		}
		price, err = CheckoutPriceForAmount(price, chosen)
		if err != nil {
			return nil, err
		}
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

	acceptedAt := s.now().UTC().Truncate(time.Microsecond)
	if req.PurchasedAt != nil {
		acceptedAt = req.PurchasedAt.UTC().Truncate(time.Microsecond)
	}
	benefitPrice := *price
	if req.AmountProvided {
		benefitPrice.Amount = req.Amount
	}
	var credit *models.CreditGrantSnapshot
	// Native provider purchases require durable accepted terms. Historical
	// observations without them must never inherit a newly added benefit.
	if req.Channel == models.ChannelManual {
		credit, err = acceptedCreditGrant(product, &benefitPrice)
		if err != nil {
			return nil, err
		}
	}
	return s.applyPurchase(ctx, req, price, product, eligibility, acceptedAt, uuid.Nil, credit)
}

// applyPurchase is the shared financial/access writer. Observed purchases
// prepare current facts above; durable sales supply their accepted snapshots.
// Every service on s must share the caller's transaction for durable completion.
func (s *CheckoutPurchaseService) applyPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest, price *models.Price, product *models.Product, eligibility *EligibilityResult, acceptedAt time.Time, acceptedPaymentID uuid.UUID, credit *models.CreditGrantSnapshot) (*payments.RegisterPurchaseResponse, error) {
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
	paymentID := acceptedPaymentID
	if paymentID == uuid.Nil {
		paymentID = uuidutil.NewV7()
	}
	settledCredit := models.CloneCreditGrantSnapshot(credit)
	if settledCredit != nil {
		settledCredit.StartsAt = now.UTC().Truncate(time.Microsecond)
		expires := settledCredit.StartsAt.AddDate(0, 0, settledCredit.ExpiresAfterDays)
		settledCredit.ExpiresAt = &expires
	}
	payment := &models.Payment{
		ID:                  paymentID,
		CustomerID:          customerID,
		PriceID:             price.ID,
		SubscriptionID:      req.SubscriptionID,
		Channel:             req.Channel,
		Rail:                models.Rail(req.Rail),
		TransactionID:       req.TransactionID,
		Amount:              amount,
		ListAmount:          price.Amount,
		Currency:            currency,
		Status:              payments.PaymentStatusSucceededValue,
		PurchasedAt:         purchasedAt,
		CreatedAt:           now,
		DiscountCode:        req.DiscountCode,
		DiscountReason:      req.DiscountReason,
		DiscountMetadata:    req.DiscountMetadata,
		Metadata:            req.Metadata,
		CreditGrantSnapshot: settledCredit,
		// or#827: RegisterPurchase records a charge the rail already approved,
		// keyed on the rail's own transaction id.
		MoneyMovement: models.MoneyMovementRail,
	}
	if req.AttemptKind != "" {
		k := req.AttemptKind
		payment.AttemptKind = &k
	}
	if req.TokenType != "" {
		tt := req.TokenType
		payment.TokenType = &tt
	}

	created, err := s.PaymentService.CreateIfNotExists(ctx, payment)
	if err != nil {
		return nil, fmt.Errorf("failed to create payment record: %w", err)
	}
	if !created {
		lookup := func() (*models.Payment, error) {
			if req.Channel == models.ChannelManual {
				return s.PaymentService.GetManualByTransactionID(ctx, req.TransactionID)
			}
			return s.PaymentService.GetByPSPTransactionID(ctx, models.Rail(req.Rail), req.TransactionID)
		}
		existingPayment, err := lookup()
		if err != nil {
			return nil, fmt.Errorf("failed to load existing payment record: %w", err)
		}
		if existingPayment.CustomerID.String() != req.UserID {
			return nil, paymentTransactionTaken("payment transaction belongs to a different user")
		}
		if existingPayment.PriceID != req.PriceID {
			return nil, paymentTransactionTaken("payment transaction belongs to a different price")
		}
		if (existingPayment.SubscriptionID == nil) != (req.SubscriptionID == nil) {
			return nil, paymentTransactionTaken("payment transaction subscription linkage mismatch")
		}
		if existingPayment.SubscriptionID != nil && *existingPayment.SubscriptionID != *req.SubscriptionID {
			return nil, paymentTransactionTaken("payment transaction belongs to a different subscription")
		}
		if !payments.PaymentStatusSucceeded(existingPayment.Status) {
			return nil, paymentTransactionTaken("payment transaction is not completed")
		}
		if amount > 0 && existingPayment.Amount != amount {
			return nil, paymentTransactionTaken("payment transaction amount mismatch")
		}
		if currency != "" && !strings.EqualFold(strings.TrimSpace(existingPayment.Currency), currency) {
			return nil, paymentTransactionTaken("payment transaction currency mismatch")
		}

		if acceptedPaymentID != uuid.Nil && (!models.SameCreditGrantPromise(existingPayment.CreditGrantSnapshot, credit) || existingPayment.ListAmount != price.Amount) {
			return nil, errors.New("existing payment contradicts accepted purchase terms")
		}
		sourceID := existingPayment.ID
		if existingPayment.SubscriptionID != nil {
			sourceID = *existingPayment.SubscriptionID
		}
		// A credit pack is bought for its credit lot, not held as a product.
		switch {
		case existingPayment.CreditGrantSnapshot != nil:
		case acceptedPaymentID != uuid.Nil:
			if err := s.applyAcceptedPurchaseAccess(ctx, req.UserID, product.ID, existingPayment.ID, price.AccessDurationHours, acceptedAt, coverage); err != nil {
				return nil, err
			}
		default:
			if err := s.grantPurchaseAccess(ctx, req.UserID, product.ID, existingPayment.ID, sourceID, coverage, existingPayment.SubscriptionID != nil, price.AccessDurationHours, acceptedAt); err != nil {
				return nil, fmt.Errorf("failed to repair access for existing payment: %w", err)
			}
		}
		if err := s.grantPurchasedCredits(ctx, existingPayment, product.ID); err != nil {
			return nil, err
		}
		log.WithFields(log.Fields{
			"payment_id": existingPayment.ID, "user_id": req.UserID, "price_id": req.PriceID,
			"rail": req.Rail, "transaction_id": req.TransactionID,
		}).Info("purchase already registered; repaired access if needed")
		return &payments.RegisterPurchaseResponse{PaymentID: existingPayment.ID, Entitlements: nonNilKeys(product.Entitlements), Eligibility: string(eligibility.Status)}, nil
	}

	sourceID := paymentID
	if req.SubscriptionID != nil {
		log.WithFields(log.Fields{"payment_id": paymentID, "user_id": req.UserID, "price_id": req.PriceID, "subscription_id": req.SubscriptionID}).Info("registered subscription payment")
		sourceID = *req.SubscriptionID
	}

	switch {
	case credit != nil:
	case acceptedPaymentID != uuid.Nil:
		if err := s.applyAcceptedPurchaseAccess(ctx, req.UserID, product.ID, paymentID, price.AccessDurationHours, acceptedAt, coverage); err != nil {
			return nil, err
		}
	default:
		if err := s.grantPurchaseAccess(ctx, req.UserID, product.ID, paymentID, sourceID, coverage, req.SubscriptionID != nil, price.AccessDurationHours, acceptedAt); err != nil {
			log.WithError(err).WithField("payment_id", sourceID).Error("failed to grant access after payment")
			return nil, fmt.Errorf("failed to grant access after payment: %w", err)
		}
	}
	if err := s.grantPurchasedCredits(ctx, payment, product.ID); err != nil {
		return nil, err
	}
	if req.SubscriptionID == nil {
		if err := s.queueReceipt(ctx, payment, product); err != nil {
			return nil, err
		}
	}
	var delayedStart *time.Time
	if coverage.HasCoverage && coverage.EndDate != nil {
		delayedStart = coverage.EndDate
	}

	log.WithFields(log.Fields{
		"payment_id": paymentID, "user_id": req.UserID, "price_id": req.PriceID, "product_id": product.ID,
		"rail": req.Rail, "transaction_id": req.TransactionID,
		"delayed_start": delayedStart, "eligibility": eligibility.Status,
	}).Info("registered purchase")

	return &payments.RegisterPurchaseResponse{PaymentID: paymentID, Entitlements: nonNilKeys(product.Entitlements), DelayedStart: delayedStart, Eligibility: string(eligibility.Status)}, nil
}

// grantPurchaseAccess grants the purchased product for the window the price
// bought: from acceptedAt, or after the customer's live windows of the product
// it stacks on, for the price's access duration (none: indefinitely). A
// subscription payment grants through its subscription. Idempotent per source.
func (s *CheckoutPurchaseService) grantPurchaseAccess(ctx context.Context, userID string, productID, paymentID, sourceID uuid.UUID, coverage *CoverageInfo, subscription bool, accessDurationHours *int, acceptedAt time.Time) error {
	if s.EntitlementService == nil {
		return nil
	}
	start := acceptedAt
	if coverage.HasCoverage && coverage.EndDate != nil {
		start = *coverage.EndDate
	}
	sourceType := models.AccessSourcePurchase
	if subscription {
		sourceType = models.AccessSourceSubscription
	}
	exists, err := s.EntitlementService.AccessExistsBySource(ctx, sourceType, sourceID.String(), productID)
	if err != nil {
		return fmt.Errorf("check existing access source: %w", err)
	}
	if exists {
		return nil
	}
	params := entitlements.PushAccessParams{UserID: userID, ProductID: productID, NotBefore: &start, SourceType: sourceType, SourceID: sourceID.String()}
	if !subscription {
		params.PaymentID = &paymentID
	}
	if accessDurationHours != nil && *accessDurationHours > 0 {
		end := start.Add(time.Duration(*accessDurationHours) * time.Hour).UTC()
		params.EndsAt = &end
	} else {
		params.Indefinite = true
	}
	if _, err := s.EntitlementService.PushAccess(ctx, params); err != nil {
		log.WithError(err).WithFields(log.Fields{"user_id": userID, "product_id": productID, "payment_id": paymentID}).Error("failed to grant purchase access")
		return err
	}
	return nil
}

func nonNilKeys(keys []string) []string {
	if keys == nil {
		return []string{}
	}
	return keys
}

// grantPurchasedCredits fulfills the payment snapshot using the existing credit
// ledger in the same transaction as the receipt. Replays retain an expired or
// revoked lot; they never mint replacement credit.
func (s *CheckoutPurchaseService) grantPurchasedCredits(ctx context.Context, payment *models.Payment, productID uuid.UUID) error {
	credit := payment.CreditGrantSnapshot
	if credit == nil {
		return nil
	}
	if err := credit.Validate(); err != nil {
		return err
	}
	if s.transactionDB == nil {
		return errors.New("purchased credit requires the payment transaction")
	}
	_, err := purchasedcredits.New(s.transactionDB, s.clock).Fund(ctx, purchasedcredits.Params{
		CustomerID: identity.CustomerID(payment.CustomerID), PaymentID: payment.ID, ProductID: productID,
		Amount: credit.Amount, PaidAmount: payment.Amount, Currency: credit.Currency,
		StartsAt: credit.StartsAt, ExpiresAt: credit.ExpiresAt,
	})
	return err
}

// queueReceipt queues the receipt of the one-off payment applyPurchase
// created, in its transaction.
func (s *CheckoutPurchaseService) queueReceipt(ctx context.Context, payment *models.Payment, product *models.Product) error {
	if s.transactionDB == nil {
		return errors.New("a purchase receipt requires the payment transaction")
	}
	if strings.TrimSpace(product.DisplayName) == "" && product.Key == "" {
		stored, err := s.ProductService.GetByID(ctx, product.ID)
		if err != nil {
			return err
		}
		product = stored
	}
	name := strings.TrimSpace(product.DisplayName)
	if name == "" {
		name = product.Key
	}
	return subscriptions.QueuePurchaseReceipt(ctx, s.transactionDB, subscriptions.PurchaseReceipt{PaymentID: payment.ID, CustomerID: payment.CustomerID,
		Items: name, Amount: payment.Amount, Currency: payment.Currency, Rail: string(payment.Rail), PaidAt: payment.PurchasedAt})
}
