package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/timeutil"

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
	SubscriptionService          *subscriptions.SubscriptionService
	PaymentService               *payments.PaymentService
	MoneyService                 *money.MoneyService
	DeduplicationService         *DeduplicationService
	NotificationService          *subscriptions.NotificationService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	// ConvergeEnqueuer (#684): subscription-state events are wake-up signals —
	// the handler marks the subscription dirty and the coalesced River job
	// fetches provider truth and converges via the #665 decider.
	ConvergeEnqueuer SubscriptionConvergeEnqueuer
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

func nmiAmountMatchesExpected(amountCents moneyutil.Cents, expectedAmountMicros moneyutil.Micros) bool {
	if expectedAmountMicros <= 0 {
		return true
	}
	expectedAmountCents, err := moneyutil.MicrosToCentsExact(expectedAmountMicros)
	if err != nil {
		return false
	}
	tolerance := expectedAmountCents * 2 / 100 // 2% tolerance, integer-only (#818)
	return amountCents >= expectedAmountCents-tolerance && amountCents <= expectedAmountCents+tolerance
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

func transactionAmountCents(body *NMITransactionEventBody) (moneyutil.Cents, error) {
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
	// #684: subscription-state events are WAKE-UP SIGNALS. The handler parses
	// ONLY the dirty subscription's identity, then enqueues the coalesced
	// fetch-and-converge job; FETCHED provider truth (query.php + v5 GET),
	// never the payload, decides the transition.
	case EventTypeNMIAddSubscription, EventTypeNMIUpdateSubscription, EventTypeNMIDeleteSubscription:
		return s.markDirtyFromRecurringEvent(ctx)
	case EventTypeNMITransactionSuccess, EventTypeNMITransactionFailure:
		return s.markDirtyFromTransactionEvent(ctx)

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

// markDirtyFromRecurringEvent parses only the subscription id from a
// recurring.subscription.* event and enqueues fetch-and-converge.
func (s *NMIWebhookService) markDirtyFromRecurringEvent(ctx context.Context) error {
	body, err := s.parseRecurringEventBody()
	if err != nil {
		return err
	}
	nmiSubID := body.SubscriptionID.Trimmed()
	if nmiSubID == "" {
		// Redelivery resends the same bytes; a payload with no identity can
		// never converge — terminal.
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Missing subscription ID", map[string]interface{}{
			"event_type": s.Data.EventType,
		}, nil))
	}
	return markSubscriptionDirty(ctx, s.ConvergeEnqueuer, s.Rail, nmiSubID, s.Data.EventType, 0)
}

// markDirtyFromTransactionEvent parses only the subscription/order reference
// from a transaction.sale.* event and enqueues fetch-and-converge. Events
// without any reference (one-off sales) carry no subscription state — the
// checkout path owns those.
func (s *NMIWebhookService) markDirtyFromTransactionEvent(ctx context.Context) error {
	body, err := s.parseTransactionEventBody()
	if err != nil {
		return err
	}
	reference := transactionSubscriptionID(body)
	if reference == "" {
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Missing subscription reference", map[string]interface{}{
			"event_type": s.Data.EventType,
		}, nil))
	}
	return markSubscriptionDirty(ctx, s.ConvergeEnqueuer, s.Rail, reference, s.Data.EventType, 0)
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

func parseNMIChargebackAmountCents(raw string) (moneyutil.Cents, error) {
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
		amountCents moneyutil.Cents
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
		Rail:        string(rail),
		AmountCents: int64(amountCents),
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
		return nil
	}

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

		var (
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
				} else if lookupErr != nil && !db.IsNotFound(lookupErr) {
					reconcileErrors++
					cbMetadata["chargeback_payment_status"] = "failed"
					cbMetadata["chargeback_payment_error"] = lookupErr.Error()
					entryLedgerErr = fmt.Errorf("lookup NMI chargeback reversal: %w", lookupErr)
					if ledgerErr == nil {
						ledgerErr = entryLedgerErr
						log.WithContext(ctx).WithError(ledgerErr).Error("Failed to lookup NMI chargeback reversal; continuing entitlement revocation")
					}
				} else {
					amountCents := moneyutil.Cents(match.AmountCents)
					if parsedAmount, parseErr := parseNMIChargebackAmountCents(cb.Amount); parseErr == nil && parsedAmount > 0 {
						amountCents = parsedAmount
					}
					if amountCents > moneyutil.Cents(match.AmountCents) {
						amountCents = moneyutil.Cents(match.AmountCents)
					}
					if _, refundErr := s.PaymentService.Refund(ctx, match.PaymentID, chargebackTransactionID, int64(moneyutil.CentsToMicros(moneyutil.Cents(amountCents))), payments.ReversalChargeback); refundErr != nil {
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
		// #675: never downgrade a refund to a 0-amount no-op — durable alert,
		// then terminal (redelivery resends the same unparseable bytes).
		if alertErr := recordLedgerRepairAlert(ctx, s.NotificationService, s.DB, s.now(), ledgerRepairAlert{
			Provider:      s.Rail,
			Operation:     "refund_amount_parse_failed",
			TransactionID: txnID,
			Err:           err,
			Metadata: map[string]any{
				"original_transaction_id": originalTxnID,
				"rail_subscription_id":    nmiSubID,
			},
		}); alertErr != nil {
			return fmt.Errorf("record NMI refund amount parse repair alert: %w", alertErr)
		}
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Invalid refund amount", map[string]interface{}{
			"transaction_id": txnID,
		}, err))
	}
	if refundAmountCents < 0 {
		refundAmountCents = -refundAmountCents
	}

	if _, err := s.normalizedRail(); err != nil {
		return err
	}

	// Try to find subscription - refund may be for a subscription payment
	var subscription *models.Subscription
	if nmiSubID != "" {
		subscription, err = s.SubscriptionService.GetByRailSubscriptionID(ctx, s.Rail, nmiSubID)
		if err != nil && !db.IsNotFound(err) {
			log.WithContext(ctx).WithError(err).WithField("rail_subscription_id", nmiSubID).
				Warn("Failed to look up subscription for refund (by rail_subscription_id)")
		} else if db.IsNotFound(err) {
			log.WithContext(ctx).WithField("rail_subscription_id", nmiSubID).
				Warn("Received refund for unknown subscription (by rail_subscription_id); continuing without lifecycle actions")
		}
	}

	// #675: no local subscription (one-time purchase, or refund raced the
	// sale) — resolve by transaction id and reverse, mirroring Stripe's
	// one-off path. Previously this ACKed with zero effect.
	if subscription == nil {
		return s.handleNMIOneOffRefund(ctx, txnID, originalTxnID, refundAmountCents)
	}

	// Determine if we should terminate subscription based on refund amount.
	// Missing original references are not safe to complete silently because we cannot
	// link the refund to the ledger entry that funded entitlement.
	shouldTerminate := false
	if subscription != nil && subscription.Price != nil && subscription.Price.Amount > 0 && originalTxnID != "" {
		refundPercentage := (int64(moneyutil.CentsToMicros(moneyutil.Cents(refundAmountCents))) * 100) / subscription.Price.Amount
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
		case lookupErr != nil && !db.IsNotFound(lookupErr):
			log.WithContext(ctx).WithError(lookupErr).WithField("refund_transaction_id", txnID).
				Warn("Failed to check existing refund payment by transaction ID")
			return fmt.Errorf("check existing refund payment: %w", lookupErr)
		default:
			var originalPayment *models.Payment
			var originalLookupErr error

			if originalTxnID != "" && originalTxnID != txnID {
				originalPayment, originalLookupErr = s.PaymentService.GetByTransactionID(ctx, rail, originalTxnID)
				if originalLookupErr != nil && !db.IsNotFound(originalLookupErr) {
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
				if _, refundErr := s.PaymentService.Refund(ctx, originalPayment.ID, txnID, int64(moneyutil.CentsToMicros(moneyutil.Cents(refundAmountCents))), payments.ReversalRefund); refundErr != nil {
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

	if shouldTerminate && subscription != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":         subscription.ID,
			"refund_amount_cents":     refundAmountCents,
			"subscription_fee_micros": subscription.Price.Amount,
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
			}
		}
	}

	log.WithContext(ctx).WithFields(log.Fields{
		"transaction_id":          txnID,
		"refund_amount_cents":     refundAmountCents,
		"subscription_terminated": shouldTerminate,
	}).Info("NMI refund processed")

	return nil
}

// handleNMIOneOffRefund reverses a refund that has no local subscription
// (dashboard refunds of one-time purchases) — mirrors the Stripe one-off path:
// resolve the original payment by transaction id, record the negative payment,
// and revoke what the payment funded once fully refunded (#675).
func (s *NMIWebhookService) handleNMIOneOffRefund(ctx context.Context, txnID, originalTxnID string, refundAmountCents moneyutil.Cents) error {
	if s.PaymentService == nil {
		return fmt.Errorf("payment service is required for NMI refund")
	}
	if txnID == "" {
		return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Missing refund transaction ID", map[string]interface{}{}, nil))
	}
	rail := models.Rail(s.Rail)
	var original *models.Payment
	if existing, err := s.PaymentService.GetByTransactionID(ctx, rail, txnID); err == nil && existing != nil {
		if existing.RefundedPaymentID == nil {
			return nil // already recorded, not as a reversal — nothing to revoke
		}
		original, err = s.PaymentService.GetByID(ctx, *existing.RefundedPaymentID)
		if err != nil {
			return fmt.Errorf("lookup original NMI payment for existing refund: %w", err)
		}
	} else if err != nil && !db.IsNotFound(err) {
		return fmt.Errorf("lookup NMI refund payment: %w", err)
	}
	if original == nil {
		if originalTxnID == "" || originalTxnID == txnID {
			// Retryable: redelivery wins once the sale materializes.
			return fmt.Errorf("unable to resolve original payment for NMI refund transaction %q", txnID)
		}
		var err error
		original, err = s.PaymentService.GetByTransactionID(ctx, rail, originalTxnID)
		if err != nil {
			if db.IsNotFound(err) {
				return fmt.Errorf("original payment %q not found for NMI refund %q", originalTxnID, txnID)
			}
			return fmt.Errorf("resolve original payment for NMI refund: %w", err)
		}
		if refundAmountCents <= 0 {
			return MarkWebhookErrorNonRetryable(newNMIBillingError(ErrorTypeNMIValidation, "Non-positive refund amount", map[string]interface{}{
				"transaction_id": txnID,
			}, nil))
		}
		if _, err := s.PaymentService.Refund(ctx, original.ID, txnID, int64(moneyutil.CentsToMicros(moneyutil.Cents(refundAmountCents))), payments.ReversalRefund); err != nil {
			return fmt.Errorf("record NMI refund: %w", err)
		}
	}
	refundedTotal, err := s.PaymentService.GetRefundTotalByPaymentID(ctx, original.ID)
	if err != nil {
		return fmt.Errorf("calculate NMI refund total: %w", err)
	}
	if refundedTotal < original.Amount {
		return nil
	}
	if original.SubscriptionID != nil && s.SubscriptionLifecycleService != nil {
		reason := "NMI refund processed"
		if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: original.SubscriptionID,
			Rail:           &rail,
			CancelType:     models.CancelTypeMerchant,
			CancelFeedback: &reason,
			RevokeAccess:   true,
		}); err != nil {
			return fmt.Errorf("cancel subscription after NMI refund: %w", err)
		}
	} else if original.SubscriptionID == nil && s.DB != nil {
		entSvc := entitlements.NewEntitlementService(s.DB, s.Clock)
		if err := entSvc.EndActiveByPayment(ctx, original.ID, models.EntitlementRevokeRefund); err != nil {
			return fmt.Errorf("revoke one-off entitlements after NMI refund: %w", err)
		}
		paSvc := productaccess.NewService(s.DB, s.Clock)
		if _, err := paSvc.RevokeProductAccessByPayment(ctx, original.ID, models.ProductAccessRevokeRefund); err != nil {
			return fmt.Errorf("revoke product access after NMI refund: %w", err)
		}
	}
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

	if _, err := s.normalizedRail(); err != nil {
		return err
	}
	var voidedSubscriptionID *uuid.UUID
	if s.PaymentService != nil && txnID != "" {
		originalPayment, paymentErr := s.PaymentService.GetByTransactionID(ctx, models.Rail(s.Rail), txnID)
		if paymentErr != nil && !db.IsNotFound(paymentErr) {
			return fmt.Errorf("lookup original payment for void: %w", paymentErr)
		}
		if originalPayment != nil {
			reversalID := "void:" + txnID
			if existingVoid, lookupErr := s.PaymentService.GetByTransactionID(ctx, models.Rail(s.Rail), reversalID); lookupErr == nil && existingVoid != nil {
				log.WithContext(ctx).WithField("void_transaction_id", reversalID).Info("NMI void reversal already recorded")
			} else if lookupErr != nil && !db.IsNotFound(lookupErr) {
				return fmt.Errorf("lookup existing void reversal: %w", lookupErr)
			} else {
				amount := originalPayment.Amount
				if amount <= 0 {
					amount = originalPayment.ListAmount
				}
				if amount <= 0 {
					return MarkWebhookErrorNonRetryable(fmt.Errorf("cannot void payment with non-positive amount"))
				}
				if _, refundErr := s.PaymentService.Refund(ctx, originalPayment.ID, reversalID, amount, payments.ReversalRefund); refundErr != nil {
					return fmt.Errorf("record void reversal: %w", refundErr)
				}
			}
			if originalPayment.SubscriptionID != nil {
				id := *originalPayment.SubscriptionID
				voidedSubscriptionID = &id
			}
		} else {
			// #675: void may race the sale webhook — retryable error (NMI
			// redelivers on non-2xx) so redelivery wins once the sale lands;
			// a plain ACK would leave the reversed charge invisible.
			log.WithContext(ctx).WithField("transaction_id", txnID).Warn("Unable to resolve original payment for NMI void")
			return fmt.Errorf("unable to resolve original payment for NMI void transaction %q", txnID)
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

	log.WithContext(ctx).WithField("transaction_id", txnID).Info("NMI void failure logged")
	return nil
}
