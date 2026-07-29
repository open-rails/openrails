package webhooks

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	identitydir "github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/identity"
	log "github.com/sirupsen/logrus"
)

type CCBillWebhookService struct {
	Data                         CCBillWebhookEvent
	DB                           *db.DB
	Clock                        clockwork.Clock
	CCBillClient                 *ccbill.RESTClient
	ProductService               *catalog.ProductService
	PriceService                 *catalog.PriceService
	NotificationService          *subscriptions.NotificationService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	ProfileRepo                  *identitydir.ProfileRepo
	PaymentService               *payments.PaymentService
	DeduplicationService         *DeduplicationService
	CheckoutSessionService       webhookCheckoutSessionStore
	MoneyService                 *money.MoneyService
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *CCBillWebhookService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

/*
func parseCCBillTimestamp(ts string) (time.Time, error) {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return time.Time{}, fmt.Errorf("timestamp is empty")
	}
	// CCBill webhooks use "YYYY-MM-DD HH:MM:SS" without timezone.
	// Treat as UTC for deterministic behavior.
	return time.ParseInLocation("2006-01-02 15:04:05", ts, time.UTC)
}*/

func parseCCBillDate(dateStr string) (time.Time, error) {
	return timeutil.ParseDateUTC(dateStr)
}

// parseCCBillDateUsingTimestamp parses date-only fields (e.g., nextRenewalDate/nextRetryDate).
//
// CCBill sends these as YYYY-MM-DD with no time-of-day. To avoid accidental access gaps due to
// ambiguity, we interpret the date as the end of that UTC day (23:59:59Z).
func parseCCBillDateUsingTimestamp(dateStr string) (*time.Time, error) {
	if strings.TrimSpace(dateStr) == "" {
		return nil, nil
	}
	d, err := parseCCBillDate(dateStr)
	if err != nil {
		return nil, err
	}
	combined := time.Date(d.Year(), d.Month(), d.Day(), 23, 59, 59, 0, time.UTC)
	return &combined, nil
}

// ccbillGraceCap bounds how far past the paid term end a CCBill-announced
// retry date may push the grace_ends_at PACING marker (when the stalled row
// parks to `unknown`). Since #691 access rides the standing entitlement window
// — grace_ends_at carries no access role and no grace windows are appended.
const ccbillGraceCap = 72 * time.Hour

func capCCBillRetryAt(nextRetryAt, paidTermEnd *time.Time) *time.Time {
	if nextRetryAt == nil {
		return nil
	}
	candidate := nextRetryAt.UTC()
	if paidTermEnd != nil {
		maxGraceEnd := paidTermEnd.UTC().Add(ccbillGraceCap)
		if candidate.After(maxGraceEnd) {
			candidate = maxGraceEnd
		}
	}
	return &candidate
}

func shouldIgnoreCCBillRenewalFailure(sub *models.Subscription, failureRenewalAt *time.Time) (bool, string) {
	if sub == nil {
		return false, ""
	}
	if sub.Status == models.StatusCancelled {
		return true, "cancelled_subscription"
	}
	if sub.Status == models.StatusActive && failureRenewalAt != nil && sub.CurrentPeriodStartsAt != nil && !failureRenewalAt.After(sub.CurrentPeriodStartsAt.UTC()) {
		return true, "stale_renewal_failure"
	}
	return false, ""
}

func parseCCBillPositiveAmountCents(rawAmount, parseFieldName, invalidFieldName string) (moneyutil.Cents, error) {
	return parseCCBillAmountCents(rawAmount, parseFieldName, invalidFieldName, false)
}

func parseCCBillAmountCents(rawAmount, parseFieldName, invalidFieldName string, allowZero bool) (moneyutil.Cents, error) {
	amountCents, err := moneyutil.ParseDecimalToCents(rawAmount)
	if err != nil {
		return 0, fmt.Errorf("failed to parse %s '%s': %w", parseFieldName, rawAmount, err)
	}

	if amountCents < 0 || (!allowZero && amountCents == 0) {
		return 0, fmt.Errorf("invalid %s: %d cents - must be greater than 0", invalidFieldName, amountCents)
	}

	return amountCents, nil
}

func ccbillInitialChargeAmount(price *models.Price) moneyutil.Micros {
	if price == nil {
		return 0
	}
	if initialAmount, _, ok := price.GetTrial(); ok {
		return moneyutil.Micros(initialAmount)
	}
	return moneyutil.Micros(price.Amount)
}

func validateCCBillBilledAmount(ctx context.Context, svc *CCBillWebhookService, currency string, billedAmountCents moneyutil.Cents, expectedAmountMicros moneyutil.Micros, contextFields map[string]interface{}, logFields log.Fields) error {
	expectedAmountCents, err := moneyutil.NativeToRailMinorExact(currency, int64(expectedAmountMicros))
	if err != nil {
		return err
	}
	tolerance := expectedAmountCents * 2 / 100 // 2% tolerance, integer-only (#818)
	if billedAmountCents >= expectedAmountCents-tolerance && billedAmountCents <= expectedAmountCents+tolerance {
		return nil
	}

	billingErr := newBillingError(ErrorTypeAmount,
		"Billed amount does not match expected price",
		map[string]interface{}{
			"expected_amount_cents": expectedAmountCents,
			"billed_amount_cents":   billedAmountCents,
			"tolerance_cents":       tolerance,
		}, nil)
	for key, value := range contextFields {
		billingErr.Context[key] = value
	}
	if svc != nil {
		svc.logBillingError(ctx, billingErr, logFields)
		// #675: amount mismatches ACK as non-retryable — a >2% drifted REAL
		// charge needs a durable operator trace, not just a log line.
		txnID, _ := billingErr.Context["transaction_id"].(string)
		if txnID == "" {
			txnID, _ = logFields["transaction_id"].(string)
		}
		if alertErr := recordLedgerRepairAlert(ctx, svc.NotificationService, svc.DB, svc.now(), ledgerRepairAlert{
			Provider:      string(models.RailCCBill),
			Operation:     "amount_mismatch",
			TransactionID: txnID,
			Err:           billingErr,
			Metadata:      billingErr.Context,
		}); alertErr != nil {
			return fmt.Errorf("record CCBill amount mismatch repair alert: %w", alertErr)
		}
	}
	return billingErr
}

// requireCCBillCurrency is the CCBill currency INGESTION boundary: whatever it
// returns is written to a payments row, so it must return the canonical UPPER
// form (CUR-6). CCBill sends the ISO-4217 NUMERIC code, which maps back through
// the same table the outbound FlexForm picks from; an alpha code arrives
// upper-cased rather than verbatim.
func requireCCBillCurrency(currencyCode Stringish, fieldName string) (string, error) {
	normalized := strings.ToUpper(currencyCode.Trimmed())
	if normalized == "" {
		return "", newBillingError(
			ErrorTypeValidation,
			"missing required currency code",
			map[string]interface{}{"field": fieldName},
			nil,
		)
	}

	// #819: same table the outbound FlexForm picks currencyCode from, read in
	// reverse — every currency we can bill maps back, so a real charge can never
	// be rejected here as a "mismatch" against the price it was billed for.
	if currency, ok := ccbill.CurrencyFromCode(currencyCode.Trimmed()); ok {
		return currency, nil
	}
	return normalized, nil
}

func validateCCBillCurrencyMatches(actualCurrency, expectedCurrency string, contextFields map[string]interface{}) error {
	actual := money.NormalizeCurrency(actualCurrency)
	expected := money.NormalizeCurrency(expectedCurrency)
	if expected == "" || actual == expected {
		return nil
	}
	billingErr := newBillingError(ErrorTypeValidation, "Billed currency does not match expected price", map[string]interface{}{
		"billed_currency":   actual,
		"expected_currency": expected,
	}, nil)
	for key, value := range contextFields {
		billingErr.Context[key] = value
	}
	return billingErr
}

func ccbillPriceLookupID(rboID, flexID string) string {
	if id := strings.TrimSpace(rboID); id != "" {
		return id
	}
	return strings.TrimSpace(flexID)
}

func (s *CCBillWebhookService) ensureFlexFormMatches(price *models.Price, flexID, formName string) error {
	expectedFormName, expectedFlexID, ok := price.GetCCBillFlexForm()
	if !ok {
		return fmt.Errorf("price %s is missing CCBill flexform configuration", price.ID)
	}
	if strings.TrimSpace(flexID) != expectedFlexID {
		return fmt.Errorf("payment form id mismatch: got %s, want %s", flexID, expectedFlexID)
	}
	if strings.TrimSpace(formName) != expectedFormName {
		return fmt.Errorf("payment form name mismatch: got %s, want %s", formName, expectedFormName)
	}
	return nil
}

func (s *CCBillWebhookService) ensureCCBillPriceMatches(price *models.Price, flexID, formName, rboID string) error {
	if expectedRBO, ok := price.GetCCBillRecurringBillingOption(); ok && strings.TrimSpace(rboID) != "" {
		if strings.TrimSpace(rboID) != expectedRBO {
			return fmt.Errorf("recurring billing option mismatch: got %s, want %s", rboID, expectedRBO)
		}
		return nil
	}
	return s.ensureFlexFormMatches(price, flexID, formName)
}

func (s *CCBillWebhookService) resolveUserID(ctx context.Context, username string) (string, error) {
	if s.ProfileRepo == nil {
		return "", fmt.Errorf("profile repo is not configured")
	}
	userID, err := s.ProfileRepo.GetUserIDByUsername(ctx, username)
	if err != nil {
		return "", fmt.Errorf("failed to resolve username '%s': %w", username, err)
	}
	return userID, nil
}

func (s *CCBillWebhookService) paymentService() *payments.PaymentService {
	if s.PaymentService != nil {
		return s.PaymentService
	}
	if s.DB != nil {
		return payments.NewPaymentService(s.DB, s.Clock)
	}
	return nil
}

func ccbillPayloadStringField(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

func (s *CCBillWebhookService) stableDedupeEventKey() string {
	body := json.RawMessage(s.Data.EventBody)
	if len(body) == 0 {
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(s.Data.EventType))))
		return "ccbill:empty:" + hex.EncodeToString(sum[:8])
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		sum := sha256.Sum256(body)
		return "ccbill:raw:" + hex.EncodeToString(sum[:8])
	}

	if txID := ccbillPayloadStringField(payload, "transactionId"); txID != "" {
		return "tx:" + txID
	}

	canonical, err := json.Marshal(payload)
	if err != nil {
		sum := sha256.Sum256(body)
		return "ccbill:raw:" + hex.EncodeToString(sum[:8])
	}

	sum := sha256.Sum256(canonical)
	return "ccbill:event:" + hex.EncodeToString(sum[:8])
}

type CCBillWebhookEventType = string

const CCBillRail models.Rail = "ccbill"

const (
	EventTypeNewSaleSuccess     CCBillWebhookEventType = "NewSaleSuccess"
	EventTypeNewSaleFailure     CCBillWebhookEventType = "NewSaleFailure"
	EventTypeRenewalSuccess     CCBillWebhookEventType = "RenewalSuccess"
	EventTypeRenewalFailure     CCBillWebhookEventType = "RenewalFailure"
	EventTypeUpgradeSuccess     CCBillWebhookEventType = "UpgradeSuccess"
	EventTypeUpgradeFailure     CCBillWebhookEventType = "UpgradeFailure"
	EventTypeCancellation       CCBillWebhookEventType = "Cancellation"
	EventTypeExpiration         CCBillWebhookEventType = "Expiration"
	EventTypeBillingDateChange  CCBillWebhookEventType = "BillingDateChange"
	EventTypeCustomerDataUpdate CCBillWebhookEventType = "CustomerDataUpdate"
	EventTypeUserReactivation   CCBillWebhookEventType = "UserReactivation"
	EventTypeRefund             CCBillWebhookEventType = "Refund"
	EventTypeVoid               CCBillWebhookEventType = "Void"
	EventTypeChargeback         CCBillWebhookEventType = "Chargeback"
)

type BillingError struct {
	Type    string                 `json:"type"`
	Message string                 `json:"message"`
	Context map[string]interface{} `json:"context"`
	Err     error                  `json:"-"`
}

func (be *BillingError) Error() string {
	if be.Err != nil {
		return fmt.Sprintf("%s: %s (%v)", be.Type, be.Message, be.Err)
	}
	return fmt.Sprintf("%s: %s", be.Type, be.Message)
}

func (be *BillingError) Unwrap() error {
	return be.Err
}

const (
	ErrorTypeValidation    = "validation_error"
	ErrorTypeAmount        = "amount_mismatch"
	ErrorTypeDuplicate     = "duplicate_transaction"
	ErrorTypeStatusChange  = "invalid_status_change"
	ErrorTypeBusinessLogic = "business_logic_error"
	ErrorTypeDatabase      = "database_error"
	ErrorTypeNotFound      = "not_found"
)

func shouldTreatCCBillErrorAsNonRetryable(err error) bool {
	if err == nil {
		return false
	}

	var billingErr *BillingError
	if errors.As(err, &billingErr) {
		switch billingErr.Type {
		case ErrorTypeValidation, ErrorTypeAmount, ErrorTypeDuplicate, ErrorTypeStatusChange:
			return true
		case ErrorTypeBusinessLogic, ErrorTypeDatabase, ErrorTypeNotFound:
			return false
		}
	}

	// Subscription lookups can fail due to out-of-order webhook delivery.
	// Keep these retryable.
	if db.IsNotFound(err) {
		return false
	}

	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "failed to parse billedinitialprice") ||
		strings.Contains(msg, "failed to parse billedamount") ||
		strings.Contains(msg, "invalid billedamount") ||
		strings.Contains(msg, "failed to parse nextrenewaldate") ||
		strings.Contains(msg, "payment form id mismatch") ||
		strings.Contains(msg, "payment form name mismatch") {
		return true
	}

	return false
}

func wrapCCBillWebhookErrorForRetry(err error) error {
	if err == nil {
		return nil
	}
	if shouldTreatCCBillErrorAsNonRetryable(err) {
		return MarkWebhookErrorNonRetryable(err)
	}
	return err
}

func (s *CCBillWebhookService) HandleCCBillWebhook(ctx context.Context) error {
	if err := s.validateWebhookAuth(ctx); err != nil {
		return MarkWebhookErrorNonRetryable(err)
	}
	if s.Data.EventType != EventTypeNewSaleSuccess && s.Data.EventType != EventTypeRenewalSuccess && s.DeduplicationService != nil {
		return s.DeduplicationService.ProcessWebhook(
			ctx,
			s.stableDedupeEventKey(),
			string(s.Data.EventType),
			models.RailCCBill,
			s.Data,
			s.handleCCBillWebhookDispatch,
		)
	}
	return s.handleCCBillWebhookDispatch(ctx)
}

func (s *CCBillWebhookService) handleCCBillWebhookDispatch(ctx context.Context) error {
	switch s.Data.EventType {
	case EventTypeNewSaleSuccess:
		return s.handleNewSaleSuccess(ctx)
	case EventTypeNewSaleFailure:
		return s.handleNewSaleFailure(ctx)
	case EventTypeRenewalSuccess:
		return s.handleRenewalSuccess(ctx)
	case EventTypeRenewalFailure:
		return s.handleRenewalFailure(ctx)
	case EventTypeUpgradeSuccess:
		return s.handleUpgradeSuccess(ctx)
	case EventTypeUpgradeFailure:
		return s.handleUpgradeFailure(ctx)
	case EventTypeCancellation:
		return s.handleCancel(ctx)
	case EventTypeExpiration:
		return s.handleExpiration(ctx)
	case EventTypeBillingDateChange:
		return s.handleBillingDateChange(ctx)
	case EventTypeCustomerDataUpdate:
		return s.handleCustomerDataUpdate(ctx)
	case EventTypeUserReactivation:
		return s.handleUserReactivation(ctx)
	case EventTypeRefund:
		return s.handleRefund(ctx)
	case EventTypeVoid:
		return s.handleVoid(ctx)
	case EventTypeChargeback:
		return s.handleChargeback(ctx)
	default:
		log.WithContext(ctx).WithFields(log.Fields{
			"rail":       "ccbill",
			"event_type": s.Data.EventType,
		}).Warn("Unsupported CCBill webhook event type")
		return MarkWebhookErrorNonRetryable(fmt.Errorf("unsupported event type: %s", s.Data.EventType))
	}
}

func (s *CCBillWebhookService) validateWebhookAuth(ctx context.Context) error {
	// Fail CLOSED: the accnum/subacc match against the ctx merchant's armed
	// account is the only per-merchant authentication a CCBill callback gets
	// (the rail has no HMAC). No client = nothing to check it against.
	if s.CCBillClient == nil {
		return fmt.Errorf("ccbill webhook auth failed: no armed ccbill account to authenticate against")
	}

	var common CCBillCommonFields
	if err := json.Unmarshal(s.Data.EventBody, &common); err != nil {
		return fmt.Errorf("parse ccbill webhook auth fields: %w", err)
	}
	clientAccnum := common.ClientAccnum.Trimmed()
	clientSubacc := strings.TrimSpace(common.ClientSubacc)
	if err := s.CCBillClient.ValidateWebhookAuth(clientAccnum, clientSubacc); err != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"event_type":        s.Data.EventType,
			"client_accnum_set": clientAccnum != "",
			"client_subacc_set": clientSubacc != "",
		}).Warn("CCBill webhook rejected due to account mismatch")
		return fmt.Errorf("ccbill webhook auth failed: %w", err)
	}
	return nil
}

func (s *CCBillWebhookService) handleNewSaleSuccess(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill webhook notification")

	var data CCBillNewSaleSuccessEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	process := func(ctx context.Context) error {
		return wrapCCBillWebhookErrorForRetry(s.handleNewSaleSuccessInternal(ctx, &data))
	}

	if s.DeduplicationService != nil && strings.TrimSpace(data.TransactionID) != "" {
		return s.DeduplicationService.ProcessWebhook(
			ctx,
			data.TransactionID,
			string(s.Data.EventType),
			models.RailCCBill,
			data,
			process,
		)
	}

	return process(ctx)
}

// ccbillEventPurchasedAt parses a CCBill event `timestamp` ("YYYY-MM-DD HH:MM:SS",
// no tz → UTC) into the provider's transaction time (#651); nil when absent or
// unparseable so the payment row records now() rather than a guessed time. CCBill
// webhooks can be delayed/retried, so this is more truthful than processing time.
func ccbillEventPurchasedAt(ts string) *time.Time {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return nil
	}
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", ts, time.UTC)
	if err != nil {
		return nil
	}
	p := parsed.UTC()
	return &p
}

func (s *CCBillWebhookService) handleNewSaleSuccessInternal(ctx context.Context, data *CCBillNewSaleSuccessEvent) error {
	email := data.Email
	formID := data.FlexID
	transactionID := strings.TrimSpace(data.TransactionID)
	if transactionID == "" {
		return newBillingError(ErrorTypeValidation, "missing required field: transactionId", map[string]interface{}{"field": "transactionId"}, nil)
	}
	userID, err := s.resolveUserID(ctx, data.Username)
	if err != nil {
		return err
	}
	formName := data.FormName
	ccBillSubID := data.SubscriptionID

	// Get price information
	priceLookupID := ccbillPriceLookupID(data.SubscriptionTypeID, data.FlexID)
	price, err := s.PriceService.GetByCCBillPriceID(ctx, priceLookupID)
	if err != nil {
		return fmt.Errorf("failed to find price for CCBill price ID %s: %w", priceLookupID, err)
	}
	if err := s.ensureCCBillPriceMatches(price, formID, formName, data.SubscriptionTypeID); err != nil {
		return err
	}

	expectedAmountMicros := ccbillInitialChargeAmount(price)
	billedAmountCents, err := parseCCBillAmountCents(data.BilledInitialPrice, "billedInitialPrice", "billedAmount", expectedAmountMicros == 0)
	if err != nil {
		return err
	}
	// Validate amount against expected cents from catalog price.
	if err := validateCCBillBilledAmount(ctx, s, price.Currency, billedAmountCents, expectedAmountMicros, map[string]interface{}{
		"price_id":                           price.ID.String(),
		"ccbill_price_id":                    priceLookupID,
		"recurring_billing_option_id":        data.SubscriptionTypeID,
		"subscription_expected_first_amount": expectedAmountMicros,
	}, log.Fields{
		"transaction_id": transactionID,
	}); err != nil {
		return err
	}

	// Use SubscriptionLifecycleService to create membership
	var emailPtr *string
	if strings.TrimSpace(email) != "" {
		emailCopy := strings.TrimSpace(email)
		emailPtr = &emailCopy
	}

	currencyValue, err := requireCCBillCurrency(data.BilledCurrencyCode, "billedCurrencyCode")
	if err != nil {
		return err
	}
	if err := validateCCBillCurrencyMatches(currencyValue, price.Currency, map[string]interface{}{
		"price_id":        price.ID.String(),
		"ccbill_price_id": priceLookupID,
		"transaction_id":  transactionID,
	}); err != nil {
		return err
	}
	paidTermEnd, err := parseCCBillDateUsingTimestamp(data.NextRenewalDate)
	if err != nil {
		return fmt.Errorf("failed to parse nextRenewalDate '%s': %w", data.NextRenewalDate, err)
	}

	if s.DB != nil {
		removed, err := subscriptions.RemoveCancelledSubscriptionsForActivation(ctx, s.DB, userID, price.ProductID, uuid.Nil)
		if err != nil {
			return fmt.Errorf("failed to cleanup cancelled subscriptions before activation: %w", err)
		}
		if removed > 0 {
			log.WithContext(ctx).WithFields(log.Fields{
				"user_id":     userID,
				"product_id":  price.ProductID,
				"removed_cnt": removed,
			}).Info("Removed cancelled subscriptions before activation (CCBill)")
		}
	}

	// CreateMembership now creates the Payment record internally
	subscription, err := s.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
		UserID:              userID,
		PriceID:             price.ID,
		Rail:                models.RailCCBill,
		RailSubscriptionID:  &ccBillSubID,
		UserEmail:           emailPtr,
		CurrentPeriodEndsAt: paidTermEnd,
		TransactionID:       transactionID,
		Amount:              int64(moneyutil.CentsToMicros(billedAmountCents)),
		AmountProvided:      true,
		Currency:            currencyValue,
		PurchasedAt:         ccbillEventPurchasedAt(data.Timestamp),
	})
	if err != nil {
		return fmt.Errorf("failed to create membership: %w", err)
	}

	if s.MoneyService != nil {
		periodEnd := time.Time{}
		if paidTermEnd != nil {
			periodEnd = paidTermEnd.UTC()
		} else if subscription.CurrentPeriodEndsAt != nil {
			periodEnd = subscription.CurrentPeriodEndsAt.UTC()
		}
		if !periodEnd.IsZero() {
			if err := s.MoneyService.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
				SubscriptionID: subscription.ID,
				PeriodEnd:      periodEnd,
				Cadence:        models.CreditGrantCadenceOnce,
				Source:         "subscription_initial",
			}); err != nil {
				// #675: propagate for retry — the deposit is idempotent per
				// (subscription, label, period_end); warn-and-ack lost the lot.
				return fmt.Errorf("grant initial subscription credits (CCBill): %w", err)
			}
		}
	}

	if s.CheckoutSessionService != nil {
		session, err := s.findCCBillCheckoutSession(ctx, data.ReservationID, userID, price.ID)
		if err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"user_id":        userID,
				"price_id":       price.ID,
				"reservation_id": data.ReservationID,
			}).Warn("failed to locate checkout session for CCBill webhook")
		} else if session != nil {
			paymentID := uuid.Nil
			if s.PaymentService != nil && strings.TrimSpace(transactionID) != "" {
				if payment, err := s.PaymentService.GetByTransactionID(ctx, models.RailCCBill, transactionID); err == nil {
					paymentID = payment.ID
				}
			}
			if err := s.CheckoutSessionService.MarkSucceededWithSubscription(ctx, session.ID, paymentID, transactionID, subscription.ID); err != nil {
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"checkout_session_id": session.ID,
					"transaction_id":      transactionID,
				}).Warn("failed to update checkout session from CCBill webhook")
			}
		}
	}

	return nil
}

func (s *CCBillWebhookService) findCCBillCheckoutSession(ctx context.Context, reservationID string, userID string, priceID uuid.UUID) (*models.CheckoutSession, error) {
	if s.CheckoutSessionService == nil {
		return nil, sql.ErrNoRows
	}
	if strings.TrimSpace(reservationID) != "" {
		session, err := s.CheckoutSessionService.FindOpenCCBillReservation(ctx, reservationID, userID, priceID)
		if err == nil || !db.IsNotFound(err) {
			return session, err
		}
	}
	return s.CheckoutSessionService.FindOpenByUserPriceRail(ctx, userID, priceID, models.RailCCBill)
}

func (s *CCBillWebhookService) handleNewSaleFailure(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill new sale failure notification")

	var data CCBillNewSaleFailureEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	formID := data.FlexID
	formName := data.FormName
	failureCode := data.FailureCode
	transactionID := data.TransactionID
	failureReason := data.FailureReason
	priceLookupID := ccbillPriceLookupID(data.SubscriptionTypeID, data.FlexID)
	price, priceLookupErr := s.PriceService.GetByCCBillPriceID(ctx, priceLookupID)
	if _, err := requireCCBillCurrency(data.BilledCurrencyCode, "billedCurrencyCode"); err != nil {
		return err
	}

	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		userID, err := s.resolveUserID(ctx, data.Username)
		if err != nil {
			return err
		}

		if priceLookupErr != nil {
			log.WithContext(ctx).WithError(priceLookupErr).WithFields(log.Fields{
				"flex_id": formID,
			}).Warn("Unable to validate CCBill form for new sale failure")
		} else if err := s.ensureCCBillPriceMatches(price, formID, formName, data.SubscriptionTypeID); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"flex_id": formID,
			}).Warn("Payment form mismatch in new sale failure")
		}

		// Add notification to queue for user about payment failure and send immediate email
		if s.NotificationService != nil && userID != "" {
			notification := &models.NotificationQueue{
				ID:         uuidutil.NewV7(),
				CustomerID: identity.CustomerIDFromString(userID).UUID(),
				EventType:  models.NotificationPaymentMethodFailed,
			}
			if err := s.NotificationService.CreateAndDeliver(ctx, notification); err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to create and deliver new sale failure notification")
			}
		}

		// #733: durably record the declined initial attempt as a failed
		// payments row (verbatim failureCode + normalized reason).
		if price != nil && userID != "" && strings.TrimSpace(transactionID) != "" {
			txdb := db.NewWithPgxTx(tx)
			kind := payments.AttemptInitial
			failed := &models.Payment{
				ID:            uuidutil.NewV7(),
				CustomerID:    identity.CustomerIDFromString(userID).UUID(),
				PriceID:       price.ID,
				Rail:          models.RailCCBill,
				TransactionID: strings.TrimSpace(transactionID),
				Amount:        price.Amount,
				ListAmount:    price.Amount,
				Currency:      price.Currency,
				Status:        payments.PaymentStatusFailedValue,
				AttemptKind:   &kind,
				MoneyMovement: models.MoneyMovementNone, // or#827: a decline moved nothing.
				PurchasedAt:   s.now(),
				CreatedAt:     s.now(),
			}
			if code := strings.TrimSpace(failureCode); code != "" {
				reason := payments.NormalizeFailureReason(string(models.RailCCBill), code)
				failed.FailureCode = &code
				failed.FailureReason = &reason
			}
			if _, err := payments.NewPaymentService(txdb, s.Clock).CreateIfNotExists(ctx, failed); err != nil {
				log.WithContext(ctx).WithError(err).WithField("transaction_id", transactionID).Error("failed to record CCBill new-sale decline payment row")
			}
		}

		if s.CheckoutSessionService != nil && price != nil {
			session, err := s.findCCBillCheckoutSession(ctx, data.ReservationID, userID, price.ID)
			if err != nil {
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"user_id":        userID,
					"price_id":       price.ID,
					"reservation_id": data.ReservationID,
				}).Warn("failed to locate checkout session for CCBill failure")
			} else if session != nil {
				message := strings.TrimSpace(failureReason)
				if message == "" {
					message = "payment failed"
				}
				if err := s.CheckoutSessionService.MarkFailed(ctx, session.ID, message, failureCode); err != nil {
					log.WithContext(ctx).WithError(err).WithFields(log.Fields{
						"checkout_session_id": session.ID,
						"transaction_id":      transactionID,
					}).Warn("failed to update checkout session from CCBill failure")
				}
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"userID":        userID,
			"failureCode":   failureCode,
			"failureReason": failureReason,
			"transactionID": transactionID,
		}).Info("Handled new sale failure")

		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (s *CCBillWebhookService) handleUpgradeSuccess(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill upgrade success notification")

	var data CCBillUpgradeSuccessEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	flexID := strings.TrimSpace(data.FlexID)
	formName := strings.TrimSpace(data.FormName)
	ccBillSubID := strings.TrimSpace(data.SubscriptionID)
	transactionID := strings.TrimSpace(data.TransactionID)
	originalSubscriptionID := strings.TrimSpace(data.OriginalSubscriptionID)
	billedAmountStr := strings.TrimSpace(data.BilledInitialPrice)

	if billedAmountStr == "" {
		return fmt.Errorf("missing required field: billedInitialPrice")
	}

	if strings.TrimSpace(ccBillSubID) == "" {
		return fmt.Errorf("missing required field: subscriptionId")
	}

	if originalSubscriptionID == "" {
		return fmt.Errorf("missing required field: originalSubscriptionId")
	}

	if transactionID == "" {
		return fmt.Errorf("missing required field: transactionId")
	}

	currencyValue, err := requireCCBillCurrency(data.BilledCurrencyCode, "billedCurrencyCode")
	if err != nil {
		return err
	}

	paymentService := s.paymentService()
	if paymentService != nil {
		existingPayment, err := paymentService.GetByTransactionID(ctx, models.RailCCBill, transactionID)
		if err != nil && !db.IsNotFound(err) {
			return fmt.Errorf("failed to check existing upgrade payment: %w", err)
		}
		if err == nil {
			log.WithContext(ctx).WithFields(log.Fields{
				"transaction_id": transactionID,
				"payment_id":     existingPayment.ID,
			}).Info("Duplicate CCBill UpgradeSuccess webhook, skipping")
			return nil
		}
	}

	var mismatchRepairAlert *ledgerRepairAlert
	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		entitlementService := entitlements.NewEntitlementService(txdb, s.Clock)
		paymentService := payments.NewPaymentService(txdb, s.Clock)
		subService := subscriptions.NewSubscriptionService(txdb, priceService, productService, nil, s.Clock)

		// Find subscription by the original rail subscription ID and then transition it.
		subscription, err := subService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), originalSubscriptionID)
		if err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("subscription not found for original rail subscription ID: %s", originalSubscriptionID)
			}
			return fmt.Errorf("failed to get subscription: %w", err)
		}

		// Store old price ID before updating
		oldPriceID := subscription.PriceID

		priceLookupID := ccbillPriceLookupID(data.SubscriptionTypeID, data.FlexID)
		newPrice, err := priceService.GetByCCBillPriceID(ctx, priceLookupID)
		if err != nil {
			return fmt.Errorf("failed to find new price for CCBill price ID %s: %w", priceLookupID, err)
		}
		if err := s.ensureCCBillPriceMatches(newPrice, flexID, formName, data.SubscriptionTypeID); err != nil {
			return err
		}

		// Validate the billed amount matches the new price.
		expectedAmountMicros := ccbillInitialChargeAmount(newPrice)
		billedAmountCents, err := parseCCBillAmountCents(billedAmountStr, "billedInitialPrice", "billedAmount", expectedAmountMicros == 0)
		if err != nil {
			return err
		}
		expectedAmountCents, err := moneyutil.NativeToRailMinorExact(newPrice.Currency, int64(expectedAmountMicros))
		if err != nil {
			return err
		}
		tolerance := expectedAmountCents * 2 / 100 // 2% tolerance, integer-only (#818)
		if billedAmountCents < (expectedAmountCents-tolerance) || billedAmountCents > (expectedAmountCents+tolerance) {
			billingErr := newBillingError(ErrorTypeAmount,
				"Upgrade billed amount does not match expected price",
				map[string]interface{}{
					"expected_amount_cents":       expectedAmountCents,
					"billed_amount_cents":         billedAmountCents,
					"tolerance_cents":             tolerance,
					"new_price_id":                newPrice.ID.String(),
					"new_flex_id":                 flexID,
					"recurring_billing_option_id": data.SubscriptionTypeID,
					"original_subscription_id":    originalSubscriptionID,
					"new_subscription_id":         ccBillSubID,
				}, nil)

			s.logBillingError(ctx, billingErr, log.Fields{
				"transaction_id":  transactionID,
				"subscription_id": subscription.ID,
			})
			// #675: durable operator trace for the real drifted charge — captured
			// here, written after the tx rolls back (see mismatchRepairAlert).
			subscriptionID := subscription.ID
			mismatchRepairAlert = &ledgerRepairAlert{
				Provider:       string(models.RailCCBill),
				Operation:      "amount_mismatch",
				TransactionID:  transactionID,
				UserID:         subscription.CustomerID.String(),
				SubscriptionID: &subscriptionID,
				Err:            billingErr,
				Metadata:       billingErr.Context,
			}
			return billingErr
		}

		now := s.now().UTC()
		// #651: record CCBill's own event time when present; now() as fallback.
		purchasedAt := now
		if t := ccbillEventPurchasedAt(data.Timestamp); t != nil {
			purchasedAt = *t
		}
		payment := &models.Payment{
			ID:             uuidutil.NewV7(),
			CustomerID:     subscription.CustomerID,
			PriceID:        newPrice.ID,
			SubscriptionID: &subscription.ID,
			Rail:           models.RailCCBill,
			TransactionID:  transactionID,
			Amount:         int64(moneyutil.CentsToMicros(billedAmountCents)),
			ListAmount:     newPrice.Amount,
			Currency:       currencyValue,
			AttemptKind:    func() *string { k := payments.AttemptRenewal; return &k }(),
			MoneyMovement:  models.MoneyMovementRail, // or#827: CCBill billed the upgrade.
			PurchasedAt:    purchasedAt,
			CreatedAt:      now,
		}

		created, err := paymentService.CreateIfNotExists(ctx, payment)
		if err != nil {
			return fmt.Errorf("failed to create upgrade payment: %w", err)
		}
		if !created {
			log.WithContext(ctx).WithFields(log.Fields{
				"transaction_id":  transactionID,
				"subscription_id": subscription.ID,
			}).Info("Duplicate CCBill UpgradeSuccess transaction detected in transaction, skipping")
			return nil
		}

		if err = subscription.ActivateWithPrice(newPrice); err != nil {
			return fmt.Errorf("failed to activate subscription: %w", err)
		}

		subscription.RailSubscriptionID = ccBillSubID

		if err = subscription.Validate(int64(billedAmountCents)); err != nil {
			return fmt.Errorf("failed to validate subscription: %w", err)
		}

		if err = subService.Update(ctx, subscription); err != nil {
			return fmt.Errorf("failed to update subscription: %w", err)
		}

		// Update entitlements based on product tier change
		if err := s.updateEntitlementsForUpgrade(ctx, txdb, entitlementService, productService, priceService, subscription, oldPriceID, newPrice.ID); err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to update entitlements for subscription upgrade")
			// Don't fail the webhook - entitlement issues shouldn't block subscription updates
		}

		// Add notification to queue for user about successful upgrade and send immediate email
		if s.NotificationService != nil {
			notification := &models.NotificationQueue{
				ID:         uuidutil.NewV7(),
				CustomerID: subscription.CustomerID,
				EventType:  models.NotificationPremiumRenewed, // Use renewed for upgrades
			}
			if err := s.NotificationService.CreateAndDeliver(ctx, notification); err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to create and deliver upgrade success notification")
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscriptionID":         subscription.ID,
			"userID":                 subscription.CustomerID.String(),
			"oldPriceID":             oldPriceID,
			"newPriceID":             newPrice.ID,
			"billedAmountCents":      billedAmountCents,
			"transactionID":          transactionID,
			"newFlexID":              flexID,
			"originalSubscriptionID": originalSubscriptionID,
			"newSubscriptionID":      ccBillSubID,
		}).Info("Processed subscription upgrade successfully")

		return nil
	}); err != nil {
		if mismatchRepairAlert != nil {
			if alertErr := recordLedgerRepairAlert(ctx, s.NotificationService, s.DB, s.now(), *mismatchRepairAlert); alertErr != nil {
				return fmt.Errorf("record CCBill upgrade amount mismatch repair alert: %w", alertErr)
			}
		}
		return err
	}
	return nil
}

// updateEntitlementsForUpgrade handles entitlement changes when a subscription is upgraded/downgraded.
// It revokes entitlements that are no longer in the new product's spec and grants new ones.
func (s *CCBillWebhookService) updateEntitlementsForUpgrade(
	ctx context.Context,
	txdb *db.DB,
	entitlementService *entitlements.EntitlementService,
	productService *catalog.ProductService,
	priceService *catalog.PriceService,
	subscription *models.Subscription,
	oldPriceID uuid.UUID,
	newPriceID uuid.UUID,
) error {
	// Get old and new prices
	oldPrice, err := priceService.GetByID(ctx, oldPriceID)
	if err != nil {
		return fmt.Errorf("failed to get old price: %w", err)
	}

	newPrice, err := priceService.GetByID(ctx, newPriceID)
	if err != nil {
		return fmt.Errorf("failed to get new price: %w", err)
	}

	// Get old and new products
	oldProduct, err := productService.GetByID(ctx, oldPrice.ProductID)
	if err != nil {
		return fmt.Errorf("failed to get old product: %w", err)
	}

	newProduct, err := productService.GetByID(ctx, newPrice.ProductID)
	if err != nil {
		return fmt.Errorf("failed to get new product: %w", err)
	}

	// Build entitlement sets for old and new products
	oldEntitlements := make(map[string]bool)
	if len(oldProduct.EntitlementsSpec) > 0 {
		for name := range oldProduct.EntitlementsSpec {
			oldEntitlements[name] = true
		}
	} else {
		oldEntitlements["premium"] = true // default entitlement
	}

	newEntitlements := make(map[string]bool)
	if len(newProduct.EntitlementsSpec) > 0 {
		for name := range newProduct.EntitlementsSpec {
			newEntitlements[name] = true
		}
	} else {
		newEntitlements["premium"] = true // default entitlement
	}

	now := s.now()

	// Revoke entitlements that are no longer in the new product (downgrade case)
	for oldEnt := range oldEntitlements {
		if !newEntitlements[oldEnt] {
			// This entitlement is being removed - revoke only this specific entitlement
			reason := models.EntitlementRevokeDowngrade
			st := models.EntitlementSourceSubscription
			sid := subscription.ID
			if err := entitlementService.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
				UserID:      subscription.CustomerID.String(),
				Entitlement: oldEnt,
				SourceType:  &st,
				SourceID:    &sid,
				Reason:      reason,
			}); err != nil {
				log.WithContext(ctx).WithError(err).WithField("entitlement", oldEnt).Warn("failed to revoke entitlement during upgrade")
			} else {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"entitlement":     oldEnt,
					"action":          "revoked",
				}).Info("Revoked entitlement during subscription tier change")
			}
		}
	}

	// Grant new entitlements that weren't in the old product (upgrade case)
	for newEnt := range newEntitlements {
		if !oldEntitlements[newEnt] {
			// This is a new entitlement - check if it already exists
			exists, err := entitlementService.ExistsBySource(ctx, models.EntitlementSourceSubscription, subscription.ID, newEnt)
			if err != nil {
				log.WithContext(ctx).WithError(err).WithField("entitlement", newEnt).Warn("failed to check entitlement existence")
				continue
			}
			if exists {
				continue
			}

			// Grant new entitlement window tied to subscription.
			notBefore := now.UTC()
			var params entitlements.PushNewEntitlementParams
			if subscription.CurrentPeriodEndsAt != nil && subscription.CurrentPeriodEndsAt.After(now) {
				endAt := subscription.CurrentPeriodEndsAt.UTC()
				params = entitlements.PushNewEntitlementParams{
					UserID:      subscription.CustomerID.String(),
					Entitlement: newEnt,
					NotBefore:   &notBefore,
					EndAt:       &endAt,
					SourceType:  models.EntitlementSourceSubscription,
					SourceID:    subscription.ID,
				}
			} else {
				params = entitlements.PushNewEntitlementParams{
					UserID:      subscription.CustomerID.String(),
					Entitlement: newEnt,
					NotBefore:   &notBefore,
					Indefinite:  true,
					SourceType:  models.EntitlementSourceSubscription,
					SourceID:    subscription.ID,
				}
			}
			if _, err := entitlementService.PushNewEntitlement(ctx, params); err != nil {
				log.WithContext(ctx).WithError(err).WithField("entitlement", newEnt).Warn("failed to grant entitlement during upgrade")
			} else {
				log.WithContext(ctx).WithFields(log.Fields{
					"subscription_id": subscription.ID,
					"user_id":         subscription.CustomerID.String(),
					"entitlement":     newEnt,
					"action":          "granted",
				}).Info("Granted new entitlement during subscription tier change")
			}
		}
	}

	// For entitlements that exist in both products, no action needed - they continue
	// The indefinite window remains valid

	return nil
}

func (s *CCBillWebhookService) handleUpgradeFailure(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill upgrade failure notification")

	var data CCBillUpgradeFailureEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	transactionID := data.TransactionID
	failureCode := data.FailureCode
	failureReason := data.FailureReason
	originalSubscriptionID := data.OriginalSubscriptionID
	if _, err := requireCCBillCurrency(data.BilledCurrencyCode, "billedCurrencyCode"); err != nil {
		return err
	}

	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		userID, err := s.resolveUserID(ctx, data.Username)
		if err != nil {
			return err
		}

		// Add notification to queue for user about upgrade failure and send immediate email
		if s.NotificationService != nil && userID != "" {
			notification := &models.NotificationQueue{
				ID:         uuidutil.NewV7(),
				CustomerID: identity.CustomerIDFromString(userID).UUID(),
				EventType:  models.NotificationPaymentMethodFailed,
			}
			if err := s.NotificationService.CreateAndDeliver(ctx, notification); err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to create and deliver upgrade failure notification")
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"userID":                 userID,
			"failureCode":            failureCode,
			"failureReason":          failureReason,
			"transactionID":          transactionID,
			"originalSubscriptionID": originalSubscriptionID,
		}).Info("Handled upgrade failure")

		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (s *CCBillWebhookService) handleBillingDateChange(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill billing date change notification")

	var data CCBillBillingDateChangeEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	pSubscriptionID := data.SubscriptionID
	nextRenewalDate := data.NextRenewalDate

	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		subService := subscriptions.NewSubscriptionService(txdb, priceService, productService, nil, s.Clock)

		// Find subscription by rail subscription ID
		sub, err := subService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), pSubscriptionID)
		if err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("subscription not found for rail subscription ID: %s", pSubscriptionID)
			}
			return fmt.Errorf("failed to get subscription: %w", err)
		}

		parsed, err := parseCCBillDateUsingTimestamp(nextRenewalDate)
		if err != nil {
			return fmt.Errorf("failed to parse nextRenewalDate '%s': %w", nextRenewalDate, err)
		}
		if parsed == nil {
			return fmt.Errorf("missing nextRenewalDate")
		}

		// Update subscription billing date
		sub.CurrentPeriodEndsAt = parsed

		if err := subService.Update(ctx, sub); err != nil {
			return fmt.Errorf("failed to update subscription billing date: %w", err)
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscriptionID":     sub.ID,
			"userID":             sub.CustomerID.String(),
			"railSubscriptionID": pSubscriptionID,
			"newRenewalDate":     parsed,
		}).Info("Updated subscription billing date successfully")

		// #678: dedup mark commits atomically with the billing-date effect.
		return MarkWebhookProcessedInTx(ctx, tx)
	}); err != nil {
		return err
	}
	return nil
}

func (s *CCBillWebhookService) handleCustomerDataUpdate(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill customer data update notification")

	var data CCBillCustomerDataUpdateEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	pSubscriptionID := data.SubscriptionID

	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		subService := subscriptions.NewSubscriptionService(txdb, priceService, productService, nil, s.Clock)

		// Find subscription by rail subscription ID
		sub, err := subService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), pSubscriptionID)
		if err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("subscription not found for rail subscription ID: %s", pSubscriptionID)
			}
			return fmt.Errorf("failed to get subscription: %w", err)
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscriptionID":     sub.ID,
			"userID":             sub.CustomerID.String(),
			"railSubscriptionID": pSubscriptionID,
		}).Info("Processed customer data update successfully")

		// #678: dedup mark commits atomically with the customer-data effect.
		return MarkWebhookProcessedInTx(ctx, tx)
	}); err != nil {
		return err
	}

	return nil
}

func (s *CCBillWebhookService) handleUserReactivation(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill user reactivation notification")

	var data CCBillUserReactivationEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	pSubscriptionID := strings.TrimSpace(data.SubscriptionID)
	transactionID := strings.TrimSpace(data.TransactionID)
	priceStr := strings.TrimSpace(data.Price)
	nextRenewalDate := strings.TrimSpace(data.NextRenewalDate)

	if pSubscriptionID == "" {
		return fmt.Errorf("missing required field: subscriptionId")
	}
	if transactionID == "" {
		return fmt.Errorf("missing required field: transactionId")
	}
	if s.SubscriptionLifecycleService == nil {
		return fmt.Errorf("subscription lifecycle service not configured")
	}

	renewalDate, err := parseCCBillDateUsingTimestamp(nextRenewalDate)
	if err != nil {
		return fmt.Errorf("failed to parse nextRenewalDate '%s': %w", nextRenewalDate, err)
	}
	if renewalDate == nil || !renewalDate.After(s.now().UTC()) {
		return fmt.Errorf("reactivation requires future nextRenewalDate")
	}

	sub, err := s.SubscriptionLifecycleService.ReactivateMembership(ctx, &subscriptions.ReactivateMembershipParams{
		Rail:                models.RailCCBill,
		RailSubscriptionID:  pSubscriptionID,
		CurrentPeriodEndsAt: renewalDate,
	})
	if err != nil {
		if subscriptions.IsTerminalTransitionBlocked(err) {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"rail_subscription_id": pSubscriptionID,
				"transaction_id":       transactionID,
			}).Warn("Blocked terminal -> active transition for CCBill UserReactivation")
			return nil
		}
		return fmt.Errorf("failed to reactivate membership: %w", err)
	}

	// Add notification to queue for user about reactivation and send immediate email
	if s.NotificationService != nil {
		notification := &models.NotificationQueue{
			ID:         uuidutil.NewV7(),
			CustomerID: sub.CustomerID,
			EventType:  models.NotificationPremiumStarted, // Use started for reactivations
		}
		if err := s.NotificationService.CreateAndDeliver(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to create and deliver reactivation notification")
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscriptionID":     sub.ID,
		"userID":             sub.CustomerID.String(),
		"transactionID":      transactionID,
		"railSubscriptionID": pSubscriptionID,
		"priceDescription":   priceStr,
		"nextRenewalDate":    nextRenewalDate,
		"periodEndsAt":       sub.CurrentPeriodEndsAt,
	}).Info("Processed user reactivation successfully")

	return nil
}

func (s *CCBillWebhookService) handleRefund(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill refund notification")

	var data CCBillRefundEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	pSubscriptionID := data.SubscriptionID
	refundAmountStr := data.Amount
	refundTransactionID := data.TransactionID // Use TransactionID as the refund transaction ID
	refundReason := data.Reason
	refundAmountCents, err := parseCCBillPositiveAmountCents(refundAmountStr, "refund amount", "amount")
	if err != nil {
		return err
	}
	if _, err := requireCCBillCurrency(data.CurrencyCode, "currencyCode"); err != nil {
		return err
	}

	var refundLedgerErr error
	var refundRepairAlert *ledgerRepairAlert
	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		subService := subscriptions.NewSubscriptionService(txdb, priceService, productService, nil, s.Clock)
		entSvc := entitlements.NewEntitlementService(txdb, s.Clock)
		paymentService := payments.NewPaymentService(txdb, s.Clock)

		// Find subscription by rail subscription ID
		sub, err := subService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), pSubscriptionID)
		if err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("subscription not found for rail subscription ID: %s", pSubscriptionID)
			}
			return fmt.Errorf("failed to get subscription: %w", err)
		}

		// Determine if we should terminate the subscription based on refund type
		shouldTerminate := false
		// Determine if we should terminate the subscription based on refund amount
		// If refund amount is significant relative to the subscription price, terminate
		if sub.Price != nil && sub.Price.Amount > 0 {
			// Use integer math: percentage = (refundCents * 100) / priceAmount
			refundPercentage := (int64(moneyutil.CentsToMicros(moneyutil.Cents(refundAmountCents))) * 100) / sub.Price.Amount
			if refundPercentage >= 80 { // If refund is 80%+ of price, terminate
				shouldTerminate = true
			}
		}

		var originalPayment *models.Payment
		if refundTransactionID != "" && refundAmountCents > 0 {
			reversalID := "refund:" + strings.TrimSpace(refundTransactionID)
			existingRefund, lookupErr := paymentService.GetByTransactionID(ctx, models.RailCCBill, reversalID)
			switch {
			case lookupErr == nil && existingRefund != nil:
				log.WithContext(ctx).WithFields(log.Fields{
					"refund_transaction_id": reversalID,
					"payment_id":            existingRefund.ID,
				}).Info("CCBill refund payment already exists; skipping duplicate ledger insert")
			case lookupErr != nil && !db.IsNotFound(lookupErr):
				err := fmt.Errorf("failed to check existing refund payment: %w", lookupErr)
				if shouldTerminate {
					refundLedgerErr = err
					log.WithContext(ctx).WithError(err).Error("Failed to check CCBill refund ledger; continuing access revocation")
				} else {
					return err
				}
			default:
				var originalErr error
				originalPayment, originalErr = paymentService.GetByTransactionID(ctx, models.RailCCBill, refundTransactionID)
				if db.IsNotFound(originalErr) {
					originalPayment, originalErr = paymentService.GetLatestChargeBySubscriptionID(ctx, sub.ID)
				}
				if originalErr != nil {
					if !db.IsNotFound(originalErr) {
						err := fmt.Errorf("failed to resolve original payment for refund: %w", originalErr)
						if shouldTerminate {
							refundLedgerErr = err
							log.WithContext(ctx).WithError(err).Error("Failed to resolve CCBill refund ledger payment; continuing access revocation")
						} else {
							return err
						}
						break
					}
					if shouldTerminate {
						refundLedgerErr = fmt.Errorf("failed to resolve original payment for terminating CCBill refund transaction %q", refundTransactionID)
						log.WithContext(ctx).WithError(refundLedgerErr).Error("Failed to resolve CCBill refund ledger payment; continuing access revocation")
						break
					}
					log.WithContext(ctx).WithFields(log.Fields{
						"subscription_id":       sub.ID,
						"refund_transaction_id": refundTransactionID,
					}).Warn("No original payment found for CCBill refund ledger linkage")
				} else {
					if _, refundErr := paymentService.Refund(ctx, originalPayment.ID, reversalID, int64(moneyutil.CentsToMicros(moneyutil.Cents(refundAmountCents))), payments.ReversalRefund); refundErr != nil {
						err := fmt.Errorf("failed to persist CCBill refund payment: %w", refundErr)
						if shouldTerminate {
							refundLedgerErr = err
							log.WithContext(ctx).WithError(err).Error("Failed to persist CCBill refund ledger; continuing access revocation")
						} else {
							return err
						}
					}
				}
			}
		}

		now := s.now()

		if shouldTerminate {
			// Terminate the subscription
			if err := sub.ResetCurrentPeriods(); err != nil {
				return err
			}

			cancelType := models.CancelTypeMerchant // Refund is merchant-initiated
			sub.Status = models.StatusCancelled
			sub.CancelType = &cancelType
			sub.CancelledAt = &now
			if refundReason != "" {
				sub.CancelFeedback = &refundReason
			}
			sub.ClearRetrySchedule()

			// End entitlements for this subscription immediately.
			if err := entSvc.RevokeSourcesForSubscription(ctx, sub.CustomerID.String(), sub.ID, models.EntitlementRevokeRefund, models.EntitlementSourceSubscription, models.EntitlementSourceGrace); err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to revoke entitlements for refunded subscription")
			}
			if refundLedgerErr != nil {
				var originalPaymentID *uuid.UUID
				if originalPayment != nil {
					id := originalPayment.ID
					originalPaymentID = &id
				}
				subscriptionID := sub.ID
				refundRepairAlert = &ledgerRepairAlert{
					Provider:          "ccbill",
					Operation:         "refund_reversal",
					TransactionID:     refundTransactionID,
					UserID:            sub.CustomerID.String(),
					OriginalPaymentID: originalPaymentID,
					SubscriptionID:    &subscriptionID,
					Err:               refundLedgerErr,
					Metadata: map[string]any{
						"rail_subscription_id": pSubscriptionID,
						"refund_amount_cents":  refundAmountCents,
						"refund_reason":        refundReason,
					},
				}
			}

			// Add notification to queue for user about account termination due to refund
			if s.NotificationService != nil {
				notification := &models.NotificationQueue{
					ID:         uuidutil.NewV7(),
					CustomerID: sub.CustomerID,
					EventType:  models.NotificationPremiumEnded,
					Data:       map[string]any{"reason": string(subscriptions.PremiumEndReasonRefund)},
				}
				if err := s.NotificationService.CreateAndDeliver(ctx, notification); err != nil {
					log.WithContext(ctx).WithError(err).Error("failed to create and deliver refund termination notification")
				}
			}
		} else {
			// Don't terminate, but log the refund for record keeping
			log.WithContext(ctx).WithFields(log.Fields{
				"subscriptionID":      sub.ID,
				"refundAmountCents":   refundAmountCents,
				"refundType":          "auto_detected",
				"refundTransactionID": refundTransactionID,
			}).Info("Partial refund processed - subscription remains active")
		}

		if err := subService.Update(ctx, sub); err != nil {
			return fmt.Errorf("failed to update subscription after refund: %w", err)
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscriptionID":         sub.ID,
			"userID":                 sub.CustomerID.String(),
			"refundAmountCents":      refundAmountCents,
			"refundType":             "auto_detected",
			"refundTransactionID":    refundTransactionID,
			"subscriptionTerminated": shouldTerminate,
		}).Info("Processed refund successfully")

		return nil
	}); err != nil {
		return err
	}
	if refundRepairAlert != nil {
		if err := recordLedgerRepairAlert(ctx, s.NotificationService, s.DB, s.now(), *refundRepairAlert); err != nil {
			return fmt.Errorf("record CCBill refund ledger repair alert: %w", err)
		}
	}

	return nil
}

func (s *CCBillWebhookService) handleVoid(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill void notification")

	var data CCBillVoidEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	pSubscriptionID := data.SubscriptionID
	voidAmountStr := data.Amount
	voidTransactionID := data.TransactionID // Use TransactionID as the void transaction ID
	voidAmountCents, err := parseCCBillPositiveAmountCents(voidAmountStr, "void amount", "amount")
	if err != nil {
		return err
	}
	if _, err := requireCCBillCurrency(data.CurrencyCode, "currencyCode"); err != nil {
		return err
	}
	var voidedSubscriptionID *uuid.UUID

	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		subService := subscriptions.NewSubscriptionService(txdb, priceService, productService, nil, s.Clock)
		paymentService := payments.NewPaymentService(txdb)

		// Try to find subscription by rail subscription ID
		// Note: For voids, the subscription might not exist yet since the transaction was voided
		sub, err := subService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), pSubscriptionID)
		if err != nil {
			if db.IsNotFound(err) {
				// #675: the void may race the NewSaleSuccess webhook — retryable
				// error (CCBill redelivers on non-2xx) so redelivery applies the
				// reversal once the sale materializes; a plain ACK let the later
				// sale create entitlements for an already-voided charge.
				log.WithContext(ctx).WithFields(log.Fields{
					"rail_subscription_id": pSubscriptionID,
					"void_amount_cents":    voidAmountCents,
					"void_transaction_id":  voidTransactionID,
				}).Warn("Void event for unknown subscription; retrying until the sale materializes")
				return fmt.Errorf("subscription %q not found for CCBill void: %w", pSubscriptionID, err)
			}
			return fmt.Errorf("failed to get subscription: %w", err)
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscriptionID":        sub.ID,
			"userID":                sub.CustomerID.String(),
			"voidAmountCents":       voidAmountCents,
			"voidTransactionID":     voidTransactionID,
			"originalTransactionID": voidTransactionID,
		}).Info("Void event for existing subscription")

		originalPayment, paymentErr := paymentService.GetByTransactionID(ctx, models.RailCCBill, voidTransactionID)
		if paymentErr != nil && !db.IsNotFound(paymentErr) {
			return fmt.Errorf("lookup original payment for void: %w", paymentErr)
		}
		if originalPayment != nil {
			reversalID := "void:" + voidTransactionID
			if existingVoid, lookupErr := paymentService.GetByTransactionID(ctx, models.RailCCBill, reversalID); lookupErr == nil && existingVoid != nil {
				log.WithContext(ctx).WithField("void_transaction_id", reversalID).Info("Void reversal already recorded")
			} else if lookupErr != nil && !db.IsNotFound(lookupErr) {
				return fmt.Errorf("lookup existing void reversal: %w", lookupErr)
			} else {
				amount := int64(moneyutil.CentsToMicros(moneyutil.Cents(voidAmountCents)))
				if amount > originalPayment.Amount {
					amount = originalPayment.Amount
				}
				if _, refundErr := paymentService.Refund(ctx, originalPayment.ID, reversalID, int64(amount), payments.ReversalRefund); refundErr != nil {
					return fmt.Errorf("record void reversal: %w", refundErr)
				}
			}
			if originalPayment.SubscriptionID != nil {
				id := *originalPayment.SubscriptionID
				voidedSubscriptionID = &id
			}
		} else {
			log.WithContext(ctx).WithFields(log.Fields{
				"void_transaction_id": voidTransactionID,
				"subscription_id":     sub.ID,
			}).Warn("Unable to resolve original payment for CCBill void")
			return fmt.Errorf("unable to resolve original payment for CCBill void transaction %q", voidTransactionID)
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscriptionID":    sub.ID,
			"userID":            sub.CustomerID.String(),
			"voidAmountCents":   voidAmountCents,
			"voidTransactionID": voidTransactionID,
		}).Info("Processed void successfully")

		return nil
	}); err != nil {
		return err
	}
	if voidedSubscriptionID != nil && s.SubscriptionLifecycleService != nil {
		rail := models.RailCCBill
		reason := "CCBill void processed"
		if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: voidedSubscriptionID,
			Rail:           &rail,
			CancelType:     models.CancelTypeMerchant,
			CancelFeedback: &reason,
			RevokeAccess:   true,
		}); err != nil {
			return fmt.Errorf("cancel membership after CCBill void: %w", err)
		}
	}

	return nil
}

func (s *CCBillWebhookService) handleChargeback(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Warn("Processing CCBill chargeback notification - immediate termination required")

	var data CCBillChargebackEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	pSubscriptionID := data.SubscriptionID
	chargebackAmountStr := data.Amount
	chargebackTransactionID := data.TransactionID // Use TransactionID as the chargeback transaction ID
	chargebackReason := data.Reason
	chargebackAmountCents, err := parseCCBillPositiveAmountCents(chargebackAmountStr, "chargeback amount", "amount")
	if err != nil {
		return err
	}
	if _, err := requireCCBillCurrency(data.CurrencyCode, "currencyCode"); err != nil {
		return err
	}
	var ledgerErr error
	var chargebackRepairAlert *ledgerRepairAlert

	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		subService := subscriptions.NewSubscriptionService(txdb, priceService, productService, nil, s.Clock)
		entSvc := entitlements.NewEntitlementService(txdb, s.Clock)
		paymentService := payments.NewPaymentService(txdb, s.Clock)

		// Find subscription by rail subscription ID
		sub, err := subService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), pSubscriptionID)
		if err != nil {
			if db.IsNotFound(err) {
				// #675: the chargeback may race the sale webhook — retryable error
				// (CCBill redelivers on non-2xx) so redelivery terminates the
				// subscription once the sale materializes; a plain ACK left the
				// charged-back access standing.
				log.WithContext(ctx).WithFields(log.Fields{
					"rail_subscription_id":    pSubscriptionID,
					"chargeback_amount_cents": chargebackAmountCents,
				}).Error("Chargeback event for unknown subscription; retrying until the sale materializes")
				return fmt.Errorf("subscription %q not found for CCBill chargeback: %w", pSubscriptionID, err)
			}
			return fmt.Errorf("failed to get subscription: %w", err)
		}

		// No external user lookup (IdP-managed ID already on subscription)
		originalPayment, paymentErr := paymentService.GetByTransactionID(ctx, models.RailCCBill, chargebackTransactionID)
		if paymentErr != nil && !db.IsNotFound(paymentErr) {
			return fmt.Errorf("lookup original payment for CCBill chargeback: %w", paymentErr)
		}
		if originalPayment == nil {
			originalPayment, paymentErr = paymentService.GetLatestChargeBySubscriptionID(ctx, sub.ID)
			if paymentErr != nil && !db.IsNotFound(paymentErr) {
				return fmt.Errorf("lookup latest payment for CCBill chargeback: %w", paymentErr)
			}
		}
		if originalPayment != nil {
			reversalID := "chargeback:" + strings.TrimSpace(chargebackTransactionID)
			if reversalID == "chargeback:" {
				reversalID += strings.TrimSpace(pSubscriptionID)
			}
			if existingChargeback, lookupErr := paymentService.GetByTransactionID(ctx, models.RailCCBill, reversalID); lookupErr == nil && existingChargeback != nil {
				log.WithContext(ctx).WithField("chargeback_transaction_id", reversalID).Info("CCBill chargeback reversal already recorded")
			} else if lookupErr != nil && !db.IsNotFound(lookupErr) {
				return fmt.Errorf("lookup existing CCBill chargeback reversal: %w", lookupErr)
			} else {
				amount := int64(moneyutil.CentsToMicros(moneyutil.Cents(chargebackAmountCents)))
				if amount > originalPayment.Amount {
					amount = originalPayment.Amount
				}
				if _, refundErr := paymentService.Refund(ctx, originalPayment.ID, reversalID, amount, payments.ReversalChargeback); refundErr != nil {
					ledgerErr = fmt.Errorf("record CCBill chargeback reversal: %w", refundErr)
					log.WithContext(ctx).WithError(ledgerErr).WithFields(log.Fields{
						"chargeback_transaction_id": reversalID,
						"original_payment_id":       originalPayment.ID,
					}).Error("Failed to record CCBill chargeback reversal; continuing entitlement revocation")
				}
			}
		} else {
			ledgerErr = fmt.Errorf("resolve original payment for CCBill chargeback ledger reversal")
			log.WithContext(ctx).WithFields(log.Fields{
				"subscription_id":           sub.ID,
				"rail_subscription_id":      pSubscriptionID,
				"chargeback_transaction_id": chargebackTransactionID,
			}).Warn("Unable to resolve original payment for CCBill chargeback ledger reversal")
		}

		now := s.now()

		// IMMEDIATE TERMINATION - chargebacks are the most serious type of dispute
		if err := sub.ResetCurrentPeriods(); err != nil {
			return err
		}

		// Chargebacks are terminal cancellations.
		cancelType := models.CancelTypeChargeback
		sub.Status = models.StatusCancelled
		sub.CancelType = &cancelType
		sub.CancelledAt = &now
		sub.EndedAt = &now
		sub.ClearRetrySchedule()

		// Include chargeback details in feedback
		chargebackFeedback := fmt.Sprintf("CHARGEBACK: %s (Code: %s, Dispute: %s)",
			chargebackReason, "unknown", "unknown")
		sub.CancelFeedback = &chargebackFeedback

		if err := subService.Update(ctx, sub); err != nil {
			return fmt.Errorf("failed to update subscription after chargeback: %w", err)
		}

		// Immediately end entitlements for this subscription.
		if err := entSvc.RevokeSourcesForSubscription(ctx, sub.CustomerID.String(), sub.ID, models.EntitlementRevokeChargeback, models.EntitlementSourceSubscription, models.EntitlementSourceGrace); err != nil {
			log.WithContext(ctx).WithError(err).Error("failed to revoke entitlements for chargebacked subscription")
		}
		if ledgerErr != nil {
			var originalPaymentID *uuid.UUID
			if originalPayment != nil {
				id := originalPayment.ID
				originalPaymentID = &id
			}
			subscriptionID := sub.ID
			chargebackRepairAlert = &ledgerRepairAlert{
				Provider:          "ccbill",
				Operation:         "chargeback_reversal",
				TransactionID:     chargebackTransactionID,
				UserID:            sub.CustomerID.String(),
				OriginalPaymentID: originalPaymentID,
				SubscriptionID:    &subscriptionID,
				Err:               ledgerErr,
				Metadata: map[string]any{
					"rail_subscription_id":    pSubscriptionID,
					"chargeback_amount_cents": chargebackAmountCents,
					"chargeback_reason":       chargebackReason,
				},
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"user_id":                 sub.CustomerID.String(),
			"chargeback_amount_cents": chargebackAmountCents,
			"dispute_id":              "unknown",
		}).Warn("User account involved in chargeback - consider fraud review")

		// Add system alert notification for chargeback (admin notification)
		if s.NotificationService != nil {
			// User notification about account termination
			userNotification := &models.NotificationQueue{
				ID:         uuidutil.NewV7(),
				CustomerID: sub.CustomerID,
				EventType:  models.NotificationPremiumEnded,
				Data:       map[string]any{"reason": string(subscriptions.PremiumEndReasonChargeback)},
			}
			if err := s.NotificationService.CreateAndDeliver(ctx, userNotification); err != nil {
				log.WithContext(ctx).WithError(err).Error("failed to create and deliver chargeback termination notification")
			}
		}

		log.WithContext(ctx).WithFields(log.Fields{
			"subscriptionID":          sub.ID,
			"userID":                  sub.CustomerID.String(),
			"chargebackAmountCents":   chargebackAmountCents,
			"chargebackTransactionID": chargebackTransactionID,
			"chargebackReasonCode":    "unknown",
			"disputeID":               "unknown",
			"subscriptionTerminated":  true,
			"fraudFlag":               true,
		}).Error("Processed chargeback - subscription terminated immediately")

		return nil
	}); err != nil {
		return err
	}
	if chargebackRepairAlert != nil {
		if err := recordLedgerRepairAlert(ctx, s.NotificationService, s.DB, s.now(), *chargebackRepairAlert); err != nil {
			return fmt.Errorf("record CCBill chargeback ledger repair alert: %w", err)
		}
	}
	return nil
}

func (s *CCBillWebhookService) handleRenewalSuccess(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Info("Processing CCBill renewal success notification")

	var data CCBillRenewalSuccessEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	process := func(ctx context.Context) error {
		return wrapCCBillWebhookErrorForRetry(s.handleRenewalSuccessInternal(ctx, &data))
	}

	if s.DeduplicationService != nil && strings.TrimSpace(data.TransactionID) != "" {
		return s.DeduplicationService.ProcessWebhook(
			ctx,
			data.TransactionID,
			string(s.Data.EventType),
			models.RailCCBill,
			data,
			process,
		)
	}

	return process(ctx)
}

func (s *CCBillWebhookService) handleRenewalSuccessInternal(ctx context.Context, data *CCBillRenewalSuccessEvent) error {
	ccBillSubID := data.SubscriptionID
	transactionID := strings.TrimSpace(data.TransactionID)
	if transactionID == "" {
		return newBillingError(ErrorTypeValidation, "missing required field: transactionId", map[string]interface{}{"field": "transactionId"}, nil)
	}
	billedAmountStr := data.BilledAmount

	billedAmountCents, err := parseCCBillPositiveAmountCents(billedAmountStr, "billedAmount", "billedAmount")
	if err != nil {
		return err
	}

	currencyValue, err := requireCCBillCurrency(data.BilledCurrencyCode, "billedCurrencyCode")
	if err != nil {
		return err
	}

	prevSub, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), ccBillSubID)
	if err != nil {
		return fmt.Errorf("failed to get subscription for renewal: %w", err)
	}
	prevStatus := prevSub.Status
	if prevSub.Price == nil {
		return fmt.Errorf("subscription price is required for CCBill renewal amount validation")
	}
	if err := validateCCBillCurrencyMatches(currencyValue, prevSub.Price.Currency, map[string]interface{}{
		"price_id":             prevSub.Price.ID.String(),
		"rail_subscription_id": ccBillSubID,
		"subscription_id":      prevSub.ID.String(),
		"transaction_id":       transactionID,
	}); err != nil {
		return err
	}
	if err := validateCCBillBilledAmount(ctx, s, prevSub.Price.Currency, billedAmountCents, moneyutil.Micros(prevSub.Price.Amount), map[string]interface{}{
		"price_id":                     prevSub.Price.ID.String(),
		"rail_subscription_id":         ccBillSubID,
		"subscription_id":              prevSub.ID.String(),
		"subscription_status":          string(prevStatus),
		"subscription_expected_amount": prevSub.Price.Amount,
	}, log.Fields{
		"transaction_id":       transactionID,
		"rail_subscription_id": ccBillSubID,
		"subscription_id":      prevSub.ID,
	}); err != nil {
		return err
	}

	paidTermEnd, err := parseCCBillDateUsingTimestamp(data.NextRenewalDate)
	if err != nil {
		return fmt.Errorf("failed to parse nextRenewalDate '%s': %w", data.NextRenewalDate, err)
	}

	// RenewMembership now creates the Payment record internally
	if err = s.SubscriptionLifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
		Rail:                models.RailCCBill,
		RailSubscriptionID:  ccBillSubID,
		CurrentPeriodEndsAt: paidTermEnd,
		TransactionID:       transactionID,
		Amount:              int64(moneyutil.CentsToMicros(billedAmountCents)),
		AmountProvided:      true,
		Currency:            currencyValue,
		PurchasedAt:         ccbillEventPurchasedAt(data.Timestamp),
	}); err != nil {
		if subscriptions.IsTerminalTransitionBlocked(err) {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"rail_subscription_id": ccBillSubID,
				"transaction_id":       transactionID,
			}).Warn("Blocked terminal -> active transition for delayed CCBill RenewalSuccess")
			if alertErr := recordLedgerRepairAlert(ctx, s.NotificationService, s.DB, s.now(), ledgerRepairAlert{
				Provider:       string(models.RailCCBill),
				Operation:      "terminal_blocked_renewal_success",
				TransactionID:  transactionID,
				UserID:         prevSub.CustomerID.String(),
				SubscriptionID: &prevSub.ID,
				Err:            err,
				Metadata: map[string]any{
					"rail_subscription_id": ccBillSubID,
					"amount_cents":         billedAmountCents,
					"currency":             currencyValue,
					"event_type":           string(s.Data.EventType),
				},
			}); alertErr != nil {
				return fmt.Errorf("record terminal-blocked CCBill renewal success repair alert: %w", alertErr)
			}
			return nil
		}
		return fmt.Errorf("failed to renew membership: %w", err)
	}

	// Get the subscription for logging
	subscription, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), ccBillSubID)
	if err != nil {
		return fmt.Errorf("failed to get subscription for logging: %w", err)
	}

	// Note: grace window cleanup happens inside RenewMembership (before pushing the next paid window)
	// to avoid the grace tail interfering with scheduling.

	if s.MoneyService != nil {
		periodEnd := time.Time{}
		if paidTermEnd != nil {
			periodEnd = paidTermEnd.UTC()
		} else if subscription.CurrentPeriodEndsAt != nil {
			periodEnd = subscription.CurrentPeriodEndsAt.UTC()
		}
		if !periodEnd.IsZero() {
			if err := s.MoneyService.GrantSubscriptionCredits(ctx, money.GrantSubscriptionCreditsParams{
				SubscriptionID: subscription.ID,
				PeriodEnd:      periodEnd,
				Cadence:        models.CreditGrantCadencePerRenewal,
				Source:         "subscription_renewal",
			}); err != nil {
				// #675: propagate for retry — the deposit is idempotent per
				// (subscription, label, period_end); warn-and-ack lost the lot.
				return fmt.Errorf("grant renewal subscription credits (CCBill): %w", err)
			}
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscriptionID":    subscription.ID,
		"userID":            subscription.CustomerID.String(),
		"billedAmountCents": billedAmountCents,
		"transactionID":     transactionID,
	}).Info("Processed subscription renewal successfully")

	return nil
}

func (s *CCBillWebhookService) handleRenewalFailure(ctx context.Context) error {
	log.WithContext(ctx).
		WithField("eventType", s.Data.EventType).
		Warn("Processing CCBill renewal failure notification")

	var data CCBillRenewalFailureEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	ccBillSubID := data.SubscriptionID
	transactionID := data.TransactionID

	nextRetryAt, err := parseCCBillDateUsingTimestamp(data.NextRetryDate)
	if err != nil {
		return fmt.Errorf("failed to parse nextRetryDate '%s': %w", data.NextRetryDate, err)
	}

	var subForLogs *models.Subscription
	ignoredRenewalFailure := false

	if err := s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := db.NewWithPgxTx(tx)
		priceService := catalog.NewPriceService(txdb)
		productService := catalog.NewProductService(txdb)
		subService := subscriptions.NewSubscriptionService(txdb, priceService, productService, nil, s.Clock)
		entSvc := entitlements.NewEntitlementService(txdb, s.Clock)
		entSvc.SetClock(s.Clock)

		sub, err := subService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), ccBillSubID)
		if err != nil {
			return fmt.Errorf("subscription not found: %w", err)
		}
		failureRenewalAt, err := parseCCBillDateUsingTimestamp(data.RenewalDate)
		if err != nil {
			return fmt.Errorf("failed to parse renewalDate '%s': %w", data.RenewalDate, err)
		}
		if ignored, reason := shouldIgnoreCCBillRenewalFailure(sub, failureRenewalAt); ignored {
			fields := log.Fields{
				"subscription_id":      sub.ID,
				"rail_subscription_id": ccBillSubID,
				"transaction_id":       transactionID,
				"reason":               reason,
			}
			if failureRenewalAt != nil {
				fields["failure_renewal_at"] = failureRenewalAt.UTC()
			}
			if sub.CurrentPeriodStartsAt != nil {
				fields["current_period_start"] = sub.CurrentPeriodStartsAt.UTC()
			}
			log.WithContext(ctx).WithFields(fields).Warn("ignoring CCBill RenewalFailure")
			subForLogs = sub
			ignoredRenewalFailure = true
			return nil
		}

		paidTermEnd := sub.CurrentPeriodEndsAt
		nextRetryAt = capCCBillRetryAt(nextRetryAt, paidTermEnd)

		// Mark subscription as past_due using CCBill's retry schedule.
		sub.Status = models.StatusPastDue
		sub.NextRetryAt = nextRetryAt
		sub.LastRetryAt = nil
		sub.RetryAttempts = nil
		sub.GraceEndsAt = nil

		// For CCBill, retry behavior is dictated by the rail. nextRetryAt only
		// dates the grace_ends_at PACING marker (when a stalled row parks to
		// `unknown`); #691 removed the grace-window appends — the auto-renew sub's
		// STANDING entitlement window keeps access intact through CCBill's dunning.
		if paidTermEnd != nil && nextRetryAt != nil && nextRetryAt.After(*paidTermEnd) {
			candidate := nextRetryAt.UTC()
			sub.GraceEndsAt = &candidate
		}

		if err := subService.Update(ctx, sub); err != nil {
			return fmt.Errorf("failed to update subscription during renewal failure: %w", err)
		}

		// #733: durably record the declined rebill as a failed payments row
		// (CCBill rebilling is provider-managed; this webhook is the only
		// per-attempt decline visibility).
		if price, perr := priceService.GetByID(ctx, sub.PriceID); perr == nil && strings.TrimSpace(transactionID) != "" {
			kind := payments.AttemptRenewal
			subID := sub.ID
			failed := &models.Payment{
				ID:             uuidutil.NewV7(),
				CustomerID:     sub.CustomerID,
				PriceID:        price.ID,
				SubscriptionID: &subID,
				Rail:           models.RailCCBill,
				TransactionID:  strings.TrimSpace(transactionID),
				Amount:         price.Amount,
				ListAmount:     price.Amount,
				Currency:       price.Currency,
				Status:         payments.PaymentStatusFailedValue,
				AttemptKind:    &kind,
				MoneyMovement:  models.MoneyMovementNone, // or#827: a decline moved nothing.
				PurchasedAt:    s.now(),
				CreatedAt:      s.now(),
			}
			if code := strings.TrimSpace(data.FailureCode); code != "" {
				reason := payments.NormalizeFailureReason(string(models.RailCCBill), code)
				failed.FailureCode = &code
				failed.FailureReason = &reason
			}
			if _, err := payments.NewPaymentService(txdb, s.Clock).CreateIfNotExists(ctx, failed); err != nil {
				log.WithContext(ctx).WithError(err).WithField("transaction_id", transactionID).Error("failed to record CCBill renewal decline payment row")
			}
		}

		subForLogs = sub
		return nil
	}); err != nil {
		return err
	}
	if ignoredRenewalFailure {
		return nil
	}

	// Reload subscription for logging (ensures relations are present if service loads them).
	subscription, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), ccBillSubID)
	if err != nil {
		// Fall back to the version we updated inside the transaction.
		subscription = subForLogs
	}
	if subscription == nil {
		return fmt.Errorf("subscription not found for logging: %s", ccBillSubID)
	}

	if s.NotificationService != nil {
		notification := &models.NotificationQueue{
			ID:         uuidutil.NewV7(),
			CustomerID: subscription.CustomerID,
			EventType:  models.NotificationPaymentMethodFailed,
			Data: map[string]any{
				"rail":                 string(models.RailCCBill),
				"rail_subscription_id": ccBillSubID,
				"transaction_id":       transactionID,
				"failure_code":         data.FailureCode,
				"failure_reason":       data.FailureReason,
			},
		}
		if err := s.NotificationService.CreateAndDeliver(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id":      subscription.ID,
				"rail_subscription_id": ccBillSubID,
				"transaction_id":       transactionID,
			}).Error("failed to create and deliver CCBill renewal failure notification")
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscriptionID":     subscription.ID,
		"userID":             subscription.CustomerID.String(),
		"railSubscriptionID": ccBillSubID,
		"failureCode":        data.FailureCode,
		"failureReason":      data.FailureReason,
		"nextRetryAt":        subscription.NextRetryAt,
		"paidTermEnd":        subscription.CurrentPeriodEndsAt,
		"graceEndsAt":        subscription.GraceEndsAt,
	}).Info("Handled renewal failure")

	return nil
}

func (s *CCBillWebhookService) handleCancel(ctx context.Context) error {
	var data CCBillCancellationEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	ccBillSubID := data.SubscriptionID
	if ccBillSubID == "" {
		return fmt.Errorf("missing required field: subscriptionId")
	}

	// Get the subscription to determine cancel type and for logging
	subscription, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), ccBillSubID)
	if err != nil {
		if db.IsNotFound(err) {
			return fmt.Errorf("subscription not found for rail subscription ID: %s", ccBillSubID)
		}
		return fmt.Errorf("failed to get subscription: %w", err)
	}

	// #696: the provider-confirmed cancel arriving AFTER our own local cancel
	// (user/admin cancel + ccbill_cancel intent) is a NO-OP — the local row
	// already carries the terminal state, cancel provenance and the #691
	// runway closure. Re-applying would overwrite cancel_type/cancelled_at,
	// re-revoke and re-notify (finding churn), never converge anything new.
	if subscription.Status == models.StatusCancelled {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":      subscription.ID,
			"rail_subscription_id": ccBillSubID,
			"cancel_source":        data.Source,
		}).Info("CCBill cancellation webhook for already-cancelled subscription; no-op")
		return nil
	}

	// Determine cancel type based on source
	var cancelType models.CancelType
	if data.Source == "failedRB" {
		cancelType = models.CancelTypeExpired
	} else {
		cancelType = models.CancelTypeMerchant
	}

	// Use SubscriptionLifecycleService to cancel membership
	rail := models.RailCCBill
	revokeAccess := data.Source == "failedRB"
	if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
		SubscriptionID:     &subscription.ID,
		Rail:               &rail,
		RailSubscriptionID: &ccBillSubID,
		CancelType:         cancelType,
		CancelFeedback:     &data.Reason,
		RevokeAccess:       revokeAccess,
	}); err != nil {
		return fmt.Errorf("failed to cancel membership: %w", err)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscriptionID":     subscription.ID,
		"userID":             subscription.CustomerID.String(),
		"railSubscriptionID": ccBillSubID,
		"cancelReason":       data.Reason,
		"cancelSource":       data.Source,
	}).Info("Cancelled subscription successfully")

	return nil
}

func (s *CCBillWebhookService) handleExpiration(ctx context.Context) error {
	var data CCBillExpirationEvent
	if err := json.Unmarshal(s.Data.EventBody, &data); err != nil {
		return err
	}

	ccBillSubID := data.SubscriptionID

	// Get the subscription for logging
	subscription, err := s.SubscriptionService.GetByRailSubscriptionID(ctx, string(models.RailCCBill), ccBillSubID)
	if err != nil {
		if db.IsNotFound(err) {
			return fmt.Errorf("subscription not found for rail subscription ID: %s", ccBillSubID)
		}
		return fmt.Errorf("failed to get subscription: %w", err)
	}
	if subscription.Status == models.StatusActive && subscription.CurrentPeriodEndsAt != nil && subscription.CurrentPeriodEndsAt.After(s.now()) {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":        subscription.ID,
			"rail_subscription_id":   ccBillSubID,
			"current_period_ends_at": subscription.CurrentPeriodEndsAt,
		}).Warn("Ignoring CCBill expiration for active paid-through subscription")
		return nil
	}

	// Use SubscriptionLifecycleService to expire membership
	if err := s.SubscriptionLifecycleService.ExpireMembership(ctx, subscription.ID); err != nil {
		return fmt.Errorf("failed to expire membership: %w", err)
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"subscriptionID":     subscription.ID,
		"userID":             subscription.CustomerID.String(),
		"railSubscriptionID": ccBillSubID,
	}).Info("Expired subscription successfully")

	return nil
}
