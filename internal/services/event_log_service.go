package services

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"net/url"
	"os"
	"path/filepath"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/spool"
	log "github.com/sirupsen/logrus"
)

// EventLogService handles logging billing events to ClickHouse
type EventLogService struct {
	clickhouseConn driver.Conn
	config         *config.ClickHouseConfig
	spool          *spool.Spool
	Clock          clockwork.Clock
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *EventLogService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// NewEventLogService creates a new event log service
func NewEventLogService(cfg *config.ClickHouseConfig) (*EventLogService, error) {
	// Feature gate: presence of HTTPAddr indicates intent to use CH
	if cfg == nil || cfg.HTTPAddr == "" {
		log.Warn("ClickHouse HTTPAddr not configured - billing events will not be logged")
		sp, _ := spool.New("")
		svc := &EventLogService{spool: sp}
		svc.startBackgroundFlush()
		return svc, nil
	}

	// Try connect; if it fails, return service with nil conn so it can retry later
	conn, err := initClickHouseConnection(cfg)
	if err != nil {
		log.WithError(err).Warn("ClickHouse unavailable at startup; will retry on use")
		sp, _ := spool.New("")
		svc := &EventLogService{clickhouseConn: nil, config: cfg, spool: sp}
		svc.startBackgroundFlush()
		return svc, nil
	}

	sp, _ := spool.New("")
	svc := &EventLogService{clickhouseConn: conn, config: cfg, spool: sp}
	svc.startBackgroundFlush()
	return svc, nil
}

// ensureConn lazily (re)establishes the ClickHouse connection
func (s *EventLogService) ensureConn(ctx context.Context) error {
	if s.config == nil || s.config.HTTPAddr == "" {
		return fmt.Errorf("ClickHouse HTTPAddr not configured")
	}
	if s.clickhouseConn != nil {
		// Quick ping; if it fails, reopen
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := s.clickhouseConn.Ping(pingCtx); err == nil {
			return nil
		}
		_ = s.clickhouseConn.Close()
		s.clickhouseConn = nil
	}
	conn, err := initClickHouseConnection(s.config)
	if err != nil {
		return err
	}
	s.clickhouseConn = conn
	return nil
}

// Ready checks connectivity to ClickHouse and establishes a connection if needed.
func (s *EventLogService) Ready(ctx context.Context) error {
	// Use a short timeout if the provided context has none
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return s.ensureConn(tctx)
	}
	return s.ensureConn(ctx)
}

// SubscriptionEventExists checks if a subscription event of the given type already exists.
// Used to avoid emitting duplicate lifecycle events when multiple webhooks report the same state change.
func (s *EventLogService) SubscriptionEventExists(ctx context.Context, subscriptionID uuid.UUID, eventType string) (bool, error) {
	if err := s.ensureConn(ctx); err != nil {
		return false, err
	}

	query := `SELECT 1 FROM subscription_events WHERE subscription_id = ? AND event_type = ? LIMIT 1`
	rows, err := s.clickhouseConn.Query(ctx, query, subscriptionID, eventType)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	hasRow := rows.Next()
	if err := rows.Err(); err != nil {
		return false, err
	}
	return hasRow, nil
}

func (s *EventLogService) startBackgroundFlush() {
	if s.spool == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = s.flushOnce(ctx, 200)
			cancel()
		}
	}()
}

func (s *EventLogService) flushOnce(ctx context.Context, limit int) error {
	if s.spool == nil {
		return nil
	}
	if err := s.ensureConn(ctx); err != nil {
		return err
	}
	paths, err := s.spool.List(limit)
	if err != nil {
		return err
	}
	// Buckets by kind
	type fileRec struct{ path string }
	var subs []SubscriptionEventData
	var subsFiles []fileRec
	var pays []PaymentEventData
	var paysFiles []fileRec
	var acus []ACUEventData
	var acusFiles []fileRec
	var chgs []ChargebackEventData
	var chgsFiles []fileRec
	var transAsPayments []PaymentEventData
	var transFiles []fileRec

	dead := func(p string) {
		_ = os.MkdirAll(filepath.Join(s.spool.Dir(), "dead-letter"), 0o755)
		_ = os.Rename(p, filepath.Join(s.spool.Dir(), "dead-letter", filepath.Base(p)))
	}

	for _, p := range paths {
		rec, _, err := s.spool.Read(p)
		if err != nil {
			log.WithError(err).Warnf("read spool %s", filepath.Base(p))
			dead(p)
			continue
		}
		switch rec.Kind {
		case "subscription":
			var d SubscriptionEventData
			if err := json.Unmarshal(rec.Data, &d); err != nil {
				log.WithError(err).Warn("decode subscription")
				dead(p)
				continue
			}
			if d.EventID == uuid.Nil {
				d.EventID = uuid.New()
			}
			if d.Timestamp.IsZero() {
				d.Timestamp = s.now().UTC()
			}
			subs = append(subs, d)
			subsFiles = append(subsFiles, fileRec{p})
		case "payment":
			var d PaymentEventData
			if err := json.Unmarshal(rec.Data, &d); err != nil {
				log.WithError(err).Warn("decode payment")
				dead(p)
				continue
			}
			if d.EventID == uuid.Nil {
				d.EventID = uuid.New()
			}
			if d.Timestamp.IsZero() {
				d.Timestamp = s.now().UTC()
			}
			if d.Currency == "" {
				d.Currency = "usd"
			}
			pays = append(pays, d)
			paysFiles = append(paysFiles, fileRec{p})
		case "transaction":
			var d TransactionEventData
			if err := json.Unmarshal(rec.Data, &d); err != nil {
				log.WithError(err).Warn("decode transaction")
				dead(p)
				continue
			}
			if d.EventID == uuid.Nil {
				d.EventID = uuid.New()
			}
			if d.Timestamp.IsZero() {
				d.Timestamp = s.now().UTC()
			}
			if d.Currency == "" {
				d.Currency = "usd"
			}
			// Map to PaymentEventData as in LogTransactionEvent
			userID := ""
			if d.UserID != nil {
				userID = *d.UserID
			}
			transAsPayments = append(transAsPayments, PaymentEventData{
				EventID:                d.EventID,
				SubscriptionID:         d.SubscriptionID,
				UserID:                 userID,
				EventType:              d.EventType,
				Processor:              d.Processor,
				ProcessorTransactionID: d.ProcessorTransactionID,
				Amount:                 d.Amount,
				Currency:               d.Currency,
				BillingInfo:            "{}",
				WebhookSource:          "",
				Metadata:               d.Metadata,
				Timestamp:              d.Timestamp,
			})
			transFiles = append(transFiles, fileRec{p})
		case "acu":
			var d ACUEventData
			if err := json.Unmarshal(rec.Data, &d); err != nil {
				log.WithError(err).Warn("decode acu")
				dead(p)
				continue
			}
			if d.EventID == uuid.Nil {
				d.EventID = uuid.New()
			}
			if d.Timestamp.IsZero() {
				d.Timestamp = s.now().UTC()
			}
			acus = append(acus, d)
			acusFiles = append(acusFiles, fileRec{p})
		case "chargeback":
			var d ChargebackEventData
			if err := json.Unmarshal(rec.Data, &d); err != nil {
				log.WithError(err).Warn("decode chargeback")
				dead(p)
				continue
			}
			if d.EventID == uuid.Nil {
				d.EventID = uuid.New()
			}
			if d.Timestamp.IsZero() {
				d.Timestamp = s.now().UTC()
			}
			if d.Currency == "" {
				d.Currency = "usd"
			}
			chgs = append(chgs, d)
			chgsFiles = append(chgsFiles, fileRec{p})
		default:
			log.Warnf("Unknown spool kind: %s; moving to dead-letter", rec.Kind)
			dead(p)
		}
	}

	// Batch insert helpers
	removeAll := func(files []fileRec) {
		for _, f := range files {
			_ = s.spool.Remove(f.path)
		}
	}
	if len(subs) > 0 {
		if err := s.insertSubscriptionBatch(ctx, subs); err != nil {
			return err
		}
		removeAll(subsFiles)
	}
	if len(pays)+len(transAsPayments) > 0 {
		// combine
		paysAll := append(pays, transAsPayments...)
		if err := s.insertPaymentBatch(ctx, paysAll); err != nil {
			return err
		}
		removeAll(paysFiles)
		removeAll(transFiles)
	}
	if len(acus) > 0 {
		if err := s.insertACUBatch(ctx, acus); err != nil {
			return err
		}
		removeAll(acusFiles)
	}
	if len(chgs) > 0 {
		if err := s.insertChargebackBatch(ctx, chgs); err != nil {
			return err
		}
		removeAll(chgsFiles)
	}
	return nil
}

// Close closes the ClickHouse connection
func (s *EventLogService) Close() error {
	if s.clickhouseConn != nil {
		return s.clickhouseConn.Close()
	}
	return nil
}

// SubscriptionEventData represents data for subscription events
type SubscriptionEventData struct {
	EventID                 uuid.UUID  `json:"event_id"`
	SubscriptionID          uuid.UUID  `json:"subscription_id"`
	UserID                  string     `json:"user_id"`
	EventType               string     `json:"event_type"`
	Status                  string     `json:"status"`
	CancelType              string     `json:"cancel_type"`
	PriceAmount             float64    `json:"price_amount"`
	PriceCurrency           string     `json:"price_currency"`
	BillingCycleDays        uint32     `json:"billing_cycle_days"`
	ProductID               *uuid.UUID `json:"product_id,omitempty"`
	PriceID                 *uuid.UUID `json:"price_id,omitempty"`
	Processor               string     `json:"processor"`
	ProcessorSubscriptionID *string    `json:"processor_subscription_id,omitempty"`
	ProcessorTransactionID  *string    `json:"processor_transaction_id,omitempty"`
	Metadata                string     `json:"metadata"`
	Timestamp               time.Time  `json:"timestamp"`
}

// PaymentEventType defines standardized event types for payment logging.
// All processors (CCBill, NMI/Mobius, Solana) should use these constants
// to ensure consistent event type values across the system.
type PaymentEventType = string

const (
	// Payment transaction events
	PaymentEventChargeSuccess PaymentEventType = "charge_success"
	PaymentEventChargeFailure PaymentEventType = "charge_failure"

	// Refund events
	PaymentEventRefund        PaymentEventType = "refund"
	PaymentEventRefundFailure PaymentEventType = "refund_failure"

	// Void events
	PaymentEventVoid        PaymentEventType = "void"
	PaymentEventVoidFailure PaymentEventType = "void_failure"

	// Chargeback events
	PaymentEventChargeback     PaymentEventType = "chargeback"
	PaymentEventBatchProcessed PaymentEventType = "batch_processed"

	// Subscription lifecycle events (for ClickHouse logging)
	PaymentEventSubscriptionCreated     PaymentEventType = "subscription_created"
	PaymentEventSubscriptionCancelled   PaymentEventType = "subscription_cancelled"
	PaymentEventSubscriptionExpired     PaymentEventType = "subscription_expired"
	PaymentEventSubscriptionReactivated PaymentEventType = "subscription_reactivated"
	PaymentEventSubscriptionPastDue     PaymentEventType = "subscription_past_due"

	// Customer/billing info events
	PaymentEventBillingDateChanged  PaymentEventType = "billing_date_changed"
	PaymentEventCustomerDataUpdated PaymentEventType = "customer_data_updated"

	// Unknown/fallback
	PaymentEventUnknown PaymentEventType = "unknown"
)

// PaymentEventData represents data for payment events
type PaymentEventData struct {
	EventID                uuid.UUID  `json:"event_id"`
	SubscriptionID         *uuid.UUID `json:"subscription_id,omitempty"`
	UserID                 string     `json:"user_id"`
	EventType              string     `json:"event_type"`
	Processor              string     `json:"processor"`
	ProcessorTransactionID *string    `json:"processor_transaction_id,omitempty"`
	Amount                 *float64   `json:"amount,omitempty"`
	Currency               string     `json:"currency"`
	BillingInfo            string     `json:"billing_info"`
	WebhookSource          string     `json:"webhook_source"`
	Metadata               string     `json:"metadata"`
	Timestamp              time.Time  `json:"timestamp"`
}

// TransactionEventData represents data for transaction events
type TransactionEventData struct {
	EventID                uuid.UUID  `json:"event_id"`
	TransactionID          string     `json:"transaction_id"`
	SubscriptionID         *uuid.UUID `json:"subscription_id,omitempty"`
	UserID                 *string    `json:"user_id,omitempty"`
	EventType              string     `json:"event_type"`
	Processor              string     `json:"processor"`
	ProcessorTransactionID *string    `json:"processor_transaction_id,omitempty"`
	Amount                 *float64   `json:"amount,omitempty"`
	Currency               string     `json:"currency"`
	Status                 string     `json:"status"`
	Metadata               string     `json:"metadata"`
	Timestamp              time.Time  `json:"timestamp"`
}

// ACUEventData represents data for Automatic Card Updater events
type ACUEventData struct {
	EventID                 uuid.UUID  `json:"event_id"`
	SubscriptionID          *uuid.UUID `json:"subscription_id,omitempty"`
	UserID                  *string    `json:"user_id,omitempty"`
	EventType               string     `json:"event_type"`
	Processor               string     `json:"processor"`
	ProcessorSubscriptionID *string    `json:"processor_subscription_id,omitempty"`
	CardInfo                string     `json:"card_info"`
	UpdateStatus            string     `json:"update_status"`
	RequiresAction          bool       `json:"requires_action"`
	Reason                  string     `json:"reason"`
	Metadata                string     `json:"metadata"`
	Timestamp               time.Time  `json:"timestamp"`
}

// ChargebackEventData represents data for chargeback events
type ChargebackEventData struct {
	EventID                uuid.UUID  `json:"event_id"`
	ChargebackID           string     `json:"chargeback_id"`
	BatchID                string     `json:"batch_id"`
	SubscriptionID         *uuid.UUID `json:"subscription_id,omitempty"`
	UserID                 *string    `json:"user_id,omitempty"`
	EventType              string     `json:"event_type"`
	Processor              string     `json:"processor"`
	ProcessorTransactionID *string    `json:"processor_transaction_id,omitempty"`
	Amount                 *float64   `json:"amount,omitempty"`
	Currency               string     `json:"currency"`
	ChargebackType         string     `json:"chargeback_type"`
	Reason                 string     `json:"reason"`
	Status                 string     `json:"status"`
	Metadata               string     `json:"metadata"`
	Timestamp              time.Time  `json:"timestamp"`
}

// Basic PII redaction for free-form strings
// - masks emails, long digit sequences, and truncates to a max length
func redactPII(s string) string {
	return redactAndTruncate(s, 1024)
}

func redactAndTruncate(s string, max int) string {
	// Simple, safe redactions (best-effort)
	// Email-like patterns
	emailRe := regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	s = emailRe.ReplaceAllString(s, "[redacted-email]")
	// PAN-like long digit sequences (13-19 digits)
	panRe := regexp.MustCompile(`\b\d{13,19}\b`)
	s = panRe.ReplaceAllString(s, "[redacted-digits]")
	if len(s) > max {
		s = s[:max]
	}
	return s
}

// LogSubscriptionEvent logs a subscription lifecycle event
func (s *EventLogService) LogSubscriptionEvent(ctx context.Context, data SubscriptionEventData) error {
	if err := s.ensureConn(ctx); err != nil {
		log.WithError(err).Warn("ClickHouse not available - spooling subscription event")
		s.enqueue("subscription", data)
		return nil
	}

	if data.EventID == uuid.Nil {
		data.EventID = uuid.New()
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}

	err := s.insertSubscription(ctx, data)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"event_id":        data.EventID,
			"subscription_id": data.SubscriptionID,
			"event_type":      data.EventType,
			"processor":       data.Processor,
		}).Error("Failed to log subscription event to ClickHouse; spooling")
		s.enqueue("subscription", data)
		return fmt.Errorf("failed to log subscription event: %w", err)
	}

	log.WithFields(log.Fields{
		"event_id":        data.EventID,
		"subscription_id": data.SubscriptionID,
		"event_type":      data.EventType,
		"processor":       data.Processor,
	}).Debug("Logged subscription event to ClickHouse")

	return nil
}

// LogPaymentEvent logs a payment transaction event
func (s *EventLogService) LogPaymentEvent(ctx context.Context, data PaymentEventData) error {
	if err := s.ensureConn(ctx); err != nil {
		log.WithError(err).Warn("ClickHouse not available - spooling payment event")
		s.enqueue("payment", data)
		return nil
	}

	if data.EventID == uuid.Nil {
		data.EventID = uuid.New()
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}
	if data.Currency == "" {
		data.Currency = "usd"
	}

	err := s.insertPayment(ctx, data)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"event_id":   data.EventID,
			"user_id":    data.UserID,
			"event_type": data.EventType,
			"processor":  data.Processor,
		}).Error("Failed to log payment event to ClickHouse; spooling")
		s.enqueue("payment", data)
		return fmt.Errorf("failed to log payment event: %w", err)
	}

	log.WithFields(log.Fields{
		"event_id":   data.EventID,
		"user_id":    data.UserID,
		"event_type": data.EventType,
		"processor":  data.Processor,
	}).Debug("Logged payment event to ClickHouse")

	return nil
}

// LogTransactionEvent logs a transaction event
func (s *EventLogService) LogTransactionEvent(ctx context.Context, data TransactionEventData) error {
	// Map transaction event into payment_events to avoid separate table.
	if err := s.ensureConn(ctx); err != nil {
		log.WithError(err).Warn("ClickHouse not available - spooling transaction event")
		s.enqueue("transaction", data)
		return nil
	}

	if data.EventID == uuid.Nil {
		data.EventID = uuid.New()
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}
	if data.Currency == "" {
		data.Currency = "usd"
	}

	evt := strings.ToLower(data.EventType)
	status := strings.ToLower(data.Status)
	paymentType := "webhook_received"
	switch {
	case strings.Contains(evt, "success") || strings.Contains(status, "complete"):
		paymentType = "charge_success"
	case strings.Contains(evt, "fail") || strings.Contains(status, "fail") || strings.Contains(status, "declin"):
		paymentType = "charge_failed"
	}

	source := "api"
	if !strings.Contains(evt, "manual") {
		source = "webhook"
	}

	if data.UserID == nil {
		log.WithFields(log.Fields{
			"transaction_id": data.TransactionID,
			"event_type":     data.EventType,
		}).Debug("Skipping mapped payment event: missing user_id")
		return nil
	}

	ped := PaymentEventData{
		EventID:                data.EventID,
		SubscriptionID:         data.SubscriptionID,
		UserID:                 *data.UserID,
		EventType:              paymentType,
		Processor:              data.Processor,
		ProcessorTransactionID: data.ProcessorTransactionID,
		Amount:                 data.Amount,
		Currency:               data.Currency,
		BillingInfo:            "{}",
		WebhookSource:          source,
		Metadata:               data.Metadata,
		Timestamp:              data.Timestamp,
	}
	return s.LogPaymentEvent(ctx, ped)
}

// LogACUEvent logs an Automatic Card Updater event
func (s *EventLogService) LogACUEvent(ctx context.Context, data ACUEventData) error {
	if err := s.ensureConn(ctx); err != nil {
		log.WithError(err).Warn("ClickHouse not available - spooling ACU event")
		s.enqueue("acu", data)
		return nil
	}

	if data.EventID == uuid.Nil {
		data.EventID = uuid.New()
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}

	// Redact card_info (writer must not store PANs); keep last4 only if present
	data.CardInfo = redactPII(data.CardInfo)

	query := `
        INSERT INTO acu_events (
            event_id, subscription_id, user_id, event_type, processor,
            processor_subscription_id, card_info, update_status, requires_action,
            reason, metadata, timestamp
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `

	err := s.clickhouseConn.Exec(ctx, query,
		data.EventID,
		data.SubscriptionID,
		data.UserID,
		data.EventType,
		data.Processor,
		data.ProcessorSubscriptionID,
		data.CardInfo,
		data.UpdateStatus,
		data.RequiresAction,
		data.Reason,
		data.Metadata,
		data.Timestamp,
	)

	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"event_id":      data.EventID,
			"event_type":    data.EventType,
			"processor":     data.Processor,
			"update_status": data.UpdateStatus,
		}).Error("Failed to log ACU event to ClickHouse; spooling")
		s.enqueue("acu", data)
		return fmt.Errorf("failed to log ACU event: %w", err)
	}

	log.WithFields(log.Fields{
		"event_id":      data.EventID,
		"event_type":    data.EventType,
		"processor":     data.Processor,
		"update_status": data.UpdateStatus,
	}).Debug("Logged ACU event to ClickHouse")

	return nil
}

// LogChargebackEvent logs a chargeback event
func (s *EventLogService) LogChargebackEvent(ctx context.Context, data ChargebackEventData) error {
	if err := s.ensureConn(ctx); err != nil {
		log.WithError(err).Warn("ClickHouse not available - spooling chargeback event")
		s.enqueue("chargeback", data)
		return nil
	}

	if data.EventID == uuid.Nil {
		data.EventID = uuid.New()
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}
	if data.Currency == "" {
		data.Currency = "usd"
	}

	query := `
		INSERT INTO chargeback_events (
			event_id, chargeback_id, batch_id, subscription_id, user_id, event_type, processor,
			processor_transaction_id, amount, currency, chargeback_type, reason,
			status, metadata, timestamp
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	err := s.clickhouseConn.Exec(ctx, query,
		data.EventID,
		data.ChargebackID,
		data.BatchID,
		nullableUUID(data.SubscriptionID),
		nullableString2(data.UserID),
		data.EventType,
		data.Processor,
		nullableString2(data.ProcessorTransactionID),
		data.Amount,
		data.Currency,
		data.ChargebackType,
		data.Reason,
		data.Status,
		data.Metadata,
		data.Timestamp,
	)

	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"event_id":        data.EventID,
			"chargeback_id":   data.ChargebackID,
			"event_type":      data.EventType,
			"processor":       data.Processor,
			"chargeback_type": data.ChargebackType,
		}).Error("Failed to log chargeback event to ClickHouse; spooling")
		s.enqueue("chargeback", data)
		return fmt.Errorf("failed to log chargeback event: %w", err)
	}

	log.WithFields(log.Fields{
		"event_id":        data.EventID,
		"chargeback_id":   data.ChargebackID,
		"event_type":      data.EventType,
		"processor":       data.Processor,
		"chargeback_type": data.ChargebackType,
	}).Debug("Logged chargeback event to ClickHouse")

	return nil
}

// Helper method to create metadata JSON
func CreateMetadataJSON(data map[string]interface{}) string {
	if data == nil {
		return "{}"
	}

	jsonData, err := json.Marshal(data)
	if err != nil {
		log.WithError(err).Error("Failed to marshal metadata to JSON")
		return "{}"
	}

	return string(jsonData)
}

func (s *EventLogService) LogAdminSubscriptionCancellation(ctx context.Context, subscription *models.Subscription, reason string, at time.Time) error {
	if s == nil || subscription == nil {
		return nil
	}

	var procSubID *string
	if subscription.ProcessorSubscriptionID != "" {
		procSubID = &subscription.ProcessorSubscriptionID
	}

	cancelType := ""
	if subscription.CancelType != nil {
		cancelType = string(*subscription.CancelType)
	}

	metadata := map[string]interface{}{"source": "admin"}
	if cancelType != "" {
		metadata["cancel_type"] = cancelType
	}
	if reason != "" {
		metadata["reason"] = redactPII(reason)
	}

	var priceAmount float64
	priceCurrency := "usd"
	var billingDays uint32
	var productID *uuid.UUID
	var priceID *uuid.UUID
	if subscription.Price != nil {
		priceAmount = float64(subscription.Price.Amount) / 100.0
		priceCurrency = subscription.Price.Currency
		if subscription.Price.BillingCycleDays != nil {
			billingDays = uint32(*subscription.Price.BillingCycleDays)
		}
		productID = &subscription.Price.ProductID
		priceID = &subscription.Price.ID
	}

	return s.LogSubscriptionEvent(ctx, SubscriptionEventData{
		SubscriptionID:          subscription.ID,
		UserID:                  subscription.UserID,
		EventType:               PaymentEventSubscriptionCancelled,
		Status:                  string(subscription.Status),
		CancelType:              cancelType,
		PriceAmount:             priceAmount,
		PriceCurrency:           priceCurrency,
		BillingCycleDays:        billingDays,
		ProductID:               productID,
		PriceID:                 priceID,
		Processor:               string(subscription.Processor),
		ProcessorSubscriptionID: procSubID,
		Metadata:                CreateMetadataJSON(metadata),
		Timestamp:               at.UTC(),
	})
}

// enqueue helper
func (s *EventLogService) enqueue(kind string, v interface{}) {
	if s.spool == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = s.spool.Enqueue(&spool.Record{Kind: kind, Data: b})
}

// insert helpers used by both direct logging and background flush
func (s *EventLogService) insertSubscription(ctx context.Context, data SubscriptionEventData) error {
	query := `
        INSERT INTO subscription_events (
            event_id, subscription_id, user_id, event_type, status, cancel_type,
            processor, processor_subscription_id, processor_transaction_id,
            price_amount, price_currency, billing_cycle_days, product_id, price_id,
            metadata, timestamp
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `
	return s.clickhouseConn.Exec(ctx, query,
		data.EventID,
		data.SubscriptionID,
		data.UserID,
		data.EventType,
		data.Status,
		data.CancelType,
		data.Processor,
		nullableString2(data.ProcessorSubscriptionID),
		nullableString2(data.ProcessorTransactionID),
		data.PriceAmount,
		data.PriceCurrency,
		data.BillingCycleDays,
		nullableUUID(data.ProductID),
		nullableUUID(data.PriceID),
		data.Metadata,
		data.Timestamp,
	)
}

func (s *EventLogService) insertSubscriptionBatch(ctx context.Context, rows []SubscriptionEventData) error {
	batch, err := s.clickhouseConn.PrepareBatch(ctx, `INSERT INTO subscription_events (event_id, subscription_id, user_id, event_type, status, cancel_type, processor, processor_subscription_id, processor_transaction_id, price_amount, price_currency, billing_cycle_days, product_id, price_id, metadata, timestamp) VALUES`)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if err := batch.Append(d.EventID, d.SubscriptionID, d.UserID, d.EventType, d.Status, d.CancelType, d.Processor, nullableString2(d.ProcessorSubscriptionID), nullableString2(d.ProcessorTransactionID), d.PriceAmount, d.PriceCurrency, d.BillingCycleDays, nullableUUID(d.ProductID), nullableUUID(d.PriceID), d.Metadata, d.Timestamp); err != nil {
			return err
		}
	}
	return batch.Send()
}

func nullableUUID(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return *id
}

func nullableString2(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func (s *EventLogService) insertPayment(ctx context.Context, data PaymentEventData) error {
	query := `
        INSERT INTO payment_events (
            event_id, subscription_id, user_id, event_type, processor,
            processor_transaction_id, amount, currency, billing_info,
            webhook_source, metadata, timestamp
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `
	return s.clickhouseConn.Exec(ctx, query,
		data.EventID,
		data.SubscriptionID,
		data.UserID,
		data.EventType,
		data.Processor,
		data.ProcessorTransactionID,
		data.Amount,
		data.Currency,
		data.BillingInfo,
		data.WebhookSource,
		data.Metadata,
		data.Timestamp,
	)
}

func (s *EventLogService) insertPaymentBatch(ctx context.Context, rows []PaymentEventData) error {
	batch, err := s.clickhouseConn.PrepareBatch(ctx, `INSERT INTO payment_events (event_id, subscription_id, user_id, event_type, processor, processor_transaction_id, amount, currency, billing_info, webhook_source, metadata, timestamp) VALUES`)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if err := batch.Append(d.EventID, d.SubscriptionID, d.UserID, d.EventType, d.Processor, d.ProcessorTransactionID, d.Amount, d.Currency, d.BillingInfo, d.WebhookSource, d.Metadata, d.Timestamp); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (s *EventLogService) insertACUBatch(ctx context.Context, rows []ACUEventData) error {
	batch, err := s.clickhouseConn.PrepareBatch(ctx, `INSERT INTO acu_events (event_id, subscription_id, user_id, event_type, processor, processor_subscription_id, card_info, update_status, requires_action, reason, metadata, timestamp) VALUES`)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if err := batch.Append(d.EventID, d.SubscriptionID, d.UserID, d.EventType, d.Processor, d.ProcessorSubscriptionID, d.CardInfo, d.UpdateStatus, d.RequiresAction, d.Reason, d.Metadata, d.Timestamp); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (s *EventLogService) insertChargebackBatch(ctx context.Context, rows []ChargebackEventData) error {
	batch, err := s.clickhouseConn.PrepareBatch(ctx, `INSERT INTO chargeback_events (event_id, chargeback_id, batch_id, subscription_id, user_id, event_type, processor, processor_transaction_id, amount, currency, chargeback_type, reason, status, metadata, timestamp) VALUES`)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if err := batch.Append(d.EventID, d.ChargebackID, d.BatchID, nullableUUID(d.SubscriptionID), nullableString2(d.UserID), d.EventType, d.Processor, nullableString2(d.ProcessorTransactionID), d.Amount, d.Currency, d.ChargebackType, d.Reason, d.Status, d.Metadata, d.Timestamp); err != nil {
			return err
		}
	}
	return batch.Send()
}

// initClickHouseConnection creates a connection to ClickHouse
func initClickHouseConnection(cfg *config.ClickHouseConfig) (driver.Conn, error) {
	if cfg.HTTPAddr == "" {
		return nil, fmt.Errorf("ClickHouse HTTPAddr not configured")
	}
	// Derive native protocol address from HTTPAddr.
	// If an HTTP URL is provided (http://host:8123), translate to native (host:9000).
	var serverAddr string
	// Explicit client_addr takes precedence when set (e.g., custom port 9002).
	if strings.TrimSpace(cfg.ClientAddr) != "" {
		serverAddr = strings.TrimSpace(cfg.ClientAddr)
	}
	if serverAddr == "" && (strings.HasPrefix(cfg.HTTPAddr, "http://") || strings.HasPrefix(cfg.HTTPAddr, "https://")) {
		if u, err := url.Parse(cfg.HTTPAddr); err == nil {
			host := u.Hostname()
			port := u.Port()
			if port == "" || port == "8123" {
				port = "9000"
			}
			serverAddr = host + ":" + port
		}
	}
	if serverAddr == "" {
		// No scheme provided; assume native address
		addr := cfg.HTTPAddr
		if strings.Contains(addr, ":") {
			serverAddr = addr
		} else {
			serverAddr = addr + ":9000"
		}
	}

	options := &clickhouse.Options{
		Addr: []string{serverAddr},
		Auth: clickhouse.Auth{
			Database: cfg.Database,
			Username: cfg.Username,
			Password: cfg.Password,
		},
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
		Settings: clickhouse.Settings{
			"max_execution_time": 60,
		},
		DialTimeout: 30 * time.Second,
	}

	conn, err := clickhouse.Open(options)
	if err != nil {
		return nil, fmt.Errorf("failed to open ClickHouse connection: %w", err)
	}

	// Test the connection
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping ClickHouse: %w", err)
	}

	log.Info("Successfully connected to ClickHouse for billing event logging")
	return conn, nil
}
