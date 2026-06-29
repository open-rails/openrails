package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"
)

const NMIRailName string = "NMI"

type NMIWebhookService struct {
	DB                           *db.DB
	Clock                        clockwork.Clock
	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	Data                         NMIWebhookEvent
	Rail                         string
	NMIClient                    *nmi.NMIClient
	EventLogService              *analytics.EventLogService
	SubscriptionService          *subscriptions.SubscriptionService
	PaymentService               *payments.PaymentService
	MoneyService                 *money.MoneyService
	DeduplicationService         *DeduplicationService
	NotificationService          *subscriptions.NotificationService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *NMIWebhookService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

func (s *NMIWebhookService) normalizedRail() (string, error) {
	rail := strings.TrimSpace(strings.ToLower(s.Rail))
	if rail == "" {
		return "", errors.New("nmi webhook rail is required")
	}
	return rail, nil
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

func explicitTransactionSubscriptionID(body *NMITransactionEventBody) string {
	if body == nil {
		return ""
	}
	if body.Subscription != nil && !body.Subscription.SubscriptionID.IsEmpty() {
		return body.Subscription.SubscriptionID.Trimmed()
	}
	if body.TransactionDetail != nil && body.TransactionDetail.Subscription != nil && !body.TransactionDetail.Subscription.SubscriptionID.IsEmpty() {
		return body.TransactionDetail.Subscription.SubscriptionID.Trimmed()
	}
	return ""
}

func nmiAmountMatchesExpected(amountCents, expectedAmountMicros int64) bool {
	if expectedAmountMicros <= 0 {
		return true
	}
	expectedAmountCents, err := moneyutil.MicrosToCentsExact(expectedAmountMicros)
	if err != nil {
		return false
	}
	tolerance := int64(float64(expectedAmountCents) * 0.02)
	return amountCents >= expectedAmountCents-tolerance && amountCents <= expectedAmountCents+tolerance
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
	subscription, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, s.Rail, ref)
	if err == nil {
		return subscription, nil
	} else if !repo.IsNotFound(err) {
		return nil, fmt.Errorf("load subscription by rail subscription ID: %w", err)
	}

	// NMI transaction.sale.success may only include the original order/PO.
	// Resolve those references through local subscription metadata before granting access.
	subscription, err = s.SubscriptionService.GetByRailMetadataValue(ctx, provider, "order_id", ref)
	if err == nil {
		return subscription, nil
	}
	if !repo.IsNotFound(err) {
		return nil, fmt.Errorf("load subscription by NMI order metadata: %w", err)
	}

	if s.PaymentService != nil {
		attempt, lookupErr := s.PaymentService.GetByMetadataValue(ctx, "nmi_subscription_order_id", ref)
		if lookupErr != nil && !repo.IsNotFound(lookupErr) {
			return nil, fmt.Errorf("load NMI subscription attempt by order metadata: %w", lookupErr)
		}
		if lookupErr == nil && attempt != nil {
			providerSubID := strings.TrimSpace(fmt.Sprint(attempt.Metadata["provider_subscription_id"]))
			if providerSubID != "" {
				return s.SubscriptionService.GetByRailSubscriptionID(ctx, provider, providerSubID)
			}
		}
	}

	return nil, sql.ErrNoRows
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

func transactionAmountCandidates(body *NMITransactionEventBody) []string {
	if body == nil {
		return nil
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
	return candidates
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
	return ""
}

func getOriginalTransactionID(body *NMITransactionEventBody) string {
	if body == nil || body.TransactionDetail == nil {
		return ""
	}
	return strings.TrimSpace(body.TransactionDetail.TransactionID.Trimmed())
}

func transactionAmountCents(body *NMITransactionEventBody) (int64, error) {
	if body == nil {
		return 0, fmt.Errorf("transaction body is nil")
	}
	var firstErr error
	for _, raw := range transactionAmountCandidates(body) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		amount, err := moneyutil.ParseDecimalToCents(raw)
		if err == nil {
			return amount, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return 0, firstErr
	}
	return 0, fmt.Errorf("amount not provided")
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
	priceCurrency := ""
	var billingDays uint32
	var productID *uuid.UUID
	var priceID *uuid.UUID

	if sub != nil && sub.Price != nil {
		priceAmount = float64(sub.Price.Amount) / 100.0
		priceCurrency = sub.Price.Currency
		if sub.Price.BillingCycleDays != nil {
			billingDays, _ = safecast.Convert[uint32](*sub.Price.BillingCycleDays)
		}
		productID = &sub.Price.ProductID
		priceID = &sub.Price.ID
	}

	return priceAmount, priceCurrency, billingDays, productID, priceID
}

// logSubscriptionEvent emits a subscription event with full pricing/status context.
func (s *NMIWebhookService) logSubscriptionEvent(ctx context.Context, sub *models.Subscription, eventType analytics.PaymentEventType, railTransactionID *string, metadata map[string]interface{}, overrideStatus *models.SubscriptionStatus, overrideCancel *models.CancelType) {
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
	if sub.RailSubscriptionID != "" {
		procSubID = &sub.RailSubscriptionID
	}

	if metadata == nil {
		metadata = map[string]interface{}{}
	}

	event := analytics.SubscriptionEventData{
		EventID:            uuidutil.NewV7(),
		SubscriptionID:     sub.ID,
		UserID:             sub.CustomerID.String(),
		EventType:          eventType,
		Status:             string(status),
		CancelType:         cancelType,
		PriceAmount:        priceAmount,
		PriceCurrency:      priceCurrency,
		BillingCycleDays:   billingDays,
		ProductID:          productID,
		PriceID:            priceID,
		Rail:               s.Rail,
		RailSubscriptionID: procSubID,
		RailTransactionID:  railTransactionID,
		Metadata:           analytics.CreateMetadataJSON(metadata),
		Timestamp:          s.now().UTC(),
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
			models.Rail(s.Rail),
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
			"rail":       s.Rail,
			"event_type": s.Data.EventType,
		}).Warn("Unsupported NMI webhook event type")
		return MarkWebhookErrorNonRetryable(fmt.Errorf("unsupported event type: %s", s.Data.EventType))
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

	provider, err := s.normalizedRail()
	if err != nil {
		return err
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

	subscription, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, s.Rail, nmiSubID)
	if err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_reference": nmiSubID,
			"provider":               provider,
		}).WithError(err).Error("Failed to load subscription for rail ID")
		return fmt.Errorf("failed to load subscription for rail ID %s: %w", nmiSubID, err)
	}

	if subscription.Status != models.StatusPending {
		log.WithContext(ctx).
			WithField("subscription_id", subscription.ID).
			WithField("rail_subscription_id", nmiSubID).
			Info("Subscription already activated; skipping add event")
		return nil
	}

	transactionID, err := s.nmiSubscriptionAddTransactionID(ctx, subscription, nmiSubID)
	if err != nil {
		return err
	}
	if transactionID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":      subscription.ID,
			"rail_subscription_id": nmiSubID,
			"next_charge_date":     body.NextChargeDate.Trimmed(),
			"rail":                 provider,
		}).Info("NMI subscription add recorded without settled transaction metadata; waiting for transaction.sale.success before activation")
		return nil
	}
	if delayedStart := nmiFutureDelayedStart(subscription.Metadata, s.now().UTC()); delayedStart != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":      subscription.ID,
			"rail_subscription_id": nmiSubID,
			"delayed_start":        delayedStart.UTC().Format(time.RFC3339),
			"rail":                 provider,
		}).Info("NMI subscription add has settled transaction metadata but checkout delayed_start is in the future; keeping pending")
		return nil
	}

	var currentPeriodEndsAt *time.Time
	if nextChargeDate, ok := parseNMIDate(body.NextChargeDate.Trimmed()); ok && nextChargeDate.After(s.now()) {
		end := nextChargeDate.UTC()
		currentPeriodEndsAt = &end
	}

	if _, err := s.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
		PriceID:             subscription.PriceID,
		UserID:              subscription.CustomerID.String(),
		Rail:                models.Rail(provider),
		RailSubscriptionID:  &subscription.RailSubscriptionID,
		UserEmail:           subscription.UserEmail,
		CurrentPeriodEndsAt: currentPeriodEndsAt,
		TransactionID:       transactionID,
		Amount:              price.Amount,
		AmountProvided:      true,
		Currency:            price.Currency,
	}); err != nil {
		return fmt.Errorf("activate NMI subscription from add event: %w", err)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":      subscription.ID,
		"rail_subscription_id": nmiSubID,
		"rail":                 provider,
		"transaction_id":       transactionID,
		"next_charge_date":     body.NextChargeDate.Trimmed(),
	}).Info("NMI subscription activated from add event with settled transaction metadata")
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

	provider, err := s.normalizedRail()
	if err != nil {
		return err
	}

	nmiSubID := body.SubscriptionID.Trimmed()
	if nmiSubID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"event_type": s.Data.EventType,
		}).Warn("NMI subscription delete missing subscription ID")
		return newNMIBillingError(ErrorTypeNMIValidation, "Missing subscription ID", map[string]interface{}{}, nil)
	}

	subscription, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, s.Rail, nmiSubID)
	if err != nil {
		if repo.IsNotFound(err) {
			log.WithContext(ctx).
				WithField("rail_subscription_id", nmiSubID).
				Warn("Received NMI delete for unknown subscription; ignoring")
			return nil
		}
		log.WithContext(ctx).WithFields(log.Fields{
			"rail_subscription_id": nmiSubID,
		}).WithError(err).Error("Failed to load subscription for delete event")
		return fmt.Errorf("failed to get subscription: %w", err)
	}

	if subscription.Status == models.StatusCancelled {
		log.WithContext(ctx).
			WithField("subscription_id", subscription.ID).
			WithField("rail_subscription_id", nmiSubID).
			Info("Subscription already cancelled; skipping NMI delete event")
		return nil
	}

	cancelFeedback := "Cancelled via NMI webhook"
	rail := models.Rail(provider)
	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":      subscription.ID,
		"rail_subscription_id": nmiSubID,
		"user_id":              subscription.CustomerID.String(),
	}).Info("Cancelling subscription via NMI delete event")
	if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
		RevokeAccess:       false, // User keeps access until period end
		Rail:               &rail,
		RailSubscriptionID: &nmiSubID,
		CancelFeedback:     &cancelFeedback,
		SubscriptionID:     &subscription.ID,
		CancelType:         models.CancelTypeMerchant,
	}); err != nil {
		return fmt.Errorf("failed to cancel subscription: %w", err)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscription_id":      subscription.ID,
		"rail_subscription_id": nmiSubID,
	}).Info("Subscription cancelled via NMI delete event")

	if s.EventLogService != nil {
		cancelType := models.CancelTypeMerchant
		statusCancelled := models.StatusCancelled
		metadata := map[string]interface{}{
			"event_type":      s.Data.EventType,
			"rail":            provider,
			"previous_status": string(subscription.Status),
			"status_after":    string(statusCancelled),
		}
		s.logSubscriptionEvent(ctx, subscription, analytics.PaymentEventSubscriptionCancelled, nil, metadata, &statusCancelled, &cancelType)
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

	provider, err := s.normalizedRail()
	if err != nil {
		return err
	}

	explicitSubID := explicitTransactionSubscriptionID(body)
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
	txnID := body.TransactionID.Trimmed()
	if txnID == "" && body.TransactionDetail != nil {
		txnID = body.TransactionDetail.TransactionID.Trimmed()
	}
	if txnID == "" {
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Missing transaction ID", map[string]interface{}{
			"subscription_reference": nmiSubID,
		}, nil))
	}
	if explicitSubID == "" && subscription.Status != models.StatusPending {
		// The initial saved-card charge activates its subscription synchronously
		// (#330), so the sale.success webhook for that very charge arrives with
		// the subscription already active and no explicit subscription id. If
		// the transaction is already recorded, this is a duplicate notification
		// — acknowledge it instead of surfacing a webhook error.
		if s.PaymentService != nil {
			if existing, lookupErr := s.PaymentService.GetByTransactionID(ctx, models.Rail(provider), txnID); lookupErr == nil && existing != nil {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_reference": nmiSubID,
					"subscription_status":    string(subscription.Status),
					"transaction_id":         txnID,
					"payment_id":             existing.ID,
				}).Info("NMI transaction success already recorded; acknowledging duplicate notification")
				return nil
			}
		}
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Non-recurring transaction cannot mutate active subscription", map[string]interface{}{
			"subscription_reference": nmiSubID,
			"subscription_status":    string(subscription.Status),
			"transaction_id":         txnID,
		}, nil))
	}

	parsedAmountCents, amountErr := transactionAmountCents(body)
	if amountErr != nil {
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Invalid transaction amount", map[string]interface{}{
			"subscription_reference": nmiSubID,
		}, amountErr))
	}
	currency := transactionCurrency(body)
	if strings.TrimSpace(currency) == "" {
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Missing transaction currency", map[string]interface{}{
			"subscription_reference": nmiSubID,
		}, nil))
	}
	if subscription.Price != nil && !nmiAmountMatchesExpected(parsedAmountCents, subscription.Price.Amount) {
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Transaction amount does not match expected price", map[string]interface{}{
			"subscription_reference": nmiSubID,
			"transaction_amount":     parsedAmountCents,
			"expected_amount":        subscription.Price.Amount,
		}, nil))
	}

	// Provider payloads are cents; payment rows are micros.
	amountCents := parsedAmountCents
	amountMicros := moneyutil.CentsToMicros(parsedAmountCents)

	fallbackCurrency := ""
	if subscription.Price != nil {
		fallbackCurrency = subscription.Price.Currency
	}
	currencyValue := normalizeNMICurrencyValue(currency, fallbackCurrency)

	processed := false

	// Activate or renew subscription based on current status
	switch subscription.Status {
	case models.StatusPending:
		if delayedStart := nmiFutureDelayedStart(subscription.Metadata, s.now().UTC()); delayedStart != nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id":      subscription.ID,
				"rail_subscription_id": subscription.RailSubscriptionID,
				"transaction_id":       txnID,
				"delayed_start":        delayedStart.UTC().Format(time.RFC3339),
				"rail":                 provider,
			}).Info("NMI transaction success has checkout delayed_start in the future; keeping subscription pending")
			return nil
		}

		if s.DB != nil {
			removed, err := subscriptions.RemoveCancelledSubscriptionsForActivation(ctx, s.DB, subscription.CustomerID.String(), subscription.ProductID, subscription.ID)
			if err != nil {
				return fmt.Errorf("failed to cleanup cancelled subscriptions before activation: %w", err)
			}
			if removed > 0 {
				log.WithContext(ctx).WithFields(log.Fields{
					"user_id":     subscription.CustomerID.String(),
					"product_id":  subscription.ProductID,
					"removed_cnt": removed,
				}).Info("Removed cancelled subscriptions before activation (NMI)")
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":             subscription.ID,
			"rail_subscription_id":        subscription.RailSubscriptionID,
			"transaction_id":              txnID,
			"amount_cents":                amountCents,
			"currency":                    currencyValue,
			"action_source":               actionSource,
			"subscription_status":         subscription.Status,
			"subscription_lifecycle_step": "activate_pending_membership",
		}).Info("Activating pending subscription from NMI transaction success")

		_, err = s.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
			PriceID:            subscription.PriceID,
			UserID:             subscription.CustomerID.String(),
			Rail:               models.Rail(provider),
			RailSubscriptionID: &subscription.RailSubscriptionID,
			UserEmail:          subscription.UserEmail,
			TransactionID:      txnID,
			Amount:             amountMicros,
			AmountProvided:     true,
			Currency:           currencyValue,
		})
		if err != nil {
			return fmt.Errorf("failed to activate subscription: %w", err)
		}

		if s.MoneyService != nil && s.SubscriptionService != nil {
			updated, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, provider, nmiSubID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Warn("failed to load subscription for initial credit grants (NMI)")
			} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
				if err := s.MoneyService.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
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
			"subscription_id":      subscription.ID,
			"rail_subscription_id": subscription.RailSubscriptionID,
			"transaction_id":       txnID,
			"user_id":              subscription.CustomerID.String(),
		}).Info("Subscription activated via NMI transaction success")

		processed = true
	// case models.StatusActive:
	// Do nothing, subscription is already active
	default:
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":             subscription.ID,
			"rail_subscription_id":        subscription.RailSubscriptionID,
			"transaction_id":              txnID,
			"amount_cents":                amountCents,
			"currency":                    currencyValue,
			"previous_status":             subscription.Status,
			"action_source":               actionSource,
			"subscription_lifecycle_step": "renew_membership",
		}).Info("Renewing subscription from NMI transaction success")

		// RenewMembership now creates the Payment record internally
		if err := s.SubscriptionLifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
			Rail:               models.Rail(provider),
			RailSubscriptionID: nmiSubID,
			TransactionID:      txnID,
			Amount:             amountMicros,
			AmountProvided:     true,
			Currency:           currencyValue,
		}); err != nil {
			if subscriptions.IsTerminalTransitionBlocked(err) {
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"subscription_id":             subscription.ID,
					"rail_subscription_id":        subscription.RailSubscriptionID,
					"transaction_id":              txnID,
					"subscription_lifecycle_step": "renew_membership",
				}).Warn("Blocked terminal -> active transition for delayed NMI success event")
				return nil
			}
			return fmt.Errorf("failed to renew subscription: %w", err)
		}

		if s.MoneyService != nil && s.SubscriptionService != nil {
			updated, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, provider, nmiSubID)
			if err != nil {
				log.WithContext(ctx).WithError(err).Warn("failed to load subscription for renewal credit grants (NMI)")
			} else if updated.CurrentPeriodEndsAt != nil && !updated.CurrentPeriodEndsAt.IsZero() {
				if err := s.MoneyService.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
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
			"subscription_id":      subscription.ID,
			"rail_subscription_id": subscription.RailSubscriptionID,
			"transaction_id":       txnID,
			"user_id":              subscription.CustomerID.String(),
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
			"rail":            provider,
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

		subEventType := analytics.PaymentEventBillingDateChanged
		switch prevStatus {
		case models.StatusPending:
			subEventType = analytics.PaymentEventSubscriptionCreated
		case models.StatusCancelled, models.StatusPastDue:
			subEventType = analytics.PaymentEventSubscriptionReactivated
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

		paymentEvent := analytics.PaymentEventData{
			EventID:        uuidutil.NewV7(),
			SubscriptionID: &subscription.ID,
			UserID:         subscription.CustomerID.String(),
			EventType:      analytics.PaymentEventChargeSuccess,
			Rail:           s.Rail,
			Amount:         amountPtr,
			Currency:       currencyValue,
			BillingInfo:    analytics.CreateMetadataJSON(map[string]interface{}{}),
			WebhookSource:  "webhook",
			Metadata:       analytics.CreateMetadataJSON(metadata),
			Timestamp:      s.now().UTC(),
		}
		if txnID != "" {
			paymentEvent.RailTransactionID = &txnID
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

	provider, err := s.normalizedRail()
	if err != nil {
		return err
	}

	explicitSubID := explicitTransactionSubscriptionID(body)
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
		if fetchErr != nil && !repo.IsNotFound(fetchErr) {
			log.WithContext(ctx).WithFields(log.Fields{
				"rail_subscription_id": nmiSubID,
			}).WithError(fetchErr).Error("Failed to load subscription for transaction failure event")
			return fmt.Errorf("failed to load subscription for transaction event: %w", fetchErr)
		}
		if repo.IsNotFound(fetchErr) {
			log.WithContext(ctx).
				WithField("rail_subscription_id", nmiSubID).
				Warn("Received NMI failure event for unknown subscription; ignoring")
			return nil
		}
	}
	var prevStatus models.SubscriptionStatus
	if subscription != nil {
		prevStatus = subscription.Status
		if explicitSubID == "" && subscription.Status != models.StatusPending {
			return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Non-recurring transaction failure cannot mutate active subscription", map[string]interface{}{
				"subscription_reference": nmiSubID,
				"subscription_status":    string(subscription.Status),
			}, nil))
		}
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
		"rail_subscription_id": nmiSubID,
	}
	if failureReason != nil && *failureReason != "" {
		logFields["failure_reason"] = *failureReason
	}
	if failureCode != nil && *failureCode != "" {
		logFields["failure_code"] = *failureCode
	}
	log.WithContext(ctx).WithFields(logFields).Warn("Marking subscription as failed due to NMI transaction failure")

	if s.PaymentService != nil && subscription != nil && txnID != "" {
		existingPayment, err := s.PaymentService.GetByTransactionID(ctx, models.Rail(s.Rail), txnID)
		if err != nil && !repo.IsNotFound(err) {
			return fmt.Errorf("failed to fetch existing payment for transaction: %w", err)
		}

		if err == nil {
			if err := s.PaymentService.MarkFailed(ctx, existingPayment.ID); err != nil {
				return fmt.Errorf("failed to mark payment as failed: %w", err)
			}
		} else {
			parsedAmountCents, amountErr := transactionAmountCents(body)
			currency := transactionCurrency(body)

			var amountMicros int64
			if amountErr != nil {
				if subscription.Price != nil && subscription.Price.Amount > 0 {
					amountMicros = subscription.Price.Amount
				}
			} else {
				amountMicros = moneyutil.CentsToMicros(parsedAmountCents)
			}

			fallbackCurrency := ""
			if subscription.Price != nil {
				fallbackCurrency = subscription.Price.Currency
			}
			currencyValue := normalizeNMICurrencyValue(currency, fallbackCurrency)

			listAmount := amountMicros
			if subscription.Price != nil && subscription.Price.Amount > 0 {
				listAmount = subscription.Price.Amount
			}

			now := s.now()
			payment := &models.Payment{
				ID:             uuidutil.NewV7(),
				CustomerID:     subscription.CustomerID,
				PriceID:        subscription.PriceID,
				SubscriptionID: &subscription.ID,
				Rail:           models.Rail(s.Rail),
				TransactionID:  txnID,
				Amount:         amountMicros,
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
		if err := s.SubscriptionLifecycleService.FailMembership(ctx, &subscriptions.FailMembershipParams{
			Rail:           models.Rail(s.Rail),
			SubscriptionID: &subscription.ID,
			FailureReason:  failureReason,
			FailureCode:    failureCode,
		}); err != nil {
			return fmt.Errorf("failed to mark subscription as failed: %w", err)
		}

		log.WithContext(ctx).WithField("rail_subscription_id", nmiSubID).Info("Subscription marked as failed for NMI transaction failure")
	}
	if s.EventLogService != nil && subscription != nil {
		if updated, err := s.SubscriptionService.GetByID(ctx, subscription.ID); err == nil {
			subscription = updated
		} else if err != nil && !repo.IsNotFound(err) {
			log.WithContext(ctx).WithError(err).WithField("rail_subscription_id", nmiSubID).
				Warn("Failed to refresh subscription after failure flow; logging with stale status")
		}

		metadata := map[string]interface{}{
			"event_type":      s.Data.EventType,
			"previous_status": string(prevStatus),
			"rail":            provider,
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
		paymentEvent := analytics.PaymentEventData{
			EventID:        uuidutil.NewV7(),
			SubscriptionID: &subscription.ID,
			UserID:         subscription.CustomerID.String(),
			EventType:      analytics.PaymentEventChargeFailure,
			Rail:           s.Rail,
			Amount:         amountPtr,
			Currency:       currency,
			BillingInfo:    analytics.CreateMetadataJSON(map[string]interface{}{}),
			WebhookSource:  "webhook",
			Metadata:       analytics.CreateMetadataJSON(metadata),
			Timestamp:      s.now().UTC(),
		}
		if txnID != "" {
			paymentEvent.RailTransactionID = &txnID
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
		eventType := analytics.PaymentEventChargeFailure
		var cancelOverride *models.CancelType
		if statusAfter == models.StatusCancelled {
			eventType = analytics.PaymentEventSubscriptionExpired
			ct := models.CancelTypeExpired
			cancelOverride = &ct
		}
		s.logSubscriptionEvent(ctx, subscription, eventType, txnPtr, metadata, &statusAfter, cancelOverride)
	} else if subscription == nil {
		log.WithContext(ctx).
			WithField("rail_subscription_id", nmiSubID).
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
	PaymentID            uuid.UUID
	PaymentTransactionID string
	SubscriptionID       uuid.UUID
	RailSubscriptionID   string
	UserID               string
	AmountCents          int64
	Currency             string
	PurchasedAt          time.Time
	CardLast4            string
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
	return parseNMIDate(trimmed)
}

func parseNMIDate(raw string) (time.Time, bool) {
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

func (s *NMIWebhookService) nmiSubscriptionAddTransactionID(ctx context.Context, subscription *models.Subscription, nmiSubID string) (string, error) {
	if transactionID := nmiProviderTransactionIDFromMetadata(subscription.Metadata); transactionID != "" {
		return transactionID, nil
	}
	if s.PaymentService == nil {
		return "", nil
	}
	attempt, err := s.PaymentService.GetByMetadataValue(ctx, "provider_subscription_id", nmiSubID)
	if err != nil {
		if repo.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("lookup NMI subscription attempt metadata: %w", err)
	}
	if attempt == nil {
		return "", nil
	}
	return nmiProviderTransactionIDFromMap(attempt.Metadata), nil
}

func nmiProviderTransactionIDFromMetadata(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	metadata := map[string]any{}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return ""
	}
	return nmiProviderTransactionIDFromMap(metadata)
}

func nmiDelayedStartFromSubscriptionMetadata(raw json.RawMessage) *time.Time {
	if len(raw) == 0 {
		return nil
	}
	metadata := map[string]any{}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil
	}
	rawDelayedStart, ok := metadata["delayed_start"].(string)
	if !ok || strings.TrimSpace(rawDelayedStart) == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(rawDelayedStart))
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func nmiFutureDelayedStart(raw json.RawMessage, now time.Time) *time.Time {
	delayedStart := nmiDelayedStartFromSubscriptionMetadata(raw)
	if delayedStart == nil {
		return nil
	}
	if delayedStart.UTC().After(now.UTC()) {
		return delayedStart
	}
	return nil
}

func nmiProviderTransactionIDFromMap(metadata map[string]any) string {
	for _, key := range []string{"provider_transaction_id", "transaction_id"} {
		value, ok := metadata[key]
		if !ok {
			continue
		}
		transactionID, ok := value.(string)
		if !ok {
			continue
		}
		transactionID = strings.TrimSpace(transactionID)
		if isRealNMIProviderTransactionID(transactionID) {
			return transactionID
		}
	}
	return ""
}

func isRealNMIProviderTransactionID(transactionID string) bool {
	transactionID = strings.TrimSpace(transactionID)
	if transactionID == "" {
		return false
	}
	for _, prefix := range []string{"sub:", "nmi_sub_attempt:", "nmi_sale_attempt:"} {
		if strings.HasPrefix(transactionID, prefix) {
			return false
		}
	}
	return true
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

func (s *NMIWebhookService) reconcileNMIChargebackEntry(ctx context.Context, rail string, cb NMIChargebackEntry) (*nmiChargebackMatch, map[string]interface{}, error) {
	meta := map[string]interface{}{
		"reconciliation_status": "unmatched",
	}
	if s == nil || s.DB == nil {
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
	if amountErr != nil || last4 == "" || !dateParsed {
		meta["reconciliation_status"] = "insufficient_identifiers"
		return nil, meta, nil
	}

	rows, err := s.DB.Gen(ctx).MatchChargebackPayments(ctx, gen.MatchChargebackPaymentsParams{
		Rail:        gen.OpenrailsRailType(rail),
		AmountCents: amountCents,
		Last4:       last4,
		FromAt:      targetTs.Add(-7 * 24 * time.Hour),
		ToAt:        targetTs.Add(7 * 24 * time.Hour),
		TargetAt:    targetTs,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, meta, nil
		}
		return nil, meta, err
	}
	matches := make([]nmiChargebackMatch, 0, len(rows))
	for _, r := range rows {
		m := nmiChargebackMatch{
			PaymentID:            r.PaymentID,
			PaymentTransactionID: r.PaymentTransactionID,
			RailSubscriptionID:   r.RailSubscriptionID,
			UserID:               r.UserID,
			AmountCents:          r.AmountCents,
			Currency:             r.Currency,
			PurchasedAt:          r.PurchasedAt,
			CardLast4:            r.CardLast4,
		}
		if r.SubscriptionID != nil {
			m.SubscriptionID = *r.SubscriptionID
		}
		matches = append(matches, m)
	}
	if len(matches) == 0 {
		return nil, meta, nil
	}
	if len(matches) > 1 {
		meta["reconciliation_status"] = "ambiguous"
		meta["candidate_count"] = len(matches)
		return nil, meta, nil
	}
	match := &matches[0]

	meta["reconciliation_status"] = "matched"
	meta["matched_payment_id"] = match.PaymentID.String()
	meta["matched_transaction_id"] = match.PaymentTransactionID
	meta["matched_subscription_id"] = match.SubscriptionID.String()
	meta["matched_rail_subscription_id"] = match.RailSubscriptionID
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
			chargebackEventData := analytics.ChargebackEventData{
				EventID:   uuidutil.NewV7(),
				EventType: analytics.PaymentEventBatchProcessed,
				Rail:      s.Rail,
				BatchID:   s.Data.EventID,
				Status:    "completed",
				Metadata:  analytics.CreateMetadataJSON(map[string]interface{}{"parse_error": err.Error()}),
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
	rail, err := s.normalizedRail()
	if err != nil {
		return err
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
		ledgerErr        error
		ledgerAlertErr   error
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

		match, reconcileMeta, reconcileErr := s.reconcileNMIChargebackEntry(ctx, rail, cb)
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
			subscriptionID          *uuid.UUID
			userID                  *string
			railTransactionID       *string
			entryLedgerErr          error
			chargebackTransactionID string
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
				railTransactionID = &txn
			}
			if s.PaymentService == nil {
				reconcileErrors++
				cbMetadata["chargeback_payment_status"] = "failed"
				cbMetadata["chargeback_payment_error"] = "payment service unavailable"
				entryLedgerErr = fmt.Errorf("payment service unavailable for NMI chargeback ledger reversal")
				if ledgerErr == nil {
					ledgerErr = entryLedgerErr
					log.WithContext(ctx).WithError(ledgerErr).Error("Failed to record NMI chargeback reversal; continuing entitlement revocation")
				}
			} else {
				chargebackTransactionID = nmiChargebackTransactionID(cb.ID.Trimmed(), match.PaymentTransactionID)
				if existing, lookupErr := s.PaymentService.GetByTransactionID(ctx, models.Rail(rail), chargebackTransactionID); lookupErr == nil && existing != nil {
					cbMetadata["chargeback_payment_status"] = "already_recorded"
				} else if lookupErr != nil && !repo.IsNotFound(lookupErr) {
					reconcileErrors++
					cbMetadata["chargeback_payment_status"] = "failed"
					cbMetadata["chargeback_payment_error"] = lookupErr.Error()
					entryLedgerErr = fmt.Errorf("lookup NMI chargeback reversal: %w", lookupErr)
					if ledgerErr == nil {
						ledgerErr = entryLedgerErr
						log.WithContext(ctx).WithError(ledgerErr).Error("Failed to lookup NMI chargeback reversal; continuing entitlement revocation")
					}
				} else {
					amountCents := match.AmountCents
					if parsedAmount, parseErr := parseNMIChargebackAmountCents(cb.Amount); parseErr == nil && parsedAmount > 0 {
						amountCents = parsedAmount
					}
					if amountCents > match.AmountCents {
						amountCents = match.AmountCents
					}
					if _, refundErr := s.PaymentService.Refund(ctx, match.PaymentID, chargebackTransactionID, moneyutil.CentsToMicros(amountCents)); refundErr != nil {
						reconcileErrors++
						cbMetadata["chargeback_payment_status"] = "failed"
						cbMetadata["chargeback_payment_error"] = refundErr.Error()
						entryLedgerErr = fmt.Errorf("record NMI chargeback reversal: %w", refundErr)
						if ledgerErr == nil {
							ledgerErr = entryLedgerErr
							log.WithContext(ctx).WithError(ledgerErr).Error("Failed to record NMI chargeback reversal; continuing entitlement revocation")
						}
					} else {
						cbMetadata["chargeback_payment_status"] = "recorded"
						cbMetadata["chargeback_payment_transaction_id"] = chargebackTransactionID
					}
				}
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
						proc := models.Rail(rail)
						subProcID := strings.TrimSpace(match.RailSubscriptionID)
						params := &subscriptions.CancelMembershipParams{
							RevokeAccess:   true,
							Rail:           &proc,
							SubscriptionID: &match.SubscriptionID,
							CancelType:     models.CancelTypeChargeback,
							CancelFeedback: &feedback,
						}
						if subProcID != "" {
							params.RailSubscriptionID = &subProcID
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

			if entryLedgerErr != nil {
				paymentID := match.PaymentID
				subID := match.SubscriptionID
				alertTransactionID := chargebackTransactionID
				if strings.TrimSpace(alertTransactionID) == "" {
					alertTransactionID = nmiChargebackTransactionID(cb.ID.Trimmed(), match.PaymentTransactionID)
				}
				if err := recordLedgerRepairAlert(ctx, s.NotificationService, s.DB, s.now(), ledgerRepairAlert{
					Provider:          rail,
					Operation:         "chargeback_reversal",
					TransactionID:     alertTransactionID,
					UserID:            match.UserID,
					OriginalPaymentID: &paymentID,
					SubscriptionID:    &subID,
					Err:               entryLedgerErr,
					Metadata: map[string]any{
						"batch_id":                        batchID,
						"chargeback_id":                   cb.ID.Trimmed(),
						"rail_subscription_id":            match.RailSubscriptionID,
						"original_payment_transaction_id": match.PaymentTransactionID,
					},
				}); err != nil {
					reconcileErrors++
					cbMetadata["ledger_repair_alert_status"] = "failed"
					cbMetadata["ledger_repair_alert_error"] = err.Error()
					if ledgerAlertErr == nil {
						ledgerAlertErr = err
					}
					log.WithContext(ctx).WithError(err).Error("Failed to persist NMI chargeback ledger repair alert")
				} else {
					cbMetadata["ledger_repair_alert_status"] = "recorded"
				}
			}
		}

		if s.EventLogService != nil {
			cbEventData := analytics.ChargebackEventData{
				EventID:           uuidutil.NewV7(),
				ChargebackID:      cb.ID.Trimmed(),
				BatchID:           batchID,
				SubscriptionID:    subscriptionID,
				UserID:            userID,
				EventType:         analytics.PaymentEventChargeback,
				Rail:              s.Rail,
				RailTransactionID: railTransactionID,
				Amount:            amountPtr,
				Currency:          "",
				ChargebackType:    "chargeback",
				Reason:            reasonText,
				Status:            cbStatus,
				Metadata:          analytics.CreateMetadataJSON(cbMetadata),
				Timestamp:         now,
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
			"event_source":       s.Rail,
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
		if body.Rail != nil {
			batchMetadata["processor_id"] = body.Rail.ID.Trimmed()
			batchMetadata["rail_name"] = body.Rail.Name.Trimmed()
		}
		chargebackEventData := analytics.ChargebackEventData{
			EventID:   uuidutil.NewV7(),
			EventType: analytics.PaymentEventBatchProcessed,
			Rail:      s.Rail,
			BatchID:   batchID,
			Status:    "completed",
			Metadata:  analytics.CreateMetadataJSON(batchMetadata),
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
	if ledgerAlertErr != nil {
		return fmt.Errorf("persist NMI chargeback ledger repair alert: %w", ledgerAlertErr)
	}
	return nil
}

func nmiChargebackTransactionID(chargebackID, originalTransactionID string) string {
	chargebackID = strings.TrimSpace(chargebackID)
	if chargebackID != "" {
		return "chargeback:" + chargebackID
	}
	return "chargeback:" + strings.TrimSpace(originalTransactionID)
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

	provider, err := s.normalizedRail()
	if err != nil {
		return err
	}

	// Try to find subscription - refund may be for a subscription payment
	var subscription *models.Subscription
	if nmiSubID != "" {
		subscription, err = s.SubscriptionService.GetByRailSubscriptionID(ctx, s.Rail, nmiSubID)
		if err != nil && !repo.IsNotFound(err) {
			log.WithContext(ctx).WithError(err).WithField("rail_subscription_id", nmiSubID).
				Warn("Failed to look up subscription for refund (by rail_subscription_id)")
		} else if repo.IsNotFound(err) {
			log.WithContext(ctx).WithField("rail_subscription_id", nmiSubID).
				Warn("Received refund for unknown subscription (by rail_subscription_id); continuing without lifecycle actions")
		}
	}

	// Determine if we should terminate subscription based on refund amount.
	// Missing original references are not safe to complete silently because we cannot
	// link the refund to the ledger entry that funded entitlement.
	shouldTerminate := false
	if subscription != nil && subscription.Price != nil && subscription.Price.Amount > 0 && originalTxnID != "" {
		refundPercentage := (moneyutil.CentsToMicros(refundAmountCents) * 100) / subscription.Price.Amount
		if refundPercentage >= 80 {
			shouldTerminate = true
		}
	} else if subscription != nil && originalTxnID == "" {
		log.WithContext(ctx).WithFields(log.Fields{
			"refund_transaction_id": txnID,
			"subscription_ref":      nmiSubID,
		}).Warn("NMI refund missing original transaction ID; skipping lifecycle termination")
		return fmt.Errorf("unable to resolve original payment for NMI refund transaction %q", txnID)
	}

	// Persist refund in the payments ledger as a negative payment linked to the original payment.
	// This complements analytics/event logging and keeps reconciliation/auditing consistent.
	if s.PaymentService != nil && subscription != nil && txnID != "" && refundAmountCents > 0 {
		rail := models.Rail(s.Rail)
		existingRefund, lookupErr := s.PaymentService.GetByTransactionID(ctx, rail, txnID)
		switch {
		case lookupErr == nil && existingRefund != nil:
			log.WithContext(ctx).WithFields(log.Fields{
				"refund_transaction_id": txnID,
				"payment_id":            existingRefund.ID,
			}).Info("Refund payment already exists; skipping duplicate ledger insert")
		case lookupErr != nil && !repo.IsNotFound(lookupErr):
			log.WithContext(ctx).WithError(lookupErr).WithField("refund_transaction_id", txnID).
				Warn("Failed to check existing refund payment by transaction ID")
			return fmt.Errorf("check existing refund payment: %w", lookupErr)
		default:
			var originalPayment *models.Payment
			var originalLookupErr error

			if originalTxnID != "" && originalTxnID != txnID {
				originalPayment, originalLookupErr = s.PaymentService.GetByTransactionID(ctx, rail, originalTxnID)
				if originalLookupErr != nil && !repo.IsNotFound(originalLookupErr) {
					log.WithContext(ctx).WithError(originalLookupErr).WithField("original_transaction_id", originalTxnID).
						Warn("Failed to resolve original payment by transaction ID for refund")
					return fmt.Errorf("resolve original payment by transaction ID: %w", originalLookupErr)
				}
			}

			if originalPayment == nil {
				shouldTerminate = false
				log.WithContext(ctx).WithFields(log.Fields{
					"refund_transaction_id":   txnID,
					"subscription_id":         subscription.ID,
					"original_transaction_id": originalTxnID,
				}).Warn("Unable to resolve original payment for refund ledger linkage; skipping payment insert")
				return fmt.Errorf("unable to resolve original payment %q for NMI refund transaction %q", originalTxnID, txnID)
			} else {
				if _, refundErr := s.PaymentService.Refund(ctx, originalPayment.ID, txnID, moneyutil.CentsToMicros(refundAmountCents)); refundErr != nil {
					log.WithContext(ctx).WithError(refundErr).WithFields(log.Fields{
						"refund_transaction_id":   txnID,
						"original_payment_id":     originalPayment.ID,
						"original_transaction_id": originalTxnID,
						"refund_amount_cents":     refundAmountCents,
					}).Warn("Failed to persist refund payment record")
					return fmt.Errorf("persist refund payment record: %w", refundErr)
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
		rail := models.Rail(s.Rail)
		cancelReason := "Refund processed"

		if s.SubscriptionLifecycleService != nil {
			if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
				Rail:               &rail,
				RailSubscriptionID: &nmiSubID,
				SubscriptionID:     &subscription.ID,
				CancelType:         models.CancelTypeMerchant,
				CancelFeedback:     &cancelReason,
				RevokeAccess:       true,
			}); err != nil {
				log.WithContext(ctx).WithError(err).Error("Failed to cancel membership after refund")
			} else {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id":      subscription.ID,
					"rail_subscription_id": nmiSubID,
				}).Info("Subscription cancelled after refund meet threshold")
				if s.EventLogService != nil {
					statusCancelled := models.StatusCancelled
					cancelType := models.CancelTypeMerchant
					meta := map[string]interface{}{
						"event_type":           s.Data.EventType,
						"refund_amount":        refundAmount,
						"subscription_id":      subscription.ID.String(),
						"rail":                 provider,
						"status_after":         string(statusCancelled),
						"previous_status":      string(subscription.Status),
						"cancelled_via_refund": shouldTerminate,
					}
					s.logSubscriptionEvent(ctx, subscription, analytics.PaymentEventSubscriptionCancelled, nil, meta, &statusCancelled, &cancelType)
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
			"rail":                    s.Rail,
			"event_source":            "webhook",
			"refund_amount":           refundAmount,
			"subscription_terminated": shouldTerminate,
		}

		negativeAmount := -refundAmount
		paymentEventData := analytics.PaymentEventData{
			EventID:        uuidutil.NewV7(),
			SubscriptionID: &subscription.ID,
			UserID:         subscription.CustomerID.String(),
			EventType:      analytics.PaymentEventRefund,
			Rail:           s.Rail,
			Amount:         &negativeAmount,
			Currency:       currencyValue,
			BillingInfo:    analytics.CreateMetadataJSON(map[string]interface{}{"refund": true}),
			WebhookSource:  "webhook",
			Metadata:       analytics.CreateMetadataJSON(metadata),
			Timestamp:      now.UTC(),
		}
		if txnID != "" {
			paymentEventData.RailTransactionID = &txnID
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
			"rail":           s.Rail,
			"event_source":   "webhook",
			"failure":        true,
		}

		paymentEventData := analytics.PaymentEventData{
			EventID:       uuidutil.NewV7(),
			EventType:     analytics.PaymentEventRefundFailure,
			Rail:          s.Rail,
			BillingInfo:   analytics.CreateMetadataJSON(map[string]interface{}{"refund_failure": true}),
			WebhookSource: "webhook",
			Metadata:      analytics.CreateMetadataJSON(metadata),
			Timestamp:     s.now().UTC(),
		}
		if txnID != "" {
			paymentEventData.RailTransactionID = &txnID
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

	provider, err := s.normalizedRail()
	if err != nil {
		return err
	}

	// Try to find subscription
	var subscription *models.Subscription
	if nmiSubID != "" {
		subscription, err = s.SubscriptionService.GetByRailSubscriptionID(ctx, provider, nmiSubID)
		if err != nil && !repo.IsNotFound(err) {
			log.WithContext(ctx).WithError(err).WithField("rail_subscription_id", nmiSubID).
				Warn("Failed to look up subscription for void")
		}
	}
	var voidedSubscriptionID *uuid.UUID
	if s.PaymentService != nil && txnID != "" {
		originalPayment, paymentErr := s.PaymentService.GetByTransactionID(ctx, models.Rail(s.Rail), txnID)
		if paymentErr != nil && !repo.IsNotFound(paymentErr) {
			return fmt.Errorf("lookup original payment for void: %w", paymentErr)
		}
		if originalPayment != nil {
			reversalID := "void:" + txnID
			if existingVoid, lookupErr := s.PaymentService.GetByTransactionID(ctx, models.Rail(s.Rail), reversalID); lookupErr == nil && existingVoid != nil {
				log.WithContext(ctx).WithField("void_transaction_id", reversalID).Info("NMI void reversal already recorded")
			} else if lookupErr != nil && !repo.IsNotFound(lookupErr) {
				return fmt.Errorf("lookup existing void reversal: %w", lookupErr)
			} else {
				amount := originalPayment.Amount
				if amount <= 0 {
					amount = originalPayment.ListAmount
				}
				if amount <= 0 {
					return MarkWebhookErrorNonRetryable(fmt.Errorf("cannot void payment with non-positive amount"))
				}
				if _, refundErr := s.PaymentService.Refund(ctx, originalPayment.ID, reversalID, amount); refundErr != nil {
					return fmt.Errorf("record void reversal: %w", refundErr)
				}
			}
			if originalPayment.SubscriptionID != nil {
				id := *originalPayment.SubscriptionID
				voidedSubscriptionID = &id
			}
		} else {
			log.WithContext(ctx).WithField("transaction_id", txnID).Warn("Unable to resolve original payment for NMI void")
			if subscription != nil {
				return fmt.Errorf("unable to resolve original payment for NMI void transaction %q", txnID)
			}
		}
	}
	if voidedSubscriptionID != nil && s.SubscriptionLifecycleService != nil {
		rail := models.Rail(s.Rail)
		reason := "NMI void processed"
		if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: voidedSubscriptionID,
			Rail:           &rail,
			CancelType:     models.CancelTypeMerchant,
			CancelFeedback: &reason,
			RevokeAccess:   true,
		}); err != nil {
			return fmt.Errorf("cancel membership after NMI void: %w", err)
		}
	}

	// Log void event to ClickHouse
	if s.EventLogService != nil {
		metadata := map[string]interface{}{
			"transaction_id": txnID,
			"rail":           provider,
			"event_source":   "webhook",
		}

		paymentEventData := analytics.PaymentEventData{
			EventID:       uuidutil.NewV7(),
			EventType:     analytics.PaymentEventVoid,
			Rail:          provider,
			BillingInfo:   analytics.CreateMetadataJSON(map[string]interface{}{"void": true}),
			WebhookSource: "webhook",
			Metadata:      analytics.CreateMetadataJSON(metadata),
			Timestamp:     s.now().UTC(),
		}
		if txnID != "" {
			paymentEventData.RailTransactionID = &txnID
		}
		if subscription != nil {
			paymentEventData.SubscriptionID = &subscription.ID
			paymentEventData.UserID = subscription.CustomerID.String()
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
			"rail":           s.Rail,
			"event_source":   "webhook",
			"failure":        true,
		}

		paymentEventData := analytics.PaymentEventData{
			EventID:       uuidutil.NewV7(),
			EventType:     analytics.PaymentEventVoidFailure,
			Rail:          s.Rail,
			BillingInfo:   analytics.CreateMetadataJSON(map[string]interface{}{"void_failure": true}),
			WebhookSource: "webhook",
			Metadata:      analytics.CreateMetadataJSON(metadata),
			Timestamp:     s.now().UTC(),
		}
		if txnID != "" {
			paymentEventData.RailTransactionID = &txnID
		}

		if err := s.EventLogService.LogPaymentEvent(ctx, paymentEventData); err != nil {
			log.WithContext(ctx).WithError(err).Error("Failed to log NMI void failure event")
		}
	}

	log.WithContext(ctx).WithField("transaction_id", txnID).Info("NMI void failure logged")
	return nil
}
