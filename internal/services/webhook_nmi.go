package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db/models"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/nmi"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
)

const NMIProcessorName string = "NMI"

type NMIWebhookService struct {
	DB                           *db.DB
	Clock                        clockwork.Clock
	PriceService                 *PriceService
	ProductService               *ProductService
	Data                         NMIWebhookEvent
	Processor                    string
	NMIClient                    *nmi.NMIClient
	EventLogService              *EventLogService
	SubscriptionService          *SubscriptionService
	PaymentService               *PaymentService
	CreditsService               *CreditsService
	DeduplicationService         *DeduplicationService
	NotificationService          *NotificationService
	SubscriptionLifecycleService *SubscriptionLifecycleService
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *NMIWebhookService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

type NMIWebhookEventType = string

const (
	// Subscription lifecycle events
	EventTypeNMIAddSubscription    NMIWebhookEventType = "recurring.subscription.add"
	EventTypeNMIUpdateSubscription NMIWebhookEventType = "recurring.subscription.update"
	EventTypeNMIDeleteSubscription NMIWebhookEventType = "recurring.subscription.delete"

	// Transaction events - sales
	EventTypeNMITransactionSuccess NMIWebhookEventType = "transaction.sale.success"
	EventTypeNMITransactionFailure NMIWebhookEventType = "transaction.sale.failure"

	// Transaction events - refunds
	EventTypeNMIRefundSuccess NMIWebhookEventType = "transaction.refund.success"
	EventTypeNMIRefundFailure NMIWebhookEventType = "transaction.refund.failure"

	// Transaction events - voids
	EventTypeNMIVoidSuccess NMIWebhookEventType = "transaction.void.success"
	EventTypeNMIVoidFailure NMIWebhookEventType = "transaction.void.failure"

	// Automatic Card Updater (ACU) events
	EventTypeNMIACUUpdated         NMIWebhookEventType = "acu.summary.automaticallyupdated"
	EventTypeNMIACUContactCustomer NMIWebhookEventType = "acu.summary.contactcustomer"
	EventTypeNMIACUClosedAccount   NMIWebhookEventType = "acu.summary.closedaccount"

	// Chargeback events
	EventTypeNMIChargebackComplete NMIWebhookEventType = "chargeback.batch.complete"
)

func (s *NMIWebhookService) parseRecurringEventBody() (*NMIRecurringEventBody, error) {
	var body NMIRecurringEventBody
	if err := json.Unmarshal(s.Data.EventBody, &body); err != nil {
		return nil, fmt.Errorf("failed to parse recurring event body: %w", err)
	}
	return &body, nil
}

func (s *NMIWebhookService) parseTransactionEventBody() (*NMITransactionEventBody, error) {
	var body NMITransactionEventBody
	if err := json.Unmarshal(s.Data.EventBody, &body); err != nil {
		return nil, fmt.Errorf("failed to parse transaction event body: %w", err)
	}
	return &body, nil
}

func (s *NMIWebhookService) parseACUEventBody() (*NMIACUEventBody, error) {
	var body NMIACUEventBody
	if err := json.Unmarshal(s.Data.EventBody, &body); err != nil {
		return nil, fmt.Errorf("failed to parse ACU event body: %w", err)
	}
	return &body, nil
}

func (s *NMIWebhookService) parseChargebackBatchEventBody() (*NMIChargebackBatchEventBody, error) {
	var body NMIChargebackBatchEventBody
	if err := json.Unmarshal(s.Data.EventBody, &body); err != nil {
		return nil, fmt.Errorf("failed to parse chargeback batch event body: %w", err)
	}
	return &body, nil
}

func transactionSubscriptionID(body *NMITransactionEventBody) string {
	if body == nil {
		return ""
	}

	candidates := []string{}
	if body.Subscription != nil {
		candidates = append(candidates, body.Subscription.SubscriptionID.Trimmed())
	}
	/*if body.TransactionDetail != nil && body.TransactionDetail.Subscription != nil {
		candidates = append(candidates, body.TransactionDetail.Subscription.SubscriptionID.Trimmed())
	}*/
	candidates = append(candidates, body.OrderID.Trimmed())
	if body.TransactionDetail != nil {
		candidates = append(candidates, body.TransactionDetail.OrderID.Trimmed())
	}
	candidates = append(candidates, body.PONumber.Trimmed())
	if body.TransactionDetail != nil {
		candidates = append(candidates, body.TransactionDetail.PONumber.Trimmed())
	}
	/*
		candidates = append(candidates, body.CustomerID.Trimmed())
		if body.TransactionDetail != nil {
			candidates = append(candidates, body.TransactionDetail.CustomerID.Trimmed())
		}*/

	for _, candidate := range candidates {
		log.Println("[DEBUG] Checking candidate ID:", candidate)
		if candidate != "" {
			return candidate
		}
	}

	return ""
}

func transactionActionSource(body *NMITransactionEventBody) string {
	if body == nil {
		return ""
	}
	if body.Action != nil && body.Action.Source != "" {
		return strings.ToLower(strings.TrimSpace(body.Action.Source))
	}
	if body.TransactionDetail != nil && body.TransactionDetail.Action != nil && body.TransactionDetail.Action.Source != "" {
		return strings.ToLower(strings.TrimSpace(body.TransactionDetail.Action.Source))
	}
	return ""
}

func isRecurringSource(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "recurring", "retry":
		return true
	default:
		return false
	}
}

func transactionPlanID(body *NMITransactionEventBody) string {
	if body == nil {
		return ""
	}
	if body.Subscription != nil && !body.Subscription.PlanID.IsEmpty() {
		return body.Subscription.PlanID.Trimmed()
	}
	if body.TransactionDetail != nil && body.TransactionDetail.Subscription != nil {
		return body.TransactionDetail.Subscription.PlanID.Trimmed()
	}
	return ""
}

func transactionAmount(body *NMITransactionEventBody) (float64, error) {
	if body == nil {
		return 0, fmt.Errorf("transaction body is nil")
	}
	if amt, err := body.Amount.Float64(); err == nil {
		return amt, nil
	}
	if body.TransactionDetail != nil {
		if amt, err := body.TransactionDetail.Amount.Float64(); err == nil {
			return amt, nil
		}
	}
	return 0, fmt.Errorf("amount not provided")
}

func transactionCurrency(body *NMITransactionEventBody) string {
	if body == nil {
		return ""
	}
	if curr := body.Currency.Trimmed(); curr != "" {
		return curr
	}
	if body.TransactionDetail != nil {
		return body.TransactionDetail.Currency.Trimmed()
	}
	return ""
}

func parseTransactionAmount(body *NMITransactionEventBody) (float64, error) {
	if body == nil {
		return 0, fmt.Errorf("nil transaction body")
	}
	amountStr := body.Amount.Trimmed()
	if amountStr == "" && body.TransactionDetail != nil {
		amountStr = body.TransactionDetail.Amount.Trimmed()
	}

	if amountStr == "" && body.Action != nil {
		amountStr = body.Action.Amount.Trimmed()
	}
	if amountStr == "" {
		return 0, fmt.Errorf("no amount in transaction body")
	}

	var amount float64
	if _, err := fmt.Sscanf(amountStr, "%f", &amount); err != nil {
		return 0, fmt.Errorf("failed to parse amount '%s': %w", amountStr, err)
	}
	return amount, nil
}

type NMIBillingError struct {
	Type    string                 `json:"type"`
	Message string                 `json:"message"`
	Context map[string]interface{} `json:"context"`
	Err     error                  `json:"-"`
}

func (be *NMIBillingError) Error() string {
	if be.Err != nil {
		return fmt.Sprintf("%s: %s (%v)", be.Type, be.Message, be.Err)
	}
	return fmt.Sprintf("%s: %s", be.Type, be.Message)
}

func (be *NMIBillingError) Unwrap() error {
	return be.Err
}

const (
	ErrorTypeNMIValidation    = "validation_error"
	ErrorTypeNMIAmount        = "amount_mismatch"
	ErrorTypeNMIDuplicate     = "duplicate_transaction"
	ErrorTypeNMIStatusChange  = "invalid_status_change"
	ErrorTypeNMIBusinessLogic = "business_logic_error"
	ErrorTypeNMIDatabase      = "db_error"
	ErrorTypeNMINotFound      = "not_found"
)

func newNMIBillingError(errorType string, message string, context map[string]interface{}, err error) *NMIBillingError {
	return &NMIBillingError{
		Type:    errorType,
		Message: message,
		Context: context,
		Err:     err,
	}
}

func priceSnapshotFromSubscription(sub *models.Subscription) (float64, string, uint32, *uuid.UUID, *uuid.UUID) {
	var priceAmount float64
	priceCurrency := "usd"
	var billingDays uint32
	var productID *uuid.UUID
	var priceID *uuid.UUID

	if sub != nil && sub.Price != nil {
		priceAmount = float64(sub.Price.Amount) / 100.0
		priceCurrency = sub.Price.Currency
		if sub.Price.BillingCycleDays != nil {
			billingDays = uint32(*sub.Price.BillingCycleDays)
		}
		productID = &sub.Price.ProductID
		priceID = &sub.Price.ID
	}

	return priceAmount, priceCurrency, billingDays, productID, priceID
}

// logSubscriptionEvent emits a subscription event with full pricing/status context.
func (s *NMIWebhookService) logSubscriptionEvent(ctx context.Context, sub *models.Subscription, eventType PaymentEventType, processorTransactionID *string, metadata map[string]interface{}, overrideStatus *models.SubscriptionStatus, overrideCancel *models.CancelType) {
	if s.EventLogService == nil || sub == nil {
		return
	}

	status := sub.Status
	if overrideStatus != nil {
		status = *overrideStatus
	}

	cancelType := ""
	if overrideCancel != nil {
		cancelType = string(*overrideCancel)
	} else if sub.CancelType != nil {
		cancelType = string(*sub.CancelType)
	}

	priceAmount, priceCurrency, billingDays, productID, priceID := priceSnapshotFromSubscription(sub)

	var procSubID *string
	if sub.ProcessorSubscriptionID != "" {
		procSubID = &sub.ProcessorSubscriptionID
	}

	if metadata == nil {
		metadata = map[string]interface{}{}
	}

	event := SubscriptionEventData{
		EventID:                 uuid.New(),
		SubscriptionID:          sub.ID,
		UserID:                  sub.UserID,
		EventType:               eventType,
		Status:                  string(status),
		CancelType:              cancelType,
		PriceAmount:             priceAmount,
		PriceCurrency:           priceCurrency,
		BillingCycleDays:        billingDays,
		ProductID:               productID,
		PriceID:                 priceID,
		Processor:               s.Processor,
		ProcessorSubscriptionID: procSubID,
		ProcessorTransactionID:  processorTransactionID,
		Metadata:                CreateMetadataJSON(metadata),
		Timestamp:               s.now().UTC(),
	}

	if err := s.EventLogService.LogSubscriptionEvent(ctx, event); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"subscription_id": sub.ID,
			"event_type":      eventType,
		}).Error("Failed to log NMI subscription event")
	}
}

func (s *NMIWebhookService) HandleNMIWebhook(ctx context.Context) error {
	// Use deduplication service if available
	if s.DeduplicationService != nil {
		return s.DeduplicationService.ProcessWebhook(
			ctx,
			s.Data.EventID,
			s.Data.EventType,
			models.Processor(s.Processor),
			s.Data,
			s.handleWebhook,
		)
	}

	return s.handleWebhook(ctx)
}

func (s *NMIWebhookService) handleWebhook(ctx context.Context) error {
	switch s.Data.EventType {
	// Subscription lifecycle events
	case EventTypeNMIAddSubscription:
		return s.handleAddSubscription(ctx)
	case EventTypeNMIUpdateSubscription:
		return s.handleUpdateSubscription(ctx)
	case EventTypeNMIDeleteSubscription:
		return s.handleDeleteSubscription(ctx)
	case EventTypeNMITransactionSuccess:
		return s.handleTransactionSaleSuccess(ctx)
	case EventTypeNMITransactionFailure:
		return s.handleTransactionSaleFailure(ctx)

	// Refund events
	case EventTypeNMIRefundSuccess:
		return s.handleRefundSuccess(ctx)
	case EventTypeNMIRefundFailure:
		return s.handleRefundFailure(ctx)

	// Void events
	case EventTypeNMIVoidSuccess:
		return s.handleVoidSuccess(ctx)
	case EventTypeNMIVoidFailure:
		return s.handleVoidFailure(ctx)

	case EventTypeNMIACUUpdated, EventTypeNMIACUContactCustomer, EventTypeNMIACUClosedAccount:
		return s.handleACUEvent(ctx)

	case EventTypeNMIChargebackComplete:
		return s.handleChargebackComplete(ctx)

	default:
		log.WithContext(ctx).WithFields(log.Fields{
			"processor":  s.Processor,
			"event_type": s.Data.EventType,
		}).Warn("Unsupported NMI webhook event type")
		return fmt.Errorf("unsupported event type: %s", s.Data.EventType)
	}
}

func (s *NMIWebhookService) handleAddSubscription(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing NMI subscription add notification")

	body, err := s.parseRecurringEventBody()
	if err != nil {
		return err
	}

	nmiSubID := body.SubscriptionID.Trimmed()
	if nmiSubID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"event_type": s.Data.EventType,
		}).Warn("NMI subscription add missing subscription ID")
		return newNMIBillingError(ErrorTypeNMIValidation, "Missing subscription ID", map[string]interface{}{}, nil)
	}

	provider := strings.TrimSpace(strings.ToLower(s.Processor))
	if provider == "" {
		provider = "mobius"
	}

	var nmiPlanID string
	if body.Plan != nil {
		nmiPlanID = body.Plan.ID.Trimmed()
	}
	if nmiPlanID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": nmiSubID,
			"event_type":      s.Data.EventType,
		}).Warn("NMI subscription add missing plan ID")
		return newNMIBillingError(ErrorTypeNMIValidation, "Missing plan ID", map[string]interface{}{
			"subscription_id": nmiSubID,
		}, nil)
	}

	price, err := s.PriceService.GetByNMIPlan(ctx, provider, nmiPlanID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": nmiSubID,
			"plan_id":         nmiPlanID,
			"provider":        provider,
		}).WithError(err).Error("Failed to resolve price for NMI plan ID")
		return fmt.Errorf("failed to find price for NMI plan ID %s: %w", nmiPlanID, err)
	}

	subscription, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, provider, nmiSubID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_reference": nmiSubID,
			"provider":               provider,
		}).WithError(err).Error("Failed to load subscription for processor ID")
		return fmt.Errorf("failed to load subscription for processor ID %s: %w", nmiSubID, err)
	}

	if subscription.Status != models.StatusPending {
		log.WithContext(ctx).
			WithField("subscription_id", subscription.ID).
			WithField("processor_subscription_id", nmiSubID).
			Info("Subscription already activated; skipping add event")
		return nil
	}

	// Extract payment info from the plan for the initial charge
	var amountCents int64
	var currency string

	// Get amount from plan (in dollars as string, e.g., "19.00")
	if body.Plan != nil && body.Plan.Amount.Trimmed() != "" {
		amountFloat, err := strconv.ParseFloat(body.Plan.Amount.Trimmed(), 64)
		if err != nil {
			log.WithContext(ctx).
				WithError(err).
				WithField("plan_amount", body.Plan.Amount.Trimmed()).
				Warn("Failed to parse plan amount; falling back to price amount")
		} else {
			amountCents = int64(amountFloat * 100)
		}
	}

	// Fall back to price amount if plan amount not available
	if amountCents == 0 && price.Amount > 0 {
		amountCents = price.Amount
	}

	// Use price currency or default to USD (NMI doesn't include currency in subscription add events)
	currency = strings.ToLower(strings.TrimSpace(price.Currency))
	if currency == "" {
		currency = "usd"
	}

	// Note: NMI subscription add events don't include a transaction ID.
	// The initial transaction will come separately via transaction.sale.success webhook.
	// We use the subscription ID as a reference for tracking.
	transactionRef := nmiSubID //"sub:" + nmiSubID

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":             subscription.ID,
		"processor_subscription_id":   nmiSubID,
		"plan_id":                     nmiPlanID,
		"price_id":                    price.ID,
		"amount_cents":                amountCents,
		"currency":                    currency,
		"transaction_reference":       transactionRef,
		"user_id":                     subscription.UserID,
		"subscription_status":         subscription.Status,
		"price_amount_cents":          price.Amount,
		"price_currency":              price.Currency,
		"subscription_lifecycle_step": "create_membership_from_subscription_add",
	}).Info("Creating membership for NMI subscription add event")

	_, err = s.SubscriptionLifecycleService.CreateMembership(ctx, &CreateMembershipParams{
		PriceID:                 price.ID,
		UserID:                  subscription.UserID,
		Processor:               models.ProcessorMobius,
		ProcessorSubscriptionID: &subscription.ProcessorSubscriptionID,
		UserEmail:               subscription.UserEmail,
		TransactionID:           transactionRef,
		Amount:                  amountCents,
		Currency:                currency,
	})
	if err != nil {
		return fmt.Errorf("failed to create membership: %w", err)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":           subscription.ID,
		"processor_subscription_id": nmiSubID,
		"user_id":                   subscription.UserID,
		"transaction_reference":     transactionRef,
	}).Info("Membership created for NMI subscription add event")

	statusActive := models.StatusActive
	if s.EventLogService != nil {
		metadata := map[string]interface{}{
			"event_type":        s.Data.EventType,
			"processor":         provider,
			"plan_id":           nmiPlanID,
			"transaction_ref":   transactionRef,
			"subscription_id":   subscription.ID.String(),
			"previous_status":   string(subscription.Status),
			"lifecycle_step":    "create_membership_from_subscription_add",
			"processor_account": s.Processor,
		}
		txn := transactionRef

		s.logSubscriptionEvent(ctx, subscription, PaymentEventSubscriptionCreated, &txn, metadata, &statusActive, nil)

	}

	return nil
}

func (s *NMIWebhookService) handleUpdateSubscription(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing NMI subscription update notification")

	body, err := s.parseRecurringEventBody()
	if err != nil {
		return err
	}

	nmiSubID := body.SubscriptionID.Trimmed()
	if nmiSubID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"event_type": s.Data.EventType,
		}).Warn("NMI subscription update missing subscription ID")
		return newNMIBillingError(ErrorTypeNMIValidation, "Missing subscription ID", map[string]interface{}{}, nil)
	}

	var nmiPlanID string
	if body.Plan != nil {
		nmiPlanID = body.Plan.ID.Trimmed()
	}

	fields := log.Fields{
		"subscription_id": nmiSubID,
	}
	if nmiPlanID != "" {
		fields["plan_id"] = nmiPlanID
	}
	if body.NextChargeDate.Trimmed() != "" {
		fields["next_charge_date"] = body.NextChargeDate.Trimmed()
	}

	log.WithContext(ctx).WithFields(fields).Info("NMI subscription metadata updated; no action required")
	return nil
}

func (s *NMIWebhookService) handleDeleteSubscription(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing NMI subscription delete notification")

	body, err := s.parseRecurringEventBody()
	if err != nil {
		return err
	}

	provider := strings.TrimSpace(strings.ToLower(s.Processor))
	if provider == "" {
		provider = "mobius"
	}

	nmiSubID := body.SubscriptionID.Trimmed()
	if nmiSubID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"event_type": s.Data.EventType,
		}).Warn("NMI subscription delete missing subscription ID")
		return newNMIBillingError(ErrorTypeNMIValidation, "Missing subscription ID", map[string]interface{}{}, nil)
	}

	subscription, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, provider, nmiSubID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			log.WithContext(ctx).
				WithField("processor_subscription_id", nmiSubID).
				Warn("Received NMI delete for unknown subscription; ignoring")
			return nil
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"processor_subscription_id": nmiSubID,
		}).WithError(err).Error("Failed to load subscription for delete event")
		return fmt.Errorf("failed to get subscription: %w", err)
	}

	if subscription.Status == models.StatusCancelled {
		log.WithContext(ctx).
			WithField("subscription_id", subscription.ID).
			WithField("processor_subscription_id", nmiSubID).
			Info("Subscription already cancelled; skipping NMI delete event")
		return nil
	}

	cancelFeedback := "Cancelled via NMI webhook"
	processor := models.ProcessorMobius
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":           subscription.ID,
		"processor_subscription_id": nmiSubID,
		"user_id":                   subscription.UserID,
	}).Info("Cancelling subscription via NMI delete event")
	if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &CancelMembershipParams{
		RevokeAccess:            false, // User keeps access until period end
		Processor:               &processor,
		ProcessorSubscriptionID: &nmiSubID,
		CancelFeedback:          &cancelFeedback,
		SubscriptionID:          &subscription.ID,
		CancelType:              models.CancelTypeMerchant,
	}); err != nil {
		return fmt.Errorf("failed to cancel subscription: %w", err)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":           subscription.ID,
		"processor_subscription_id": nmiSubID,
	}).Info("Subscription cancelled via NMI delete event")

	if s.EventLogService != nil {
		cancelType := models.CancelTypeMerchant
		statusCancelled := models.StatusCancelled
		metadata := map[string]interface{}{
			"event_type":      s.Data.EventType,
			"processor":       provider,
			"previous_status": string(subscription.Status),
			"status_after":    string(statusCancelled),
		}
		s.logSubscriptionEvent(ctx, subscription, PaymentEventSubscriptionCancelled, nil, metadata, &statusCancelled, &cancelType)
	}

	return nil
}

// handleChargebackComplete processes chargeback batch completion notifications
func (s *NMIWebhookService) handleTransactionSaleSuccess(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing NMI transaction success notification")

	body, err := s.parseTransactionEventBody()
	if err != nil {
		return err
	}

	provider := strings.TrimSpace(strings.ToLower(s.Processor))
	if provider == "" {
		provider = "mobius"
	}

	nmiSubID := transactionSubscriptionID(body)
	if nmiSubID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"event_type": s.Data.EventType,
		}).Warn("NMI transaction success missing subscription reference")
		return newNMIBillingError(ErrorTypeNMIValidation, "Missing subscription reference", map[string]interface{}{}, nil)
	}

	// The following field checked for does not
	/*actionSource := transactionActionSource(body)
	if !isRecurringSource(actionSource) {
		log.WithContext(ctx).
			WithFields(log.Fields{
				"subscription_reference": nmiSubID,
				"action_source":          actionSource,
				"event_type":             s.Data.EventType,
			}).Info("Ignoring NMI transaction success without recurring source")
		return nil
	}*/
	actionSource := ""

	subID, err := uuid.Parse(nmiSubID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_reference": nmiSubID,
			"provider":               provider,
		}).WithError(err).Error("Failed to parse subscription ID as UUID")
		return fmt.Errorf("failed to parse subscription ID '%s' as UUID: %w", nmiSubID, err)
	}

	subscription, err := s.SubscriptionService.GetByID(ctx, subID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_reference": nmiSubID,
			"provider":               provider,
		}).WithError(err).Error("Failed to load subscription for transaction event")
		return fmt.Errorf("failed to load subscription for transaction event: %w", err)
	}
	prevStatus := subscription.Status

	amount, amountErr := transactionAmount(body)
	currency := transactionCurrency(body)
	txnID := body.TransactionID.Trimmed()
	if txnID == "" && body.TransactionDetail != nil {
		txnID = body.TransactionDetail.TransactionID.Trimmed()
	}

	// Calculate amount in cents for Payment record
	var amountCents int64
	if amountErr != nil {
		log.WithContext(ctx).
			WithField("transaction_id", txnID).
			WithError(amountErr).
			Warn("Failed to parse NMI transaction amount; falling back to price amount")
		if subscription.Price != nil && subscription.Price.Amount > 0 {
			amountCents = subscription.Price.Amount
		}
	} else {
		amountCents = int64(amount * 100) // Convert dollars to cents
	}

	// Normalize currency
	currencyValue := strings.ToLower(strings.TrimSpace(currency))
	if currencyValue == "" {
		if subscription.Price != nil && strings.TrimSpace(subscription.Price.Currency) != "" {
			currencyValue = subscription.Price.Currency
		} else {
			currencyValue = "usd"
		}
	}

	processed := false

	// Activate or renew subscription based on current status
	switch subscription.Status {
	case models.StatusPending:
		if s.DB != nil {
			removed, err := removeCancelledSubscriptionsForActivation(ctx, s.DB, subscription.UserID, subscription.ProductID, subscription.ID)
			if err != nil {
				return fmt.Errorf("failed to cleanup cancelled subscriptions before activation: %w", err)
			}
			if removed > 0 {
				log.WithContext(ctx).WithFields(log.Fields{
					"user_id":     subscription.UserID,
					"product_id":  subscription.ProductID,
					"removed_cnt": removed,
				}).Info("Removed cancelled subscriptions before activation (NMI)")
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":             subscription.ID,
			"processor_subscription_id":   subscription.ProcessorSubscriptionID,
			"transaction_id":              txnID,
			"amount_cents":                amountCents,
			"currency":                    currencyValue,
			"action_source":               actionSource,
			"subscription_status":         subscription.Status,
			"subscription_lifecycle_step": "activate_pending_membership",
		}).Info("Activating pending subscription from NMI transaction success")

		_, err = s.SubscriptionLifecycleService.CreateMembership(ctx, &CreateMembershipParams{
			PriceID:                 subscription.PriceID,
			UserID:                  subscription.UserID,
			Processor:               models.ProcessorMobius,
			ProcessorSubscriptionID: &subscription.ProcessorSubscriptionID,
			UserEmail:               subscription.UserEmail,
			TransactionID:           txnID,
			Amount:                  amountCents,
			Currency:                currencyValue,
		})
		if err != nil {
			return fmt.Errorf("failed to activate subscription: %w", err)
		}

		if s.CreditsService != nil && s.SubscriptionService != nil {
			updated, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorMobius), provider, nmiSubID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Warn("failed to load subscription for initial credit grants (NMI)")
			} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
				if err := s.CreditsService.GrantSubscriptionCredits(ctx, GrantSubscriptionCreditsParams{
					SubscriptionID: updated.ID,
					PeriodEnd:      updated.CurrentPeriodEndsAt.UTC(),
					Cadence:        models.CreditGrantCadenceOnce,
					Source:         "subscription_initial",
				}); err != nil {
					log.WithContext(ctx).WithError(err).Warn("failed to grant initial subscription credits (NMI)")
				}
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":           subscription.ID,
			"processor_subscription_id": subscription.ProcessorSubscriptionID,
			"transaction_id":            txnID,
			"user_id":                   subscription.UserID,
		}).Info("Subscription activated via NMI transaction success")

		processed = true
	// case models.StatusActive:
	// Do nothing, subscription is already active
	default:
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":             subscription.ID,
			"processor_subscription_id":   subscription.ProcessorSubscriptionID,
			"transaction_id":              txnID,
			"amount_cents":                amountCents,
			"currency":                    currencyValue,
			"previous_status":             subscription.Status,
			"action_source":               actionSource,
			"subscription_lifecycle_step": "renew_membership",
		}).Info("Renewing subscription from NMI transaction success")

		// RenewMembership now creates the Payment record internally
		if err := s.SubscriptionLifecycleService.RenewMembership(ctx, &RenewMembershipParams{
			Processor:               models.ProcessorMobius,
			ProcessorSubscriptionID: nmiSubID,
			TransactionID:           txnID,
			Amount:                  amountCents,
			Currency:                currencyValue,
		}); err != nil {
			if IsTerminalTransitionBlocked(err) {
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"subscription_id":             subscription.ID,
					"processor_subscription_id":   subscription.ProcessorSubscriptionID,
					"transaction_id":              txnID,
					"subscription_lifecycle_step": "renew_membership",
				}).Warn("Blocked terminal -> active transition for delayed NMI success event")
				return nil
			}
			return fmt.Errorf("failed to renew subscription: %w", err)
		}

		if s.CreditsService != nil && s.SubscriptionService != nil {
			updated, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorMobius), provider, nmiSubID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Warn("failed to load subscription for renewal credit grants (NMI)")
			} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
				if err := s.CreditsService.GrantSubscriptionCredits(ctx, GrantSubscriptionCreditsParams{
					SubscriptionID: updated.ID,
					PeriodEnd:      updated.CurrentPeriodEndsAt.UTC(),
					Cadence:        models.CreditGrantCadencePerRenewal,
					Source:         "subscription_renewal",
				}); err != nil {
					log.WithContext(ctx).WithError(err).Warn("failed to grant renewal subscription credits (NMI)")
				}
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":           subscription.ID,
			"processor_subscription_id": subscription.ProcessorSubscriptionID,
			"transaction_id":            txnID,
			"user_id":                   subscription.UserID,
		}).Info("Subscription renewed via NMI transaction success")

		processed = true
	}

	if !processed {
		return nil
	}

	if s.EventLogService != nil {
		metadata := map[string]interface{}{
			"event_type":      s.Data.EventType,
			"action_source":   actionSource,
			"previous_status": string(prevStatus),
			"processor":       provider,
		}
		if txnID != "" {
			metadata["transaction_id"] = txnID
		}
		if planID := transactionPlanID(body); planID != "" {
			metadata["plan_id"] = planID
		}
		if body.CustomerID.Trimmed() != "" {
			metadata["customer_id"] = body.CustomerID.Trimmed()
		}
		if body.CustomerVaultID.Trimmed() != "" {
			metadata["customer_vault_id"] = body.CustomerVaultID.Trimmed()
		}
		if ord := body.OrderID.Trimmed(); ord != "" {
			metadata["order_id"] = ord
		}

		subEventType := PaymentEventBillingDateChanged
		switch prevStatus {
		case models.StatusPending:
			subEventType = PaymentEventSubscriptionCreated
		case models.StatusCancelled, models.StatusPastDue:
			subEventType = PaymentEventSubscriptionReactivated
		}
		var txnPtr *string
		if txnID != "" {
			txnPtr = &txnID
		}
		statusActive := models.StatusActive
		metadata["status_after"] = string(statusActive)

		s.logSubscriptionEvent(ctx, subscription, subEventType, txnPtr, metadata, &statusActive, nil)

		var amountPtr *float64
		if amountErr == nil {
			amountPtr = &amount
		}

		paymentEvent := PaymentEventData{
			EventID:        uuid.New(),
			SubscriptionID: &subscription.ID,
			UserID:         subscription.UserID,
			EventType:      PaymentEventChargeSuccess,
			Processor:      s.Processor,
			Amount:         amountPtr,
			Currency:       currency,
			BillingInfo:    CreateMetadataJSON(map[string]interface{}{}),
			WebhookSource:  "webhook",
			Metadata:       CreateMetadataJSON(metadata),
			Timestamp:      s.now().UTC(),
		}
		if txnID != "" {
			paymentEvent.ProcessorTransactionID = &txnID
		}
		if err := s.EventLogService.LogPaymentEvent(ctx, paymentEvent); err != nil {
			log.WithContext(ctx).WithError(err).Error("Failed to log NMI payment event")
		}
	}

	return nil
}

func (s *NMIWebhookService) handleTransactionSaleFailure(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Warn("Processing NMI transaction failure notification")

	body, err := s.parseTransactionEventBody()
	if err != nil {
		return err
	}

	provider := strings.TrimSpace(strings.ToLower(s.Processor))
	if provider == "" {
		provider = "mobius"
	}

	nmiSubID := transactionSubscriptionID(body)
	if nmiSubID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"event_type": s.Data.EventType,
		}).Warn("NMI transaction failure missing subscription reference")
		return newNMIBillingError(ErrorTypeNMIValidation, "Missing subscription reference", map[string]interface{}{}, nil)
	}

	subID, err := uuid.Parse(nmiSubID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_reference": nmiSubID,
			"provider":               provider,
		}).WithError(err).Error("Failed to parse subscription ID as UUID")
		return fmt.Errorf("failed to parse subscription ID '%s' as UUID: %w", nmiSubID, err)
	}

	//actionSource := "" //transactionActionSource(body)
	/*if !isRecurringSource(actionSource) {
		log.WithContext(ctx).
			WithFields(log.Fields{
				"subscription_reference": nmiSubID,
				"action_source":          actionSource,
				"event_type":             s.Data.EventType,
			}).Info("Ignoring NMI transaction failure without recurring source")
		return nil
	}*/

	var (
		subscription *models.Subscription
		fetchErr     error
	)

	if s.SubscriptionService != nil {
		subscription, fetchErr = s.SubscriptionService.GetByID(ctx, subID)
		if fetchErr != nil && !errors.Is(fetchErr, sql.ErrNoRows) {
			log.WithContext(ctx).WithFields(log.Fields{
				"processor_subscription_id": nmiSubID,
			}).WithError(fetchErr).Error("Failed to load subscription for transaction failure event")
			return fmt.Errorf("failed to load subscription for transaction event: %w", fetchErr)
		}
		if errors.Is(fetchErr, sql.ErrNoRows) {
			log.WithContext(ctx).
				WithField("processor_subscription_id", nmiSubID).
				Warn("Received NMI failure event for unknown subscription; ignoring")
			return nil
		}
	}
	var prevStatus models.SubscriptionStatus
	if subscription != nil {
		prevStatus = subscription.Status
	}

	txnID := body.TransactionID.Trimmed()
	if txnID == "" && body.TransactionDetail != nil {
		txnID = body.TransactionDetail.TransactionID.Trimmed()
	}

	var failureReason, failureCode *string
	if body.Action != nil {
		if txt := strings.TrimSpace(body.Action.ResponseText); txt != "" {
			failureReason = &txt
		}
		if code := strings.TrimSpace(body.Action.ResponseCode.String()); code != "" {
			failureCode = &code
		}
	} else if body.TransactionDetail != nil && body.TransactionDetail.Action != nil {
		if txt := strings.TrimSpace(body.TransactionDetail.Action.ResponseText); txt != "" {
			failureReason = &txt
		}
		if code := strings.TrimSpace(body.TransactionDetail.Action.ResponseCode.String()); code != "" {
			failureCode = &code
		}
	}

	logFields := log.Fields{
		"processor_subscription_id": nmiSubID,
	}
	if failureReason != nil && *failureReason != "" {
		logFields["failure_reason"] = *failureReason
	}
	if failureCode != nil && *failureCode != "" {
		logFields["failure_code"] = *failureCode
	}
	log.WithContext(ctx).WithFields(logFields).Warn("Marking subscription as failed due to NMI transaction failure")

	if s.PaymentService != nil && subscription != nil && txnID != "" {
		existingPayment, err := s.PaymentService.GetByTransactionID(ctx, models.Processor(s.Processor), txnID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("failed to fetch existing payment for transaction: %w", err)
		}

		if err == nil && existingPayment != nil {
			if err := s.PaymentService.MarkFailed(ctx, existingPayment.ID); err != nil {
				return fmt.Errorf("failed to mark payment as failed: %w", err)
			}
		} else {
			amount, amountErr := transactionAmount(body)
			currency := transactionCurrency(body)

			var amountCents int64
			if amountErr != nil {
				if subscription.Price != nil && subscription.Price.Amount > 0 {
					amountCents = subscription.Price.Amount
				}
			} else {
				amountCents = int64(amount * 100)
			}

			currencyValue := strings.ToLower(strings.TrimSpace(currency))
			if currencyValue == "" {
				if subscription.Price != nil && strings.TrimSpace(subscription.Price.Currency) != "" {
					currencyValue = subscription.Price.Currency
				} else {
					currencyValue = "usd"
				}
			}

			listAmount := amountCents
			if subscription.Price != nil && subscription.Price.Amount > 0 {
				listAmount = subscription.Price.Amount
			}

			now := s.now()
			payment := &models.Payment{
				ID:             uuid.New(),
				UserID:         subscription.UserID,
				PriceID:        subscription.PriceID,
				SubscriptionID: &subscription.ID,
				Processor:      models.Processor(s.Processor),
				TransactionID:  txnID,
				Amount:         amountCents,
				ListAmount:     listAmount,
				Currency:       currencyValue,
				PurchasedAt:    now,
				CreatedAt:      now,
			}
			if err := s.PaymentService.Create(ctx, payment); err != nil {
				return fmt.Errorf("failed to create payment for failure: %w", err)
			}
			if err := s.PaymentService.MarkFailed(ctx, payment.ID); err != nil {
				return fmt.Errorf("failed to mark new payment as failed: %w", err)
			}
		}
	}

	if subscription != nil {
		if err := s.SubscriptionLifecycleService.FailMembership(ctx, &FailMembershipParams{
			Processor:      models.Processor(s.Processor),
			SubscriptionID: &subscription.ID,
			FailureReason:  failureReason,
			FailureCode:    failureCode,
		}); err != nil {
			return fmt.Errorf("failed to mark subscription as failed: %w", err)
		}

		log.WithContext(ctx).WithField("processor_subscription_id", nmiSubID).Info("Subscription marked as failed for NMI transaction failure")
	}
	if s.EventLogService != nil && subscription != nil {
		if updated, err := s.SubscriptionService.GetByID(ctx, subID); err == nil && updated != nil {
			subscription = updated
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.WithContext(ctx).WithError(err).WithField("processor_subscription_id", nmiSubID).
				Warn("Failed to refresh subscription after failure flow; logging with stale status")
		}

		metadata := map[string]interface{}{
			"event_type":      s.Data.EventType,
			"previous_status": string(prevStatus),
			"processor":       provider,
		}
		if failureReason != nil && *failureReason != "" {
			metadata["failure_reason"] = *failureReason
		}
		if failureCode != nil && *failureCode != "" {
			metadata["failure_code"] = *failureCode
		}
		if txnID != "" {
			metadata["transaction_id"] = txnID
		}

		var amountPtr *float64
		if amt, amtErr := transactionAmount(body); amtErr == nil {
			amountPtr = &amt
		}

		currency := transactionCurrency(body)
		paymentEvent := PaymentEventData{
			EventID:        uuid.New(),
			SubscriptionID: &subscription.ID,
			UserID:         subscription.UserID,
			EventType:      PaymentEventChargeFailure,
			Processor:      s.Processor,
			Amount:         amountPtr,
			Currency:       currency,
			BillingInfo:    CreateMetadataJSON(map[string]interface{}{}),
			WebhookSource:  "webhook",
			Metadata:       CreateMetadataJSON(metadata),
			Timestamp:      s.now().UTC(),
		}
		if txnID != "" {
			paymentEvent.ProcessorTransactionID = &txnID
		}
		if err := s.EventLogService.LogPaymentEvent(ctx, paymentEvent); err != nil {
			log.WithContext(ctx).WithError(err).Error("Failed to log NMI payment failure event")
		}

		var txnPtr *string
		if txnID != "" {
			txnPtr = &txnID
			metadata["transaction_id"] = txnID
		}
		statusAfter := subscription.Status
		metadata["status_after"] = string(statusAfter)
		eventType := PaymentEventChargeFailure
		var cancelOverride *models.CancelType
		if statusAfter == models.StatusCancelled {
			eventType = PaymentEventSubscriptionExpired
			ct := models.CancelTypeExpired
			cancelOverride = &ct
		}
		s.logSubscriptionEvent(ctx, subscription, eventType, txnPtr, metadata, &statusAfter, cancelOverride)
	} else if subscription == nil {
		log.WithContext(ctx).
			WithField("processor_subscription_id", nmiSubID).
			Warn("Unable to log NMI payment failure event because subscription record was not found")
	}

	return nil
}

func (s *NMIWebhookService) handleACUEvent(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing NMI ACU notification")

	body, err := s.parseACUEventBody()
	if err != nil {
		return err
	}

	vaultID := body.VaultID.Trimmed()
	fields := log.Fields{"vault_id": vaultID}
	if body.Subscription != nil && !body.Subscription.SubscriptionID.IsEmpty() {
		fields["subscription_id"] = body.Subscription.SubscriptionID.Trimmed()
	}
	if body.PaymentMethod != nil {
		fields["card_last4"] = body.PaymentMethod.LastFour.Trimmed()
		fields["card_type"] = body.PaymentMethod.CardType.Trimmed()
		fields["expiry"] = body.PaymentMethod.ExpiryDate.Trimmed()
	}

	log.WithContext(ctx).WithFields(fields).Info("Received NMI ACU event (no automatic vault update configured)")
	return nil
}

func (s *NMIWebhookService) handleChargebackComplete(ctx context.Context) error {
	// NMI chargeback events are batch operations containing multiple disputes
	// IMPORTANT: NMI chargebacks do NOT include transaction_id or subscription info,
	// so automatic subscription termination is not possible. Manual intervention required.
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing NMI chargeback batch notification")

	body, err := s.parseChargebackBatchEventBody()
	if err != nil {
		// Fall back to basic logging if parsing fails
		log.WithContext(ctx).WithError(err).Warn("Failed to parse chargeback batch body; logging basic event")
		if s.EventLogService != nil {
			chargebackEventData := ChargebackEventData{
				EventID:   uuid.New(),
				EventType: PaymentEventBatchProcessed,
				Processor: s.Processor,
				BatchID:   s.Data.EventID,
				Status:    "completed",
				Metadata:  CreateMetadataJSON(map[string]interface{}{"parse_error": err.Error()}),
				Timestamp: s.now(),
			}
			if logErr := s.EventLogService.LogChargebackEvent(ctx, chargebackEventData); logErr != nil {
				log.WithError(logErr).Error("Failed to log chargeback batch event to ClickHouse")
			}
		}
		return nil
	}

	now := s.now()
	batchID := s.Data.EventID

	// Log batch-level summary first
	if s.EventLogService != nil {
		var totalAmount string
		var chargebackCount int
		if body.Batch != nil {
			totalAmount = body.Batch.TotalAmount
			chargebackCount = body.Batch.Count
		} else {
			chargebackCount = len(body.Chargebacks)
		}

		batchMetadata := map[string]interface{}{
			"batch_type":       "chargeback",
			"event_source":     s.Processor,
			"batch_status":     "completed",
			"chargeback_count": chargebackCount,
		}
		if totalAmount != "" {
			batchMetadata["total_amount"] = totalAmount
		}
		if body.Processor != nil {
			batchMetadata["processor_id"] = body.Processor.ID.Trimmed()
			batchMetadata["processor_name"] = body.Processor.Name.Trimmed()
		}

		chargebackEventData := ChargebackEventData{
			EventID:   uuid.New(),
			EventType: PaymentEventBatchProcessed,
			Processor: s.Processor,
			BatchID:   batchID,
			Status:    "completed",
			Metadata:  CreateMetadataJSON(batchMetadata),
			Timestamp: now,
		}

		if err := s.EventLogService.LogChargebackEvent(ctx, chargebackEventData); err != nil {
			log.WithError(err).Error("Failed to log chargeback batch event to ClickHouse")
		}

		// Log each individual chargeback for audit purposes
		for i, cb := range body.Chargebacks {
			cbMetadata := map[string]interface{}{
				"batch_id":      batchID,
				"chargeback_id": cb.ID.Trimmed(),
				"date":          cb.Date,
				"customer_name": cb.CustomerName,
				"cc_last4":      cb.CCNumber, // Already masked by NMI
				"reason_code":   cb.ReasonCode,
				"reason":        cb.Reason,
				"batch_index":   i,
				// Note: No transaction_id or subscription_id available from NMI
				"requires_manual_review": true,
			}

			// Parse amount if available
			var amountPtr *float64
			if cb.Amount != "" {
				var amt float64
				if _, parseErr := fmt.Sscanf(cb.Amount, "%f", &amt); parseErr == nil {
					amountPtr = &amt
				}
			}

			cbEventData := ChargebackEventData{
				EventID:   uuid.New(),
				EventType: PaymentEventChargeback,
				Processor: s.Processor,
				BatchID:   batchID,
				Status:    "received",
				Amount:    amountPtr,
				Metadata:  CreateMetadataJSON(cbMetadata),
				Timestamp: now,
			}

			if err := s.EventLogService.LogChargebackEvent(ctx, cbEventData); err != nil {
				log.WithContext(ctx).
					WithError(err).
					WithField("chargeback_id", cb.ID.Trimmed()).
					Error("Failed to log individual chargeback event to ClickHouse")
			}
		}
	}

	chargebackCount := len(body.Chargebacks)
	if body.Batch != nil {
		chargebackCount = body.Batch.Count
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"eventID":          s.Data.EventID,
		"eventType":        s.Data.EventType,
		"chargeback_count": chargebackCount,
	}).Warn("NMI chargeback batch processed - manual review required for subscription termination")

	return nil
}

// handleRefundSuccess processes NMI refund.success webhooks
// Matches CCBill logic: if refund >= 80% of subscription price, terminate subscription
func (s *NMIWebhookService) handleRefundSuccess(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing NMI refund notification")

	body, err := s.parseTransactionEventBody()
	if err != nil {
		return err
	}

	txnID := body.TransactionID.Trimmed()
	nmiSubID := transactionSubscriptionID(body)

	// Parse refund amount
	refundAmount, err := parseTransactionAmount(body)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("Failed to parse refund amount")
		refundAmount = 0
	}

	provider := strings.TrimSpace(strings.ToLower(s.Processor))
	if provider == "" {
		provider = "mobius"
	}

	// Try to find subscription - refund may be for a subscription payment
	var subscription *models.Subscription
	if nmiSubID != "" {
		subID, parseErr := uuid.Parse(nmiSubID)
		log.Println("NMI Refund - Parsed Subscription ID:", subID, "Error:", parseErr)
		if parseErr == nil {
			subscription, err = s.SubscriptionService.GetByID(ctx, subID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.WithContext(ctx).WithError(err).WithField("processor_subscription_id", nmiSubID).
					Warn("Failed to look up subscription for refund (by UUID)")
			} else if errors.Is(err, sql.ErrNoRows) {
				log.WithContext(ctx).WithField("subscription_id", nmiSubID).
					Warn("Received refund for unknown subscription (by UUID); continuing without lifecycle actions")
			}
		} else {
			subscription, err = s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, provider, nmiSubID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				log.WithContext(ctx).WithError(err).WithField("processor_subscription_id", nmiSubID).
					Warn("Failed to look up subscription for refund (by processor_subscription_id)")
			} else if errors.Is(err, sql.ErrNoRows) {
				log.WithContext(ctx).WithField("processor_subscription_id", nmiSubID).
					Warn("Received refund for unknown subscription (by processor_subscription_id); continuing without lifecycle actions")
			}
		}
	}

	// Determine if we should terminate subscription based on refund amount
	shouldTerminate := false
	if subscription != nil && subscription.Price != nil && subscription.Price.Amount > 0 {
		refundAmountCents := int64(math.Abs(refundAmount) * 100)
		refundPercentage := (refundAmountCents * 100) / subscription.Price.Amount
		if refundPercentage >= 80 {
			shouldTerminate = true
		}
	}

	now := s.now()

	if shouldTerminate && subscription != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":  subscription.ID,
			"refund_amount":    refundAmount,
			"subscription_fee": float64(subscription.Price.Amount) / 100,
		}).Warn("Terminating subscription due to significant refund (>=80%)")

		// Use lifecycle service to cancel membership with immediate revocation
		processor := models.Processor(s.Processor)
		cancelReason := "Refund processed"
		if subscription != nil {
			if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &CancelMembershipParams{
				Processor:               &processor,
				ProcessorSubscriptionID: &nmiSubID,
				SubscriptionID:          &subscription.ID,
				CancelType:              models.CancelTypeMerchant,
				CancelFeedback:          &cancelReason,
				RevokeAccess:            true,
			}); err != nil {
				log.WithContext(ctx).WithError(err).Error("Failed to cancel membership after refund")
			} else {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id":           subscription.ID,
					"processor_subscription_id": nmiSubID,
				}).Info("Subscription cancelled after refund meet threshold")
				if s.EventLogService != nil {
					statusCancelled := models.StatusCancelled
					cancelType := models.CancelTypeMerchant
					meta := map[string]interface{}{
						"event_type":           s.Data.EventType,
						"refund_amount":        refundAmount,
						"subscription_id":      subscription.ID.String(),
						"processor":            provider,
						"status_after":         string(statusCancelled),
						"previous_status":      string(subscription.Status),
						"cancelled_via_refund": shouldTerminate,
					}
					s.logSubscriptionEvent(ctx, subscription, PaymentEventSubscriptionCancelled, nil, meta, &statusCancelled, &cancelType)
				}
			}
		}
	}

	// Log refund event to ClickHouse
	if s.EventLogService != nil && subscription != nil {
		metadata := map[string]interface{}{
			"transaction_id":          txnID,
			"processor":               s.Processor,
			"event_source":            "webhook",
			"refund_amount":           refundAmount,
			"subscription_terminated": shouldTerminate,
		}

		negativeAmount := -refundAmount
		paymentEventData := PaymentEventData{
			EventID:        uuid.New(),
			SubscriptionID: &subscription.ID,
			UserID:         subscription.UserID,
			EventType:      PaymentEventRefund,
			Processor:      s.Processor,
			Amount:         &negativeAmount,
			Currency:       transactionCurrency(body),
			BillingInfo:    CreateMetadataJSON(map[string]interface{}{"refund": true}),
			WebhookSource:  "webhook",
			Metadata:       CreateMetadataJSON(metadata),
			Timestamp:      now.UTC(),
		}
		if txnID != "" {
			paymentEventData.ProcessorTransactionID = &txnID
		}

		if err := s.EventLogService.LogPaymentEvent(ctx, paymentEventData); err != nil {
			log.WithContext(ctx).WithError(err).Error("Failed to log NMI refund event")
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"transaction_id":          txnID,
		"refund_amount":           refundAmount,
		"subscription_terminated": shouldTerminate,
	}).Info("NMI refund processed")

	return nil
}

// handleRefundFailure logs failed refund attempts
func (s *NMIWebhookService) handleRefundFailure(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Warn("Processing NMI refund failure notification")

	body, err := s.parseTransactionEventBody()
	if err != nil {
		return err
	}

	txnID := body.TransactionID.Trimmed()

	// Log refund failure for audit purposes
	if s.EventLogService != nil {
		metadata := map[string]interface{}{
			"transaction_id": txnID,
			"processor":      s.Processor,
			"event_source":   "webhook",
			"failure":        true,
		}

		paymentEventData := PaymentEventData{
			EventID:       uuid.New(),
			EventType:     PaymentEventRefundFailure,
			Processor:     s.Processor,
			BillingInfo:   CreateMetadataJSON(map[string]interface{}{"refund_failure": true}),
			WebhookSource: "webhook",
			Metadata:      CreateMetadataJSON(metadata),
			Timestamp:     s.now().UTC(),
		}
		if txnID != "" {
			paymentEventData.ProcessorTransactionID = &txnID
		}

		if err := s.EventLogService.LogPaymentEvent(ctx, paymentEventData); err != nil {
			log.WithContext(ctx).WithError(err).Error("Failed to log NMI refund failure event")
		}
	}

	log.WithContext(ctx).WithField("transaction_id", txnID).Info("NMI refund failure logged")
	return nil
}

// handleVoidSuccess processes NMI void.success webhooks
func (s *NMIWebhookService) handleVoidSuccess(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing NMI void notification")

	body, err := s.parseTransactionEventBody()
	if err != nil {
		return err
	}

	txnID := body.TransactionID.Trimmed()
	nmiSubID := transactionSubscriptionID(body)

	provider := strings.TrimSpace(strings.ToLower(s.Processor))
	if provider == "" {
		provider = "mobius"
	}

	// Try to find subscription
	var subscription *models.Subscription
	if nmiSubID != "" {
		subscription, err = s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, provider, nmiSubID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.WithContext(ctx).WithError(err).WithField("processor_subscription_id", nmiSubID).
				Warn("Failed to look up subscription for void")
		}
	}

	// Log void event to ClickHouse
	if s.EventLogService != nil {
		metadata := map[string]interface{}{
			"transaction_id": txnID,
			"processor":      s.Processor,
			"event_source":   "webhook",
		}

		paymentEventData := PaymentEventData{
			EventID:       uuid.New(),
			EventType:     PaymentEventVoid,
			Processor:     s.Processor,
			BillingInfo:   CreateMetadataJSON(map[string]interface{}{"void": true}),
			WebhookSource: "webhook",
			Metadata:      CreateMetadataJSON(metadata),
			Timestamp:     s.now().UTC(),
		}
		if txnID != "" {
			paymentEventData.ProcessorTransactionID = &txnID
		}
		if subscription != nil {
			paymentEventData.SubscriptionID = &subscription.ID
			paymentEventData.UserID = subscription.UserID
		}

		if err := s.EventLogService.LogPaymentEvent(ctx, paymentEventData); err != nil {
			log.WithContext(ctx).WithError(err).Error("Failed to log NMI void event")
		}
	}

	log.WithContext(ctx).WithField("transaction_id", txnID).Info("NMI void processed")
	return nil
}

// handleVoidFailure logs failed void attempts
func (s *NMIWebhookService) handleVoidFailure(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Warn("Processing NMI void failure notification")

	body, err := s.parseTransactionEventBody()
	if err != nil {
		return err
	}

	txnID := body.TransactionID.Trimmed()

	// Log void failure for audit purposes
	if s.EventLogService != nil {
		metadata := map[string]interface{}{
			"transaction_id": txnID,
			"processor":      s.Processor,
			"event_source":   "webhook",
			"failure":        true,
		}

		paymentEventData := PaymentEventData{
			EventID:       uuid.New(),
			EventType:     PaymentEventVoidFailure,
			Processor:     s.Processor,
			BillingInfo:   CreateMetadataJSON(map[string]interface{}{"void_failure": true}),
			WebhookSource: "webhook",
			Metadata:      CreateMetadataJSON(metadata),
			Timestamp:     s.now().UTC(),
		}
		if txnID != "" {
			paymentEventData.ProcessorTransactionID = &txnID
		}

		if err := s.EventLogService.LogPaymentEvent(ctx, paymentEventData); err != nil {
			log.WithContext(ctx).WithError(err).Error("Failed to log NMI void failure event")
		}
	}

	log.WithContext(ctx).WithField("transaction_id", txnID).Info("NMI void failure logged")
	return nil
}
