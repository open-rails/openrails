package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/doujins-org/doujins-billing/internal/db"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/doujins-org/doujins-billing/internal/db/repo"
	"github.com/doujins-org/doujins-billing/internal/integrations/ccbill"
	"github.com/doujins-org/doujins-billing/internal/integrations/nmi"
	"github.com/doujins-org/doujins-billing/internal/processors"
	"github.com/doujins-org/doujins-billing/pkg/api"
	"github.com/doujins-org/doujins-billing/pkg/query"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
)

type SubscribeData struct {
	Email           string `json:"email"`
	FirstName       string `json:"first_name"`
	LastName        string `json:"last_name"`
	Address1        string `json:"address1"`
	City            string `json:"city"`
	State           string `json:"state"`
	Zip             string `json:"zip"`
	Country         string `json:"country"`
	PriceID         string `json:"price_id"`
	Processor       string `json:"processor,omitempty"` // Processor: mobius, ccbill, solana
	PaymentToken    string `json:"payment_token,omitempty"`
	PaymentMethodID string `json:"payment_method_id,omitempty"`
}

type GetSubscriptionsFilters struct {
	UserID          string     `form:"user_id"`
	Status          string     `form:"status"`
	PriceID         uuid.UUID  `form:"price_id"`
	Processor       string     `form:"processor"`
	CreatedAfter    *time.Time `form:"created_after" time_format:"2006-01-02"`
	CreatedBefore   *time.Time `form:"created_before" time_format:"2006-01-02"`
	CancelledAfter  *time.Time `form:"cancelled_after" time_format:"2006-01-02"`
	CancelledBefore *time.Time `form:"cancelled_before" time_format:"2006-01-02"`
	ExpiresBefore   *time.Time `form:"expires_before" time_format:"2006-01-02"`
	SortBy          string     `form:"sort_by"`    // created_at (default), expires_at, cancelled_at
	SortOrder       string     `form:"sort_order"` // asc, desc (default)
}

type SubscriptionService struct {
	subscriptionRepo     *repo.SubscriptionRepo
	Clock                clockwork.Clock
	PriceService         *PriceService
	ProductService       *ProductService
	NotificationService  *NotificationService
	CCBillRESTClient     *ccbill.RESTClient
	NMIClients           map[string]*nmi.NMIClient
	PaymentMethodService *PaymentMethodService
	VaultService         *VaultService
	IdempotencyService   *IdempotencyService
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *SubscriptionService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

func (s *SubscriptionService) nmiClientForProcessor(provider string) (*nmi.NMIClient, error) {
	providerKey := strings.TrimSpace(strings.ToLower(provider))
	if providerKey == "" {
		providerKey = "mobius"
	}
	if s.NMIClients == nil {
		return nil, fmt.Errorf("nmi provider '%s' is not configured", providerKey)
	}
	client, ok := s.NMIClients[providerKey]
	if !ok {
		return nil, fmt.Errorf("nmi provider '%s' is not configured", providerKey)
	}
	return client, nil
}

type PaymentProcessor = int

const (
	ProcessorCCBill = "ccbill"
	ProcessorStripe = "stripe"
	// Deprecated: ProcessorNMI is deprecated. Use "mobius" or other NMI-backed processor names instead.
	ProcessorNMI = "nmi"

	CurrencyUSD = "usd"
	CurrencyEUR = "eur"

	BillingCycleMonthly = 30

	WebhookSourceCCBill = "ccbill_webhook"
	WebhookSourceNMI    = "nmi_webhook"
	WebhookSourceSystem = "system"

	EventReasonSubscriptionExpired        = "subscription_expired"
	EventReasonSubscriptionDeletedWebhook = "subscription_deleted_via_webhook"
	EventReasonPaymentDeclined            = "payment_declined"

	StatusMessagePending = "pending"
)

const (
	CCBill PaymentProcessor = iota
	NMI
)

var (
	PaymentProcessors = map[string]PaymentProcessor{
		ProcessorCCBill: CCBill,
		ProcessorNMI:    NMI,
		"mobius":        NMI, // mobius uses NMI gateway
	}
)

type SubscribeResponse struct {
	URL            string `json:"url,omitempty"`
	Status         string `json:"status,omitempty"`
	Message        string `json:"message,omitempty"`
	SubscriptionID string `json:"subscription_id,omitempty"`
}

func (s *SubscriptionService) Subscribe(ctx context.Context, data *SubscribeData, user *UserIdentity) (any, error) {
	priceID, err := api.ParsePriceID(data.PriceID)
	if err != nil {
		return nil, fmt.Errorf("invalid price ID: %w", err)
	}

	price, err := s.PriceService.GetByID(ctx, priceID)
	if err != nil {
		return nil, fmt.Errorf("price not found: %w", err)
	}
	if !price.IsActive {
		return nil, errors.New("price is not available for purchase")
	}
	if s.ProductService != nil {
		product, err := s.ProductService.GetByID(ctx, price.ProductID)
		if err != nil {
			return nil, fmt.Errorf("product not found: %w", err)
		}
		if !product.IsActive {
			return nil, errors.New("product is not available for purchase")
		}
	}

	existingSub, err := s.GetByUserID(ctx, user.ID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("failed to check existing subscription: %w", err)
	}

	if existingSub != nil && existingSub.Status == models.StatusActive {
		return nil, errors.New("user already has an active subscription")
	}

	processor := strings.TrimSpace(strings.ToLower(data.Processor))
	if processor == "" {
		processor = ProcessorCCBill // default
	}

	switch processor {
	case ProcessorCCBill:
		// CCBill now uses FlexForm integration instead of direct payment processing
		return map[string]any{
			"status":  "redirect_required",
			"message": "CCBill payments now use FlexForm integration",
			"instructions": map[string]string{
				"step1": "Generate FlexForm URL using POST /v1/subscriptions/ccbill/flexform-url",
				"step2": "Redirect the user to the returned redirect_url",
				"step3": "User completes payment on the hosted CCBill page",
				"step4": "Subscription will be activated via webhook upon successful payment",
			},
			"flexform_endpoint": "/v1/subscriptions/ccbill/flexform-url",
		}, nil
	case "mobius", ProcessorNMI: // mobius is the processor, NMI is the underlying gateway
		// Get NMI config from price's processors JSONB
		nmiPlanID, _, hasNMI := price.GetNMIConfig()
		if !hasNMI || nmiPlanID == "" {
			return nil, fmt.Errorf("price %s is missing an NMI plan configuration", price.ID)
		}

		// mobius is the default NMI provider
		provider := "mobius"

		client, err := s.nmiClientForProcessor(provider)
		if err != nil {
			return nil, err
		}

		email := strings.TrimSpace(data.Email)

		subscriptionID := uuid.New()

		var customerVaultID string
		var createdPaymentMethod *models.PaymentMethod
		paymentMethodID := strings.TrimSpace(data.PaymentMethodID)
		if paymentMethodID != "" {
			if s.PaymentMethodService == nil {
				return nil, errors.New("payment method service unavailable")
			}

			pmID, err := api.ParsePaymentMethodID(paymentMethodID)
			if err != nil {
				return nil, fmt.Errorf("invalid payment method ID: %w", err)
			}

			paymentMethod, err := s.PaymentMethodService.ValidatePaymentMethodOperation(ctx, pmID, user.ID)
			if err != nil {
				return nil, fmt.Errorf("unable to use saved payment method: %w", err)
			}

			if !processors.IsNMIBackedProcessor(paymentMethod.Processor) {
				return nil, errors.New("saved payment method is not compatible with NMI-backed subscriptions")
			}

			customerVaultID = paymentMethod.VaultID
			// Use the processor from the payment method
			provider = strings.ToLower(string(paymentMethod.Processor))
		}

		trimmedToken := strings.TrimSpace(data.PaymentToken)
		if trimmedToken == "" && customerVaultID == "" {
			return nil, errors.New("payment token or payment method ID is required")
		}

		const idemOp = "nmi_subscription"
		idemKey := buildNMIIdempotencyKey(user.ID, priceID, trimmedToken, paymentMethodID)
		var idemClaimed bool
		if s.IdempotencyService != nil {
			rec, exists, err := s.IdempotencyService.Begin(ctx, idemOp, idemKey)
			if err != nil {
				return nil, fmt.Errorf("start idempotency window: %w", err)
			}
			if exists {
				if rec.Status == IdempotencyStatusSuccess {
					return nil, errors.New("duplicate subscription request detected (already succeeded)")
				}
				if rec.Status == IdempotencyStatusPending {
					return nil, errors.New("subscription request in progress, please wait")
				}
				// Failed status - allow retry
			}
			idemClaimed = !exists
		}

		params := nmi.RecurringPaymentData{
			CardUserData: nmi.CardUserData{
				FirstName: data.FirstName,
				LastName:  data.LastName,
				Address1:  data.Address1,
				City:      data.City,
				State:     data.State,
				Zip:       data.Zip,
				Country:   data.Country,
			},
			PlanID:     nmiPlanID,
			Amount:     float64(price.Amount) / 100.0, // Convert cents to dollars for NMI API
			Currency:   price.Currency,
			Email:      email,
			OrderID:    subscriptionID.String(),
			PONumber:   subscriptionID.String(),
			CustomerID: user.ID,
		}

		if strings.TrimSpace(params.FirstName) == "" {
			params.FirstName = user.Username
		}
		if strings.TrimSpace(params.LastName) == "" {
			params.LastName = "Member"
		}
		if strings.TrimSpace(params.Address1) == "" {
			params.Address1 = "N/A"
		}
		if strings.TrimSpace(params.City) == "" {
			params.City = "N/A"
		}
		if strings.TrimSpace(params.State) == "" {
			params.State = "N/A"
		}
		if strings.TrimSpace(params.Zip) == "" {
			params.Zip = "00000"
		}
		if strings.TrimSpace(params.Country) == "" {
			params.Country = "US"
		}

		if trimmedToken != "" && customerVaultID == "" {
			if s.VaultService == nil {
				return nil, errors.New("vault service unavailable")
			}

			vaultReq := &CreateVaultRequest{
				PaymentToken: trimmedToken,
				Provider:     provider,
				FirstName:    params.FirstName,
				LastName:     params.LastName,
				Address1:     params.Address1,
				City:         params.City,
				State:        params.State,
				Zip:          params.Zip,
				Country:      params.Country,
				Email:        email,
			}

			pm, err := s.VaultService.CreateVault(ctx, user, vaultReq)
			if err != nil {
				return nil, fmt.Errorf("failed to create payment method: %w", err)
			}

			customerVaultID = strings.TrimSpace(pm.VaultID)
			if customerVaultID == "" {
				return nil, errors.New("failed to create payment method vault: empty customer vault ID")
			}
			createdPaymentMethod = pm
			provider = strings.ToLower(string(pm.Processor))
		}

		if customerVaultID != "" {
			params.CustomerVaultID = customerVaultID
		} else if trimmedToken != "" {
			params.PaymentToken = trimmedToken
		}

		resp, err := client.AddRecurringSubscription(params)
		if err != nil {
			if idemClaimed && s.IdempotencyService != nil {
				_ = s.IdempotencyService.Fail(ctx, idemOp, idemKey, err)
			}
			if createdPaymentMethod != nil && s.VaultService != nil {
				cleanupErr := s.VaultService.DeleteVault(ctx, createdPaymentMethod)
				if cleanupErr != nil {
					log.WithError(cleanupErr).WithFields(log.Fields{
						"vault_id": customerVaultID,
						"user_id":  user.ID,
					}).Warn("failed to cleanup payment method after subscription error")
				}
			}
			return nil, err
		}
		if idemClaimed && s.IdempotencyService != nil {
			payload, marshalErr := json.Marshal(resp)
			if marshalErr != nil {
				_ = s.IdempotencyService.Fail(ctx, idemOp, idemKey, marshalErr)
			} else {
				_ = s.IdempotencyService.Complete(ctx, idemOp, idemKey, payload)
			}
		}

		var emailCopy *string
		if email != "" {
			emailCopy = &email
		}
		subscription := &models.Subscription{
			UserID:                  user.ID,
			ProductID:               price.ProductID,
			PriceID:                 priceID,
			ID:                      subscriptionID,
			ProcessorSubscriptionID: resp.SubscriptionID,
			Status:                  models.StatusPending,
			Processor:               models.ProcessorMobius,
			UserEmail:               emailCopy,
		}
		if strings.TrimSpace(data.PaymentMethodID) != "" {
			pmUUID, _ := api.ParsePaymentMethodID(strings.TrimSpace(data.PaymentMethodID))
			subscription.PaymentMethodID = &pmUUID
		} else if createdPaymentMethod != nil {
			subscription.PaymentMethodID = &createdPaymentMethod.ID
		}

		if err := s.Create(ctx, subscription); err != nil {
			return nil, fmt.Errorf("failed to create subscription: %w", err)
		}
		return resp, nil
	default:
		return nil, errors.New("invalid payment processor")
	}
}

// GetUserSubscription retrieves the current subscription for a user
func (s *SubscriptionService) GetUserSubscription(ctx context.Context, userID string) (*models.Subscription, error) {
	return s.GetByUserID(ctx, userID)
}

// CancelUserSubscription cancels a user's subscription
func (s *SubscriptionService) CancelUserSubscription(ctx context.Context, userID string, feedback string) error {
	subscription, err := s.GetByUserID(ctx, userID)
	if err != nil {
		return fmt.Errorf("subscription not found: %w", err)
	}

	if subscription.Status != models.StatusActive {
		return errors.New("subscription is not active")
	}

	now := s.now()
	cancelType := models.CancelTypeUser
	subscription.Status = models.StatusCancelled
	subscription.CancelledAt = &now
	subscription.CancelType = &cancelType
	if feedback != "" {
		subscription.CancelFeedback = &feedback
	}

	if err := s.Update(ctx, subscription); err != nil {
		return fmt.Errorf("failed to update subscription: %w", err)
	}

	// Entitlements are managed in lifecycle and user flows

	// Add notification
	notification := &models.NotificationQueue{
		ID:        uuid.New(),
		UserID:    userID,
		EventType: models.NotificationPremiumEnded,
		Data: map[string]any{
			"reason": string(PremiumEndReasonUserCancel),
		},
	}
	if err := s.NotificationService.Create(ctx, notification); err != nil {
		log.WithError(err).Error("failed to create cancellation notification")
	}

	return nil
}

func buildNMIIdempotencyKey(userID string, priceID uuid.UUID, paymentToken string, paymentMethodID string) string {
	token := strings.TrimSpace(paymentToken)
	method := strings.TrimSpace(paymentMethodID)
	if token == "" {
		token = "none"
	}
	if method == "" {
		method = "none"
	}
	return fmt.Sprintf("user:%s:price:%s:token:%s:method:%s", strings.TrimSpace(userID), priceID.String(), token, method)
}

// GetAvailableProducts returns all active products with their prices
func (s *SubscriptionService) GetAvailableProducts(ctx context.Context) ([]*models.Product, error) {
	products, err := s.ProductService.GetActive(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get active products: %w", err)
	}

	// Load prices for each product
	for _, product := range products {
		prices, err := s.PriceService.GetActiveByProductID(ctx, product.ID)
		if err != nil {
			log.WithFields(log.Fields{
				"product_id": product.ID,
				"error":      err.Error(),
			}).Warn("Failed to load prices for product")
			continue
		}
		product.Prices = prices
	}

	return products, nil
}

func NewSubscriptionService(
	db *db.DB,
	priceService *PriceService,
	productService *ProductService,
	notificationService *NotificationService,
	ccbillRESTClient *ccbill.RESTClient,
	nmiClients map[string]*nmi.NMIClient,
	paymentMethodService *PaymentMethodService,
) *SubscriptionService {
	return &SubscriptionService{
		subscriptionRepo:     repo.NewSubscriptionRepo(db),
		PriceService:         priceService,
		ProductService:       productService,
		NotificationService:  notificationService,
		CCBillRESTClient:     ccbillRESTClient,
		NMIClients:           nmiClients,
		PaymentMethodService: paymentMethodService,
	}
}

func (s *SubscriptionService) Create(ctx context.Context, subscription *models.Subscription) error {
	return s.subscriptionRepo.Create(ctx, subscription)
}

func (s *SubscriptionService) GetByID(ctx context.Context, id uuid.UUID) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByID(ctx, id)
}

func (s *SubscriptionService) GetByUserID(ctx context.Context, id string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetLatestByUserID(ctx, id)
}

func (s *SubscriptionService) GetByUserIDAndPriceID(ctx context.Context, id string, priceID uuid.UUID) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByUserIDAndPriceID(ctx, id, priceID)
}

// GetActiveOrPendingByUserIDAndProductID returns an active or pending subscription for a user and product.
// Uses the denormalized ProductID field for efficient lookup.
func (s *SubscriptionService) GetActiveOrPendingByUserIDAndProductID(ctx context.Context, userID string, productID uuid.UUID) (*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveOrPendingByUserIDAndProductID(ctx, userID, productID)
}

// GetActiveOrPendingByUserIDAndTierGroup returns an active or pending subscription for a user
// in the specified tier group. Used to detect upgrade/downgrade scenarios.
// Returns the subscription with its Price and Product loaded.
func (s *SubscriptionService) GetActiveOrPendingByUserIDAndTierGroup(ctx context.Context, userID string, tierGroup string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveOrPendingByUserIDAndTierGroup(ctx, userID, tierGroup)
}

func (s *SubscriptionService) Update(ctx context.Context, subscription *models.Subscription) error {
	return s.subscriptionRepo.Update(ctx, subscription)
}

func (s *SubscriptionService) GetSubscribers(ctx context.Context, params query.QueryOptions[GetSubscriptionsFilters]) ([]*models.Subscription, int64, error) {
	repoParams := query.QueryOptions[repo.SubscriptionFilters]{
		Filters: repo.SubscriptionFilters{
			UserID:          params.Filters.UserID,
			Status:          params.Filters.Status,
			PriceID:         params.Filters.PriceID,
			Processor:       params.Filters.Processor,
			CreatedAfter:    params.Filters.CreatedAfter,
			CreatedBefore:   params.Filters.CreatedBefore,
			CancelledAfter:  params.Filters.CancelledAfter,
			CancelledBefore: params.Filters.CancelledBefore,
			ExpiresBefore:   params.Filters.ExpiresBefore,
			SortBy:          params.Filters.SortBy,
			SortOrder:       params.Filters.SortOrder,
		},
		Limit:  params.Limit,
		Offset: params.Offset,
	}

	return s.subscriptionRepo.GetSubscribers(ctx, repoParams)
}

func (s *SubscriptionService) GetPaginatedByUserID(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	return s.subscriptionRepo.GetPaginatedByUserID(ctx, userID, page, pageSize)
}

// GetSubscriptionsWithDetailsForUser retrieves subscriptions with related price information for billing history
func (s *SubscriptionService) GetSubscriptionsWithDetailsForUser(ctx context.Context, userID string, page, pageSize int) ([]models.Subscription, int, error) {
	return s.subscriptionRepo.GetSubscriptionsWithDetailsForUser(ctx, userID, page, pageSize)
}

// GetActiveSubscriptionsByUserID retrieves only active subscriptions for a user
func (s *SubscriptionService) GetActiveSubscriptionsByUserID(ctx context.Context, userID string) ([]models.Subscription, error) {
	return s.subscriptionRepo.GetActiveSubscriptionsByUserID(ctx, userID)
}

// GetSubscriptionsByProcessorAndUserID retrieves subscriptions filtered by processor
func (s *SubscriptionService) GetSubscriptionsByProcessorAndUserID(ctx context.Context, userID string, processor models.Processor) ([]models.Subscription, error) {
	return s.subscriptionRepo.GetSubscriptionsByProcessorAndUserID(ctx, userID, processor)
}

// GetActiveSubscription retrieves the active subscription for a user
func (s *SubscriptionService) GetActiveSubscription(ctx context.Context, userID string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveSubscription(ctx, userID)
}

// GetByProcessorSubscriptionID finds a subscription by processor, gateway (optional), and processor_subscription_id
func (s *SubscriptionService) GetByProcessorSubscriptionID(ctx context.Context, processor, gateway, processorSubscriptionID string) (*models.Subscription, error) {
	return s.subscriptionRepo.GetByProcessorSubscriptionID(ctx, processor, gateway, processorSubscriptionID)
}

// GetActiveSubscriptionsByProcessor gets all active subscriptions for a processor
func (s *SubscriptionService) GetActiveSubscriptionsByProcessor(ctx context.Context, processor string) ([]*models.Subscription, error) {
	return s.subscriptionRepo.GetActiveSubscriptionsByProcessor(ctx, processor)
}

// Delete removes a subscription from the database permanently
func (s *SubscriptionService) Delete(ctx context.Context, id uuid.UUID) error {
	return s.subscriptionRepo.Delete(ctx, id)
}
