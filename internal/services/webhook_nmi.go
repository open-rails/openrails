package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/open-rails/openrails/internal/db/models"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
)

const NMIProcessorName string = "NMI"

type NMIWebhookService struct {
	DB                           *db.DB
	Clock                        clockwork.Clock
	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	Data                         NMIWebhookEvent
	Processor                    string
	NMIClient                    *nmi.NMIClient
	EventLogService              *EventLogService
	SubscriptionService          *SubscriptionService
	PaymentService               *PaymentService
	CreditsService               *credits.CreditsService
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
	if body.TransactionDetail != nil && body.TransactionDetail.Subscription != nil && !body.TransactionDetail.Subscription.SubscriptionID.IsEmpty() {
		candidates = append(candidates, body.TransactionDetail.Subscription.SubscriptionID.Trimmed())
	}
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
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}

	return ""
}

func (s *NMIWebhookService) resolveSubscriptionFromReference(ctx context.Context, provider, reference string) (*models.Subscription, error) {
	if s.SubscriptionService == nil {
		return nil, fmt.Errorf("subscription service is required")
	}
	ref := strings.TrimSpace(reference)
	if ref == "" {
		return nil, sql.ErrNoRows
	}

	// Primary lookup: provider/external subscription ID as sent by NMI.
	subscription, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, ref)
	if err == nil {
		return subscription, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load subscription by processor subscription ID: %w", err)
	}

	// Fallback lookup: UUID if the reference is a local subscription ID.
	subID, parseErr := uuid.Parse(ref)
	if parseErr != nil {
		return nil, sql.ErrNoRows
	}
	return s.SubscriptionService.GetByID(ctx, subID)
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

func transactionAmountRaw(body *NMITransactionEventBody) (string, error) {
	if body == nil {
		return "", fmt.Errorf("transaction body is nil")
	}
	candidates := []string{body.Amount.Trimmed()}
	if body.TransactionDetail != nil {
		candidates = append(candidates, body.TransactionDetail.Amount.Trimmed())
	}
	if body.Action != nil {
		candidates = append(candidates, body.Action.Amount.Trimmed())
	}
	if body.TransactionDetail != nil && body.TransactionDetail.Action != nil {
		candidates = append(candidates, body.TransactionDetail.Action.Amount.Trimmed())
	}

	for _, value := range candidates {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}

	return "", fmt.Errorf("amount not provided")
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

func normalizeNMICurrencyValue(primary string, fallbacks ...string) string {
	allValues := append([]string{primary}, fallbacks...)
	for _, value := range allValues {
		normalized := strings.ToLower(strings.TrimSpace(value))
		if normalized != "" {
			return normalized
		}
	}
	return "usd"
}

func getOriginalTransactionID(body *NMITransactionEventBody) string {
	if body == nil || body.TransactionDetail == nil {
		return ""
	}
	return strings.TrimSpace(body.TransactionDetail.TransactionID.Trimmed())
}

func transactionAmountCents(body *NMITransactionEventBody) (int64, error) {
	raw, err := transactionAmountRaw(body)
	if err != nil {
		return 0, err
	}
	return moneyutil.ParseDecimalToCents(raw)
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

	subscription, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, nmiSubID)
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
		parsedAmountCents, amountErr := moneyutil.ParseDecimalToCents(body.Plan.Amount.Trimmed())
		if amountErr != nil {
			log.WithContext(ctx).
				WithError(amountErr).
				WithField("plan_amount", body.Plan.Amount.Trimmed()).
				Warn("Failed to parse plan amount; falling back to price amount")
		} else {
			amountCents = parsedAmountCents
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

	subscription, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, nmiSubID)
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

	subscription, err := s.resolveSubscriptionFromReference(ctx, provider, nmiSubID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_reference": nmiSubID,
			"provider":               provider,
		}).WithError(err).Error("Failed to load subscription for transaction event")
		return fmt.Errorf("failed to load subscription for transaction event: %w", err)
	}
	prevStatus := subscription.Status

	parsedAmountCents, amountErr := transactionAmountCents(body)
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
		amountCents = parsedAmountCents
	}

	fallbackCurrency := ""
	if subscription.Price != nil {
		fallbackCurrency = subscription.Price.Currency
	}
	currencyValue := normalizeNMICurrencyValue(currency, fallbackCurrency)

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
			updated, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorMobius), nmiSubID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Warn("failed to load subscription for initial credit grants (NMI)")
			} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
				if err := s.CreditsService.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
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
			updated, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorMobius), nmiSubID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Warn("failed to load subscription for renewal credit grants (NMI)")
			} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
				if err := s.CreditsService.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
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
			amountValue := moneyutil.CentsToMajorUnits(parsedAmountCents)
			amountPtr = &amountValue
		}

		paymentEvent := PaymentEventData{
			EventID:        uuid.New(),
			SubscriptionID: &subscription.ID,
			UserID:         subscription.UserID,
			EventType:      PaymentEventChargeSuccess,
			Processor:      s.Processor,
			Amount:         amountPtr,
			Currency:       currencyValue,
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
		subscription, fetchErr = s.resolveSubscriptionFromReference(ctx, provider, nmiSubID)
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

		if err == nil {
			if err := s.PaymentService.MarkFailed(ctx, existingPayment.ID); err != nil {
				return fmt.Errorf("failed to mark payment as failed: %w", err)
			}
		} else {
			parsedAmountCents, amountErr := transactionAmountCents(body)
			currency := transactionCurrency(body)

			var amountCents int64
			if amountErr != nil {
				if subscription.Price != nil && subscription.Price.Amount > 0 {
					amountCents = subscription.Price.Amount
				}
			} else {
				amountCents = parsedAmountCents
			}

			fallbackCurrency := ""
			if subscription.Price != nil {
				fallbackCurrency = subscription.Price.Currency
			}
			currencyValue := normalizeNMICurrencyValue(currency, fallbackCurrency)

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
		if updated, err := s.SubscriptionService.GetByID(ctx, subscription.ID); err == nil {
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
		if amtCents, amtErr := transactionAmountCents(body); amtErr == nil {
			amt := moneyutil.CentsToMajorUnits(amtCents)
			amountPtr = &amt
		}

		fallbackCurrency := ""
		if subscription.Price != nil {
			fallbackCurrency = subscription.Price.Currency
		}
		currency := normalizeNMICurrencyValue(transactionCurrency(body), fallbackCurrency)
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

type nmiChargebackMatch struct {
	PaymentID               uuid.UUID
	PaymentTransactionID    string
	SubscriptionID          uuid.UUID
	ProcessorSubscriptionID string
	UserID                  string
	AmountCents             int64
	Currency                string
	PurchasedAt             time.Time
	CardLast4               string
}

func normalizeNMIChargebackLast4(raw string) string {
	digits := strings.Builder{}
	for _, r := range raw {
		if unicode.IsDigit(r) {
			digits.WriteRune(r)
		}
	}
	value := digits.String()
	if len(value) < 4 {
		return ""
	}
	return value[len(value)-4:]
}

func parseNMIChargebackAmountCents(raw string) (int64, error) {
	amountCents, err := moneyutil.ParseDecimalToCents(raw)
	if err != nil {
		return 0, err
	}
	if amountCents <= 0 {
		return 0, fmt.Errorf("amount must be positive")
	}
	return amountCents, nil
}

func parseNMIChargebackDate(raw string) (time.Time, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, false
	}
	layouts := []string{
		"2006-01-02",
		"2006/01/02",
		"20060102",
		"1/2/2006",
		"01/02/2006",
		"1-2-2006",
		"01-02-2006",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05Z07:00",
		time.RFC3339,
	}
	if ts, err := timeutil.ParseFirstUTC(trimmed, layouts...); err == nil {
		return ts.UTC(), true
	}
	return time.Time{}, false
}

func splitNMIChargebackReason(rawReason, rawReasonCode string) (string, string) {
	reasonCode := strings.TrimSpace(rawReasonCode)
	reason := strings.TrimSpace(rawReason)

	if reasonCode == "" {
		if idx := strings.Index(reason, ":"); idx > 0 {
			candidate := strings.TrimSpace(reason[:idx])
			allDigits := candidate != ""
			for _, r := range candidate {
				if !unicode.IsDigit(r) {
					allDigits = false
					break
				}
			}
			if allDigits {
				reasonCode = candidate
				reason = strings.TrimSpace(reason[idx+1:])
			}
		}
	}

	return reasonCode, reason
}

func (s *NMIWebhookService) reconcileNMIChargebackEntry(ctx context.Context, processor string, cb NMIChargebackEntry) (*nmiChargebackMatch, map[string]interface{}, error) {
	meta := map[string]interface{}{
		"reconciliation_status": "unmatched",
	}
	if s == nil || s.DB == nil || s.DB.GetDB() == nil {
		meta["reconciliation_error"] = "database unavailable"
		return nil, meta, nil
	}

	last4 := normalizeNMIChargebackLast4(cb.CCNumber)
	if last4 != "" {
		meta["cc_last4_normalized"] = last4
	}

	var (
		amountCents int64
		amountErr   error
	)
	if amountCents, amountErr = parseNMIChargebackAmountCents(cb.Amount); amountErr == nil {
		meta["amount_cents"] = amountCents
	} else {
		meta["amount_parse_error"] = amountErr.Error()
	}

	targetTs, dateParsed := parseNMIChargebackDate(cb.Date)
	if dateParsed {
		meta["chargeback_date_parsed"] = targetTs.Format(time.RFC3339)
	} else {
		meta["chargeback_date_parse_error"] = cb.Date
	}

	query := s.DB.GetDB().
		NewSelect().
		TableExpr("billing.payments AS p").
		ColumnExpr("p.id AS payment_id").
		ColumnExpr("p.transaction_id AS payment_transaction_id").
		ColumnExpr("p.subscription_id AS subscription_id").
		ColumnExpr("sub.processor_subscription_id AS processor_subscription_id").
		ColumnExpr("p.user_id AS user_id").
		ColumnExpr("p.amount AS amount_cents").
		ColumnExpr("p.currency AS currency").
		ColumnExpr("p.purchased_at AS purchased_at").
		ColumnExpr("pm.last_four AS card_last4").
		Join("JOIN billing.subscriptions AS sub ON sub.id = p.subscription_id").
		Join("LEFT JOIN billing.payment_methods AS pm ON pm.id = sub.payment_method_id").
		Where("p.subscription_id IS NOT NULL").
		Where("p.processor = ?", models.Processor(processor)).
		Where("sub.processor = ?", models.Processor(processor)).
		Where("p.amount > 0").
		Limit(1)

	if amountErr == nil {
		query = query.Where("p.amount = ?", amountCents)
	}
	if last4 != "" {
		query = query.Where("RIGHT(regexp_replace(COALESCE(pm.last_four, ''), '[^0-9]', '', 'g'), 4) = ?", last4)
	}
	if dateParsed {
		query = query.OrderExpr("ABS(EXTRACT(EPOCH FROM (p.purchased_at - ?::timestamptz))) ASC", targetTs)
	}

	query = query.OrderExpr("p.purchased_at DESC")

	match := new(nmiChargebackMatch)
	if err := query.Scan(ctx, match); err != nil {
		if err == sql.ErrNoRows {
			return nil, meta, nil
		}
		return nil, meta, err
	}

	meta["reconciliation_status"] = "matched"
	meta["matched_payment_id"] = match.PaymentID.String()
	meta["matched_transaction_id"] = match.PaymentTransactionID
	meta["matched_subscription_id"] = match.SubscriptionID.String()
	meta["matched_processor_subscription_id"] = match.ProcessorSubscriptionID
	meta["matched_user_id"] = match.UserID
	meta["matched_payment_purchased_at"] = match.PurchasedAt.Format(time.RFC3339)
	meta["matched_amount_cents"] = match.AmountCents
	if strings.TrimSpace(match.CardLast4) != "" {
		meta["matched_card_last4"] = strings.TrimSpace(match.CardLast4)
	}

	return match, meta, nil
}

func (s *NMIWebhookService) handleChargebackComplete(ctx context.Context) error {
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
	processor := strings.TrimSpace(strings.ToLower(s.Processor))
	if processor == "" {
		processor = "mobius"
	}

	chargebackCount := len(body.Chargebacks)
	if body.Batch != nil && body.Batch.Count > 0 {
		chargebackCount = body.Batch.Count
	} else if body.Count > 0 {
		chargebackCount = body.Count
	}

	var (
		reconciledCount  int
		cancelledCount   int
		unmatchedCount   int
		reconcileErrors  int
		alreadyCancelled int
	)
	processedSubs := make(map[uuid.UUID]struct{})

	for i, cb := range body.Chargebacks {
		cbMetadata := map[string]interface{}{
			"batch_id":      batchID,
			"batch_index":   i,
			"chargeback_id": cb.ID.Trimmed(),
			"date":          cb.Date,
			"customer_name": cb.CustomerName,
			"cc_last4":      cb.CCNumber,
		}
		reasonCode, reasonText := splitNMIChargebackReason(cb.Reason, cb.ReasonCode)
		if reasonCode != "" {
			cbMetadata["reason_code"] = reasonCode
		}
		cbMetadata["reason"] = reasonText

		var amountPtr *float64
		if cbAmountCents, err := parseNMIChargebackAmountCents(cb.Amount); err == nil {
			cbAmount := moneyutil.CentsToMajorUnits(cbAmountCents)
			amountPtr = &cbAmount
		}

		match, reconcileMeta, reconcileErr := s.reconcileNMIChargebackEntry(ctx, processor, cb)
		if reconcileErr != nil {
			reconcileErrors++
			cbMetadata["reconciliation_status"] = "error"
			cbMetadata["reconciliation_error"] = reconcileErr.Error()
		} else {
			for key, value := range reconcileMeta {
				cbMetadata[key] = value
			}
		}

		cbStatus := "received"
		var (
			subscriptionID         *uuid.UUID
			userID                 *string
			processorTransactionID *string
		)

		if reconcileErr == nil && match == nil {
			unmatchedCount++
			cbMetadata["requires_manual_review"] = true
		}

		if reconcileErr == nil && match != nil {
			reconciledCount++
			cbMetadata["requires_manual_review"] = false
			subscriptionID = &match.SubscriptionID
			if match.UserID != "" {
				uid := match.UserID
				userID = &uid
			}
			if match.PaymentTransactionID != "" {
				txn := match.PaymentTransactionID
				processorTransactionID = &txn
			}

			if _, seen := processedSubs[match.SubscriptionID]; seen {
				cbMetadata["termination_status"] = "already_processed_in_batch"
			} else {
				processedSubs[match.SubscriptionID] = struct{}{}
				if s.SubscriptionLifecycleService == nil {
					reconcileErrors++
					cbMetadata["termination_status"] = "failed"
					cbMetadata["termination_error"] = "subscription lifecycle service unavailable"
				} else if s.SubscriptionService == nil {
					reconcileErrors++
					cbMetadata["termination_status"] = "failed"
					cbMetadata["termination_error"] = "subscription service unavailable"
				} else {

					subscription, subErr := s.SubscriptionService.GetByID(ctx, match.SubscriptionID)
					if subErr != nil {
						reconcileErrors++
						cbMetadata["termination_status"] = "failed"
						cbMetadata["termination_error"] = fmt.Sprintf("failed to load subscription: %v", subErr)
					} else {
						reasonCodeDisplay := reasonCode
						if reasonCodeDisplay == "" {
							reasonCodeDisplay = "unknown"
						}
						reasonDisplay := reasonText
						if reasonDisplay == "" {
							reasonDisplay = strings.TrimSpace(cb.Reason)
						}
						feedback := fmt.Sprintf(
							"CHARGEBACK: %s (Code: %s, Dispute: %s)",
							reasonDisplay,
							reasonCodeDisplay,
							strings.TrimSpace(cb.ID.Trimmed()),
						)
						proc := models.Processor(processor)
						subProcID := strings.TrimSpace(match.ProcessorSubscriptionID)
						params := &CancelMembershipParams{
							RevokeAccess:   true,
							Processor:      &proc,
							SubscriptionID: &match.SubscriptionID,
							CancelType:     models.CancelTypeChargeback,
							CancelFeedback: &feedback,
						}
						if subProcID != "" {
							params.ProcessorSubscriptionID = &subProcID
						}
						if err := s.SubscriptionLifecycleService.CancelMembership(ctx, params); err != nil {
							reconcileErrors++
							cbMetadata["termination_status"] = "failed"
							cbMetadata["termination_error"] = err.Error()
						} else {
							if subscription.Status == models.StatusCancelled {
								alreadyCancelled++
								cbMetadata["termination_status"] = "already_cancelled"
							} else {
								cancelledCount++
								cbMetadata["termination_status"] = "cancelled_immediate"
							}
							cbStatus = "completed"
						}
					}
				}
			}
		}

		if s.EventLogService != nil {
			cbEventData := ChargebackEventData{
				EventID:                uuid.New(),
				ChargebackID:           cb.ID.Trimmed(),
				BatchID:                batchID,
				SubscriptionID:         subscriptionID,
				UserID:                 userID,
				EventType:              PaymentEventChargeback,
				Processor:              s.Processor,
				ProcessorTransactionID: processorTransactionID,
				Amount:                 amountPtr,
				Currency:               "",
				ChargebackType:         "chargeback",
				Reason:                 reasonText,
				Status:                 cbStatus,
				Metadata:               CreateMetadataJSON(cbMetadata),
				Timestamp:              now,
			}
			if err := s.EventLogService.LogChargebackEvent(ctx, cbEventData); err != nil {
				log.WithContext(ctx).
					WithError(err).
					WithField("chargeback_id", cb.ID.Trimmed()).
					Error("Failed to log individual chargeback event to ClickHouse")
			}
		}
	}

	if s.EventLogService != nil {
		batchMetadata := map[string]interface{}{
			"batch_type":         "chargeback",
			"event_source":       s.Processor,
			"batch_status":       "completed",
			"chargeback_count":   chargebackCount,
			"reconciled_count":   reconciledCount,
			"cancelled_count":    cancelledCount,
			"already_cancelled":  alreadyCancelled,
			"unmatched_count":    unmatchedCount,
			"reconcile_failures": reconcileErrors,
		}
		if body.Batch != nil && strings.TrimSpace(body.Batch.TotalAmount) != "" {
			batchMetadata["total_amount"] = body.Batch.TotalAmount
		} else if strings.TrimSpace(body.ChargebackAmount) != "" {
			batchMetadata["total_amount"] = strings.TrimSpace(body.ChargebackAmount)
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
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"eventID":            s.Data.EventID,
		"eventType":          s.Data.EventType,
		"chargeback_count":   chargebackCount,
		"reconciled_count":   reconciledCount,
		"cancelled_count":    cancelledCount,
		"already_cancelled":  alreadyCancelled,
		"unmatched_count":    unmatchedCount,
		"reconcile_failures": reconcileErrors,
	}).Warn("NMI chargeback batch processed with automated reconciliation")

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
	originalTxnID := getOriginalTransactionID(body)

	// Parse refund amount exactly in cents (avoid float drift), then derive display float.
	refundAmountCents, err := transactionAmountCents(body)
	if err != nil {
		log.WithContext(ctx).WithError(err).Warn("Failed to parse refund amount")
		refundAmountCents = 0
	}
	if refundAmountCents < 0 {
		refundAmountCents = -refundAmountCents
	}
	refundAmount := moneyutil.CentsToMajorUnits(refundAmountCents)

	provider := strings.TrimSpace(strings.ToLower(s.Processor))
	if provider == "" {
		provider = "mobius"
	}

	// Try to find subscription - refund may be for a subscription payment
	var subscription *models.Subscription
	if nmiSubID != "" {
		subID, parseErr := uuid.Parse(nmiSubID)
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
			subscription, err = s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, nmiSubID)
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
		refundPercentage := (refundAmountCents * 100) / subscription.Price.Amount
		if refundPercentage >= 80 {
			shouldTerminate = true
		}
	}

	// Persist refund in the payments ledger as a negative payment linked to the original payment.
	// This complements analytics/event logging and keeps reconciliation/auditing consistent.
	if s.PaymentService != nil && subscription != nil && txnID != "" && refundAmountCents > 0 {
		processor := models.Processor(s.Processor)
		existingRefund, lookupErr := s.PaymentService.GetByTransactionID(ctx, processor, txnID)
		switch {
		case lookupErr == nil && existingRefund != nil:
			log.WithContext(ctx).WithFields(log.Fields{
				"refund_transaction_id": txnID,
				"payment_id":            existingRefund.ID,
			}).Info("Refund payment already exists; skipping duplicate ledger insert")
		case lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows):
			log.WithContext(ctx).WithError(lookupErr).WithField("refund_transaction_id", txnID).
				Warn("Failed to check existing refund payment by transaction ID")
		default:
			var originalPayment *models.Payment
			var originalLookupErr error

			if originalTxnID != "" && originalTxnID != txnID {
				originalPayment, originalLookupErr = s.PaymentService.GetByTransactionID(ctx, processor, originalTxnID)
				if originalLookupErr != nil && !errors.Is(originalLookupErr, sql.ErrNoRows) {
					log.WithContext(ctx).WithError(originalLookupErr).WithField("original_transaction_id", originalTxnID).
						Warn("Failed to resolve original payment by transaction ID for refund")
					originalPayment = nil
				}
			}

			if originalPayment == nil {
				originalPayment, originalLookupErr = s.PaymentService.GetLatestChargeBySubscriptionID(ctx, subscription.ID)
				if originalLookupErr != nil && !errors.Is(originalLookupErr, sql.ErrNoRows) {
					log.WithContext(ctx).WithError(originalLookupErr).WithField("subscription_id", subscription.ID).
						Warn("Failed to resolve original payment by subscription fallback for refund")
					originalPayment = nil
				}
			}

			if originalPayment == nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"refund_transaction_id": txnID,
					"subscription_id":       subscription.ID,
				}).Warn("Unable to resolve original payment for refund ledger linkage; skipping payment insert")
			} else {
				if _, refundErr := s.PaymentService.Refund(ctx, originalPayment.ID, txnID, refundAmountCents); refundErr != nil {
					log.WithContext(ctx).WithError(refundErr).WithFields(log.Fields{
						"refund_transaction_id":   txnID,
						"original_payment_id":     originalPayment.ID,
						"original_transaction_id": originalTxnID,
						"refund_amount_cents":     refundAmountCents,
					}).Warn("Failed to persist refund payment record")
				} else {
					log.WithContext(ctx).WithFields(log.Fields{
						"refund_transaction_id": txnID,
						"original_payment_id":   originalPayment.ID,
						"subscription_id":       subscription.ID,
						"refund_amount_cents":   refundAmountCents,
					}).Info("Persisted refund payment record")
				}
			}
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

		if s.SubscriptionLifecycleService != nil {
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
		fallbackCurrency := ""
		if subscription.Price != nil {
			fallbackCurrency = subscription.Price.Currency
		}
		currencyValue := normalizeNMICurrencyValue(transactionCurrency(body), fallbackCurrency)
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
			Currency:       currencyValue,
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
		subscription, err = s.SubscriptionService.GetByProcessorSubscriptionID(ctx, s.Processor, nmiSubID)
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
