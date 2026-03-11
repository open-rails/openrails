package checkout

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	log "github.com/sirupsen/logrus"
)

type checkoutSubscriptionAccess interface {
	GetActiveOrPendingByUserIDAndTierGroup(ctx context.Context, userID, tierGroup string) (*models.Subscription, error)
	GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error)
}

type CheckoutPurchaseService struct {
	PriceService        *catalog.PriceService
	ProductService      *catalog.ProductService
	PaymentService      *payments.PaymentService
	EntitlementService  *entitlements.EntitlementService
	SubscriptionService checkoutSubscriptionAccess
	Clock               clockwork.Clock
}

func NewCheckoutPurchaseService(
	priceService *catalog.PriceService,
	productService *catalog.ProductService,
	paymentService *payments.PaymentService,
	entitlementService *entitlements.EntitlementService,
	subscriptionService checkoutSubscriptionAccess,
) *CheckoutPurchaseService {
	return &CheckoutPurchaseService{
		PriceService:        priceService,
		ProductService:      productService,
		PaymentService:      paymentService,
		EntitlementService:  entitlementService,
		SubscriptionService: subscriptionService,
	}
}

func (s *CheckoutPurchaseService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

func (s *CheckoutPurchaseService) CheckPurchaseEligibility(ctx context.Context, userID string, priceID uuid.UUID) (*EligibilityResult, error) {
	price, err := s.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsActive {
		return &EligibilityResult{Status: EligibilityBlocked, Reason: "price is not available for purchase"}, nil
	}

	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found: %w", err)
	}
	if !product.IsActive {
		return &EligibilityResult{Status: EligibilityBlocked, Reason: "product is not available for purchase"}, nil
	}

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

func (s *CheckoutPurchaseService) GetUserProductCoverage(ctx context.Context, userID string, product *models.Product) (*CoverageInfo, error) {
	now := s.now()
	coverage := &CoverageInfo{HasCoverage: false}

	if s.SubscriptionService != nil {
		sub, err := s.SubscriptionService.GetActiveOrPendingByUserIDAndProductID(ctx, userID, product.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("failed to check subscription: %w", err)
		}
		if sub != nil {
			coverage.HasCoverage = true
			coverage.SourceType = "subscription"
			coverage.SourceID = &sub.ID
			if sub.CurrentPeriodEndsAt == nil || sub.CurrentPeriodEndsAt.IsZero() {
				coverage.IsIndefinite = true
				return coverage, nil
			}
			coverage.EndDate = sub.CurrentPeriodEndsAt
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
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
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
	if req.Processor == "" {
		return nil, errors.New("processor is required")
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
		purchasedAt = req.PurchasedAt.UTC()
	}

	paymentID := uuid.New()
	payment := &models.Payment{
		ID:               paymentID,
		UserID:           req.UserID,
		PriceID:          price.ID,
		SubscriptionID:   req.SubscriptionID,
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

	sourceID := paymentID
	if req.SubscriptionID != nil {
		log.WithFields(log.Fields{"payment_id": paymentID, "user_id": req.UserID, "price_id": req.PriceID, "subscription_id": req.SubscriptionID}).Info("registered subscription payment")
		sourceID = *req.SubscriptionID
	}

	var grantedEntitlements []string
	if err := s.grantProductEntitlements(ctx, req.UserID, product, sourceID, coverage, req.SubscriptionID != nil && req.SubscriptionID.String() != "", req.WalletPurchase, price.BillingCycleDays); err != nil {
		log.WithError(err).WithField("payment_id", sourceID).Error("failed to grant entitlements after payment")
	} else if product.EntitlementsSpec != nil {
		for entName := range product.EntitlementsSpec {
			grantedEntitlements = append(grantedEntitlements, entName)
		}
	}

	var delayedStart *time.Time
	if coverage.HasCoverage && coverage.EndDate != nil {
		delayedStart = coverage.EndDate
	}

	log.WithFields(log.Fields{
		"payment_id": paymentID, "user_id": req.UserID, "price_id": req.PriceID, "product_id": product.ID,
		"processor": req.Processor, "transaction_id": req.TransactionID, "entitlements": grantedEntitlements,
		"delayed_start": delayedStart, "eligibility": eligibility.Status,
	}).Info("registered purchase")

	return &payments.RegisterPurchaseResponse{PaymentID: paymentID, Entitlements: grantedEntitlements, DelayedStart: delayedStart, Eligibility: string(eligibility.Status)}, nil
}

func (s *CheckoutPurchaseService) grantProductEntitlements(ctx context.Context, userID string, product *models.Product, paymentID uuid.UUID, coverage *CoverageInfo, subscription bool, walletPurchase bool, billingCycleDays *int) error {
	if s.EntitlementService == nil || product.EntitlementsSpec == nil {
		return nil
	}

	now := s.now()
	for entitlementName, durationDays := range product.EntitlementsSpec {
		startAt := now
		if coverage.HasCoverage && coverage.EndDate != nil {
			startAt = *coverage.EndDate
		}

		var endAt *time.Time
		if walletPurchase && billingCycleDays == nil {
			newDate := startAt.AddDate(0, 1, 0)
			endAt = &newDate
		} else if billingCycleDays != nil && *billingCycleDays > 0 {
			end := startAt.Add(time.Duration(*billingCycleDays) * 24 * time.Hour)
			endAt = &end
		}
		if durationDays != nil && *durationDays > 0 {
			end := startAt.Add(time.Duration(*durationDays) * 24 * time.Hour)
			endAt = &end
		}

		sourceType := models.EntitlementSourceOneOff
		if subscription {
			sourceType = models.EntitlementSourceSubscription
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
