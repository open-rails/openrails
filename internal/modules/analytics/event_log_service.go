package analytics

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
	"sync"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/spool"
	log "github.com/sirupsen/logrus"
)

// ARCHITECTURAL BOUNDARY (issue #232 — Postgres is the system of record):
//
//	Postgres is the authoritative system of record for billing state and for
//	every money / entitlement / authorization DECISION (balances, holds,
//	credits, charges, subscription status, entitlements). ClickHouse is a
//	DERIVED, merchant-scoped analytics / event sink ONLY.
//
// Three invariants this package must preserve:
//  1. No money / entitlement / authorization decision ever READS from
//     ClickHouse. This service therefore exposes only write/log methods and
//     admin DISPLAY metrics (see AdminMetricsService); it must never be wired
//     into a credit/hold/entitlement/charge/authz decision path.
//  2. Every ClickHouse event is merchant-scoped: it carries the resolved
//     merchant_id (merchant.Require). Hosted multi-merchant analytics
//     must never be written unscoped.
//  3. ClickHouse is OPTIONAL: when unconfigured/unavailable, every Log* method
//     is a no-op or spools-and-returns-nil — it never errors on the billing
//     hot path, so billing correctness never depends on ClickHouse.
//
// Do NOT reintroduce ClickHouse as a source of truth for any decision.

// EventLogService handles logging billing events to ClickHouse
type EventLogService struct {
	mu             sync.Mutex
	clickhouseConn driver.Conn
	config         *config.ClickHouseConfig
	spool          *spool.Spool
	clockMu        sync.RWMutex
	clock          clockwork.Clock
	flushCancel    context.CancelFunc
	flushWG        sync.WaitGroup
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *EventLogService) now() time.Time {
	s.clockMu.RLock()
	defer s.clockMu.RUnlock()
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// NewEventLogService creates a new event log service
func NewEventLogService(cfg *config.ClickHouseConfig, clocks ...clockwork.Clock) (*EventLogService, error) {
	clock := timeutil.FirstClock(clocks...)
	// Feature gate: presence of HTTPAddr indicates intent to use CH
	if cfg == nil || cfg.HTTPAddr == "" {
		log.Warn("ClickHouse HTTPAddr not configured - billing events will not be logged")
		return &EventLogService{clock: clock}, nil
	}

	sp, err := spool.New("")
	if err != nil {
		return nil, fmt.Errorf("create analytics spool: %w", err)
	}

	// Try connect; if it fails, return service with nil conn so it can retry later
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := initClickHouseConnection(ctx, cfg)
	if err != nil {
		log.WithError(err).Warn("ClickHouse unavailable at startup; will retry on use")
		svc := &EventLogService{clickhouseConn: nil, config: cfg, spool: sp, clock: clock}
		svc.startBackgroundFlush()
		return svc, nil
	}

	svc := &EventLogService{clickhouseConn: conn, config: cfg, spool: sp, clock: clock}
	svc.startBackgroundFlush()
	return svc, nil
}

func (s *EventLogService) SetClock(c clockwork.Clock) {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	s.clock = timeutil.FirstClock(c)
}

func (s *EventLogService) Clock() clockwork.Clock {
	s.clockMu.RLock()
	defer s.clockMu.RUnlock()
	return s.clock
}

// resolveMerchantID returns the merchant id to stamp on a ClickHouse analytics
// event. It prefers a merchant_id already set on the event (e.g. resolved by an
// upstream writer), otherwise resolves it from the request context. A missing
// merchant is an error: analytics events must always be merchant-scoped, so we
// never write an unscoped hosted event (issue #232).
func resolveMerchantID(ctx context.Context, existing string) (string, error) {
	if existing != "" {
		return existing, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	return tid.String(), nil
}

// ensureConn lazily (re)establishes the ClickHouse connection
func (s *EventLogService) ensureConn(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureConnLocked(ctx)
}

// ensureConnLocked lazily (re)establishes the ClickHouse connection.
// s.mu must be held by the caller.
func (s *EventLogService) ensureConnLocked(ctx context.Context) error {
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
	conn, err := initClickHouseConnection(ctx, s.config)
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

// NOTE (issue #232): a ClickHouse-reading SubscriptionEventExists helper used to
// live here. It was dead code (no caller) and a READ against ClickHouse, which
// violates the boundary that no decision/dedup logic reads from the analytics
// sink. Subscription-event de-duplication, like all billing decisions, must be
// derived from Postgres state. The helper was removed; do not reintroduce
// ClickHouse reads on any non-admin-display path.

func (s *EventLogService) startBackgroundFlush() {
	if s.spool == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.flushCancel = cancel
	s.flushWG.Add(1)
	go func() {
		defer s.flushWG.Done()
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				flushCtx, flushCancel := context.WithTimeout(ctx, 5*time.Second)
				if err := s.flushOnce(flushCtx, 200); err != nil && flushCtx.Err() == nil {
					log.WithError(err).Warn("analytics spool flush failed")
				}
				flushCancel()
			}
		}
	}()
}

func (s *EventLogService) flushOnce(ctx context.Context, limit int) error {
	if s.spool == nil {
		return nil
	}
	paths, err := s.spool.List(limit)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureConnLocked(ctx); err != nil {
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
				d.EventID = uuidutil.NewV7()
			}
			if d.MerchantID, err = resolveMerchantID(ctx, d.MerchantID); err != nil {
				log.WithError(err).Warn("resolve merchant for subscription")
				dead(p)
				continue
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
				d.EventID = uuidutil.NewV7()
			}
			if d.MerchantID, err = resolveMerchantID(ctx, d.MerchantID); err != nil {
				log.WithError(err).Warn("resolve merchant for payment")
				dead(p)
				continue
			}
			if d.Timestamp.IsZero() {
				d.Timestamp = s.now().UTC()
			}
			if cur, cerr := analyticsCurrency(d.Currency, d.Amount); cerr != nil {
				log.WithError(cerr).Warn("payment currency")
				dead(p)
				continue
			} else {
				d.Currency = cur
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
				d.EventID = uuidutil.NewV7()
			}
			if d.MerchantID, err = resolveMerchantID(ctx, d.MerchantID); err != nil {
				log.WithError(err).Warn("resolve merchant for transaction")
				dead(p)
				continue
			}
			if d.Timestamp.IsZero() {
				d.Timestamp = s.now().UTC()
			}
			if cur, cerr := analyticsCurrency(d.Currency, d.Amount); cerr != nil {
				log.WithError(cerr).Warn("transaction currency")
				dead(p)
				continue
			} else {
				d.Currency = cur
			}
			// Map to PaymentEventData as in LogTransactionEvent
			userID := ""
			if d.UserID != nil {
				userID = *d.UserID
			}
			transAsPayments = append(transAsPayments, PaymentEventData{
				EventID:           d.EventID,
				MerchantID:        d.MerchantID,
				SubscriptionID:    d.SubscriptionID,
				UserID:            userID,
				EventType:         d.EventType,
				Rail:              d.Rail,
				RailTransactionID: d.RailTransactionID,
				Amount:            d.Amount,
				Currency:          d.Currency,
				BillingInfo:       "{}",
				WebhookSource:     "",
				Metadata:          d.Metadata,
				Timestamp:         d.Timestamp,
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
				d.EventID = uuidutil.NewV7()
			}
			if d.MerchantID, err = resolveMerchantID(ctx, d.MerchantID); err != nil {
				log.WithError(err).Warn("resolve merchant for acu")
				dead(p)
				continue
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
				d.EventID = uuidutil.NewV7()
			}
			if d.MerchantID, err = resolveMerchantID(ctx, d.MerchantID); err != nil {
				log.WithError(err).Warn("resolve merchant for chargeback")
				dead(p)
				continue
			}
			if d.Timestamp.IsZero() {
				d.Timestamp = s.now().UTC()
			}
			if cur, cerr := analyticsCurrency(d.Currency, d.Amount); cerr != nil {
				log.WithError(cerr).Warn("chargeback currency")
				dead(p)
				continue
			} else {
				d.Currency = cur
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
			if err := s.spool.Remove(f.path); err != nil {
				log.WithError(err).Warnf("remove analytics spool %s", filepath.Base(f.path))
			}
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
	if s.flushCancel != nil {
		s.flushCancel()
		s.flushWG.Wait()
	}
	if s.spool != nil && s.config != nil && s.config.HTTPAddr != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := s.flushOnce(ctx, 200); err != nil && ctx.Err() == nil {
			log.WithError(err).Warn("analytics spool final flush failed")
		}
		cancel()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clickhouseConn != nil {
		err := s.clickhouseConn.Close()
		s.clickhouseConn = nil
		return err
	}
	return nil
}

// SubscriptionEventData represents data for subscription events
type SubscriptionEventData struct {
	EventID uuid.UUID `json:"event_id"`
	// MerchantID scopes this analytics event to a merchant / billing namespace
	// (issue #232). It is populated from the request context at log time via
	// merchant.Require; never write an unscoped hosted event.
	MerchantID         string     `json:"merchant_id"`
	SubscriptionID     uuid.UUID  `json:"subscription_id"`
	UserID             string     `json:"user_id"`
	EventType          string     `json:"event_type"`
	Status             string     `json:"status"`
	CancelType         string     `json:"cancel_type"`
	PriceAmount        float64    `json:"price_amount"`
	PriceCurrency      string     `json:"price_currency"`
	BillingCycleDays   uint32     `json:"billing_cycle_days"`
	ProductID          *uuid.UUID `json:"product_id,omitempty"`
	PriceID            *uuid.UUID `json:"price_id,omitempty"`
	Rail               string     `json:"rail"`
	RailSubscriptionID *string    `json:"rail_subscription_id,omitempty"`
	RailTransactionID  *string    `json:"rail_transaction_id,omitempty"`
	Metadata           string     `json:"metadata"`
	Timestamp          time.Time  `json:"timestamp"`
}

// PaymentEventType defines standardized event types for payment logging.
// All rails (CCBill, NMI-backed, Solana) should use these constants
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
	EventID uuid.UUID `json:"event_id"`
	// MerchantID scopes this analytics event to a merchant / billing namespace (issue #232).
	MerchantID        string     `json:"merchant_id"`
	SubscriptionID    *uuid.UUID `json:"subscription_id,omitempty"`
	UserID            string     `json:"user_id"`
	EventType         string     `json:"event_type"`
	Rail              string     `json:"rail"`
	RailTransactionID *string    `json:"rail_transaction_id,omitempty"`
	Amount            *float64   `json:"amount,omitempty"`
	Currency          string     `json:"currency"`
	BillingInfo       string     `json:"billing_info"`
	WebhookSource     string     `json:"webhook_source"`
	Metadata          string     `json:"metadata"`
	Timestamp         time.Time  `json:"timestamp"`
}

// TransactionEventData represents data for transaction events
type TransactionEventData struct {
	EventID uuid.UUID `json:"event_id"`
	// MerchantID scopes this analytics event to a merchant / billing namespace (issue #232).
	MerchantID        string     `json:"merchant_id"`
	TransactionID     string     `json:"transaction_id"`
	SubscriptionID    *uuid.UUID `json:"subscription_id,omitempty"`
	UserID            *string    `json:"user_id,omitempty"`
	EventType         string     `json:"event_type"`
	Rail              string     `json:"rail"`
	RailTransactionID *string    `json:"rail_transaction_id,omitempty"`
	Amount            *float64   `json:"amount,omitempty"`
	Currency          string     `json:"currency"`
	Status            string     `json:"status"`
	Metadata          string     `json:"metadata"`
	Timestamp         time.Time  `json:"timestamp"`
}

// ACUEventData represents data for Automatic Card Updater events
type ACUEventData struct {
	EventID uuid.UUID `json:"event_id"`
	// MerchantID scopes this analytics event to a merchant / billing namespace (issue #232).
	MerchantID         string     `json:"merchant_id"`
	SubscriptionID     *uuid.UUID `json:"subscription_id,omitempty"`
	UserID             *string    `json:"user_id,omitempty"`
	EventType          string     `json:"event_type"`
	Rail               string     `json:"rail"`
	RailSubscriptionID *string    `json:"rail_subscription_id,omitempty"`
	CardInfo           string     `json:"card_info"`
	UpdateStatus       string     `json:"update_status"`
	RequiresAction     bool       `json:"requires_action"`
	Reason             string     `json:"reason"`
	Metadata           string     `json:"metadata"`
	Timestamp          time.Time  `json:"timestamp"`
}

// ChargebackEventData represents data for chargeback events
type ChargebackEventData struct {
	EventID uuid.UUID `json:"event_id"`
	// MerchantID scopes this analytics event to a merchant / billing namespace (issue #232).
	MerchantID        string     `json:"merchant_id"`
	ChargebackID      string     `json:"chargeback_id"`
	BatchID           string     `json:"batch_id"`
	SubscriptionID    *uuid.UUID `json:"subscription_id,omitempty"`
	UserID            *string    `json:"user_id,omitempty"`
	EventType         string     `json:"event_type"`
	Rail              string     `json:"rail"`
	RailTransactionID *string    `json:"rail_transaction_id,omitempty"`
	Amount            *float64   `json:"amount,omitempty"`
	Currency          string     `json:"currency"`
	ChargebackType    string     `json:"chargeback_type"`
	Reason            string     `json:"reason"`
	Status            string     `json:"status"`
	Metadata          string     `json:"metadata"`
	Timestamp         time.Time  `json:"timestamp"`
}

// Basic PII redaction for free-form strings
// - masks emails, long digit sequences, and truncates to a max length
func redactPII(s string) string {
	return redactAndTruncate(s, 1024)
}

func analyticsCurrency(currency string, amount *float64) (string, error) {
	currency = strings.TrimSpace(currency)
	if amount != nil && currency == "" {
		return "", fmt.Errorf("currency required when amount is set")
	}
	return currency, nil
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
	if s == nil || s.config == nil || s.config.HTTPAddr == "" {
		return nil
	}
	if data.EventID == uuid.Nil {
		data.EventID = uuidutil.NewV7()
	}
	if resolved, err := resolveMerchantID(ctx, data.MerchantID); err != nil {
		return err
	} else {
		data.MerchantID = resolved
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}

	s.mu.Lock()
	err := s.ensureConnLocked(ctx)
	if err == nil {
		err = s.insertSubscription(ctx, data)
	}
	s.mu.Unlock()
	if err != nil {
		if err := s.enqueue("subscription", data); err != nil {
			return fmt.Errorf("failed to log subscription event and enqueue spool record: %w", err)
		}
		log.WithError(err).WithFields(log.Fields{
			"event_id":        data.EventID,
			"subscription_id": data.SubscriptionID,
			"event_type":      data.EventType,
			"rail":            data.Rail,
		}).Warn("Failed to log subscription event to ClickHouse; spooled")
		return nil
	}

	log.WithFields(log.Fields{
		"event_id":        data.EventID,
		"subscription_id": data.SubscriptionID,
		"event_type":      data.EventType,
		"rail":            data.Rail,
	}).Debug("Logged subscription event to ClickHouse")

	return nil
}

// LogPaymentEvent logs a payment transaction event
func (s *EventLogService) LogPaymentEvent(ctx context.Context, data PaymentEventData) error {
	if s == nil || s.config == nil || s.config.HTTPAddr == "" {
		return nil
	}
	if data.EventID == uuid.Nil {
		data.EventID = uuidutil.NewV7()
	}
	if resolved, err := resolveMerchantID(ctx, data.MerchantID); err != nil {
		return err
	} else {
		data.MerchantID = resolved
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}
	currency, err := analyticsCurrency(data.Currency, data.Amount)
	if err != nil {
		return err
	}
	data.Currency = currency

	s.mu.Lock()
	err = s.ensureConnLocked(ctx)
	if err == nil {
		err = s.insertPayment(ctx, data)
	}
	s.mu.Unlock()
	if err != nil {
		if err := s.enqueue("payment", data); err != nil {
			return fmt.Errorf("failed to log payment event and enqueue spool record: %w", err)
		}
		log.WithError(err).WithFields(log.Fields{
			"event_id":   data.EventID,
			"user_id":    data.UserID,
			"event_type": data.EventType,
			"rail":       data.Rail,
		}).Warn("Failed to log payment event to ClickHouse; spooled")
		return nil
	}

	log.WithFields(log.Fields{
		"event_id":   data.EventID,
		"user_id":    data.UserID,
		"event_type": data.EventType,
		"rail":       data.Rail,
	}).Debug("Logged payment event to ClickHouse")

	return nil
}

// LogTransactionEvent logs a transaction event
func (s *EventLogService) LogTransactionEvent(ctx context.Context, data TransactionEventData) error {
	// Map transaction event into payment_events to avoid separate table.
	if s == nil || s.config == nil || s.config.HTTPAddr == "" {
		return nil
	}
	if data.EventID == uuid.Nil {
		data.EventID = uuidutil.NewV7()
	}
	if resolved, err := resolveMerchantID(ctx, data.MerchantID); err != nil {
		return err
	} else {
		data.MerchantID = resolved
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}
	currency, err := analyticsCurrency(data.Currency, data.Amount)
	if err != nil {
		return err
	}
	data.Currency = currency
	if err := s.ensureConn(ctx); err != nil {
		if err := s.enqueue("transaction", data); err != nil {
			return fmt.Errorf("failed to enqueue transaction event: %w", err)
		}
		log.WithError(err).Warn("ClickHouse not available - spooled transaction event")
		return nil
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
		EventID:           data.EventID,
		MerchantID:        data.MerchantID,
		SubscriptionID:    data.SubscriptionID,
		UserID:            *data.UserID,
		EventType:         paymentType,
		Rail:              data.Rail,
		RailTransactionID: data.RailTransactionID,
		Amount:            data.Amount,
		Currency:          data.Currency,
		BillingInfo:       "{}",
		WebhookSource:     source,
		Metadata:          data.Metadata,
		Timestamp:         data.Timestamp,
	}
	return s.LogPaymentEvent(ctx, ped)
}

// LogACUEvent logs an Automatic Card Updater event
func (s *EventLogService) LogACUEvent(ctx context.Context, data ACUEventData) error {
	if s == nil || s.config == nil || s.config.HTTPAddr == "" {
		return nil
	}
	if data.EventID == uuid.Nil {
		data.EventID = uuidutil.NewV7()
	}
	if resolved, err := resolveMerchantID(ctx, data.MerchantID); err != nil {
		return err
	} else {
		data.MerchantID = resolved
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}

	// Redact card_info (writer must not store PANs); keep last4 only if present
	data.CardInfo = redactPII(data.CardInfo)

	query := `
        INSERT INTO acu_events (
            event_id, merchant_id, subscription_id, user_id, event_type, rail,
            rail_subscription_id, card_info, update_status, requires_action,
            reason, metadata, timestamp
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `

	s.mu.Lock()
	err := s.ensureConnLocked(ctx)
	if err == nil {
		err = s.clickhouseConn.Exec(ctx, query,
			data.EventID,
			data.MerchantID,
			data.SubscriptionID,
			data.UserID,
			data.EventType,
			data.Rail,
			data.RailSubscriptionID,
			data.CardInfo,
			data.UpdateStatus,
			data.RequiresAction,
			data.Reason,
			data.Metadata,
			data.Timestamp,
		)
	}
	s.mu.Unlock()

	if err != nil {
		if err := s.enqueue("acu", data); err != nil {
			return fmt.Errorf("failed to log ACU event and enqueue spool record: %w", err)
		}
		log.WithError(err).WithFields(log.Fields{
			"event_id":      data.EventID,
			"event_type":    data.EventType,
			"rail":          data.Rail,
			"update_status": data.UpdateStatus,
		}).Warn("Failed to log ACU event to ClickHouse; spooled")
		return nil
	}

	log.WithFields(log.Fields{
		"event_id":      data.EventID,
		"event_type":    data.EventType,
		"rail":          data.Rail,
		"update_status": data.UpdateStatus,
	}).Debug("Logged ACU event to ClickHouse")

	return nil
}

// LogChargebackEvent logs a chargeback event
func (s *EventLogService) LogChargebackEvent(ctx context.Context, data ChargebackEventData) error {
	if s == nil || s.config == nil || s.config.HTTPAddr == "" {
		return nil
	}
	if data.EventID == uuid.Nil {
		data.EventID = uuidutil.NewV7()
	}
	if resolved, err := resolveMerchantID(ctx, data.MerchantID); err != nil {
		return err
	} else {
		data.MerchantID = resolved
	}
	if data.Timestamp.IsZero() {
		data.Timestamp = s.now().UTC()
	}
	currency, err := analyticsCurrency(data.Currency, data.Amount)
	if err != nil {
		return err
	}
	data.Currency = currency

	query := `
		INSERT INTO chargeback_events (
			event_id, merchant_id, chargeback_id, batch_id, subscription_id, user_id, event_type, rail,
			rail_transaction_id, amount, currency, chargeback_type, reason,
			status, metadata, timestamp
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	s.mu.Lock()
	err = s.ensureConnLocked(ctx)
	if err == nil {
		err = s.clickhouseConn.Exec(ctx, query,
			data.EventID,
			data.MerchantID,
			data.ChargebackID,
			data.BatchID,
			nullableUUID(data.SubscriptionID),
			nullableString2(data.UserID),
			data.EventType,
			data.Rail,
			nullableString2(data.RailTransactionID),
			data.Amount,
			data.Currency,
			data.ChargebackType,
			data.Reason,
			data.Status,
			data.Metadata,
			data.Timestamp,
		)
	}
	s.mu.Unlock()

	if err != nil {
		if err := s.enqueue("chargeback", data); err != nil {
			return fmt.Errorf("failed to log chargeback event and enqueue spool record: %w", err)
		}
		log.WithError(err).WithFields(log.Fields{
			"event_id":        data.EventID,
			"chargeback_id":   data.ChargebackID,
			"event_type":      data.EventType,
			"rail":            data.Rail,
			"chargeback_type": data.ChargebackType,
		}).Warn("Failed to log chargeback event to ClickHouse; spooled")
		return nil
	}

	log.WithFields(log.Fields{
		"event_id":        data.EventID,
		"chargeback_id":   data.ChargebackID,
		"event_type":      data.EventType,
		"rail":            data.Rail,
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
	if subscription.RailSubscriptionID != "" {
		procSubID = &subscription.RailSubscriptionID
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
	priceCurrency := ""
	var billingDays uint32
	var productID *uuid.UUID
	var priceID *uuid.UUID
	if subscription.Price != nil {
		priceAmount = float64(subscription.Price.Amount) / 100.0
		priceCurrency = subscription.Price.Currency
		if subscription.Price.BillingCycleDays != nil {
			billingDays, _ = safecast.Convert[uint32](*subscription.Price.BillingCycleDays)
		}
		productID = &subscription.Price.ProductID
		priceID = &subscription.Price.ID
	}

	return s.LogSubscriptionEvent(ctx, SubscriptionEventData{
		SubscriptionID:     subscription.ID,
		UserID:             subscription.CustomerID.String(),
		EventType:          PaymentEventSubscriptionCancelled,
		Status:             string(subscription.Status),
		CancelType:         cancelType,
		PriceAmount:        priceAmount,
		PriceCurrency:      priceCurrency,
		BillingCycleDays:   billingDays,
		ProductID:          productID,
		PriceID:            priceID,
		Rail:               string(subscription.Rail),
		RailSubscriptionID: procSubID,
		Metadata:           CreateMetadataJSON(metadata),
		Timestamp:          at.UTC(),
	})
}

func (s *EventLogService) LogLifecycleChargeSuccess(ctx context.Context, sub *models.Subscription, rail models.Rail, transactionID string, amount int64, currency string, at time.Time, metadata map[string]interface{}) error {
	if s == nil || sub == nil {
		return nil
	}
	var amountFloat *float64
	if amount > 0 {
		f := float64(amount) / 100.0
		amountFloat = &f
	}
	var txnID *string
	if transactionID != "" {
		txnID = &transactionID
	}
	currency = strings.TrimSpace(currency)
	if amount > 0 && currency == "" {
		return fmt.Errorf("currency required when amount is set")
	}
	if metadata == nil {
		metadata = map[string]interface{}{}
	}
	metadata["subscription_id"] = sub.ID.String()
	return s.LogPaymentEvent(ctx, PaymentEventData{
		SubscriptionID:    &sub.ID,
		UserID:            sub.CustomerID.String(),
		EventType:         PaymentEventChargeSuccess,
		Rail:              string(rail),
		RailTransactionID: txnID,
		Amount:            amountFloat,
		Currency:          currency,
		BillingInfo:       "{}",
		WebhookSource:     "lifecycle",
		Metadata:          CreateMetadataJSON(metadata),
		Timestamp:         at.UTC(),
	})
}

func (s *EventLogService) LogLifecycleCancellation(ctx context.Context, subscriptionID uuid.UUID, userID string, rail models.Rail, cancelType models.CancelType, revokeAccess bool, at time.Time) error {
	if s == nil {
		return nil
	}
	return s.LogSubscriptionEvent(ctx, SubscriptionEventData{
		SubscriptionID: subscriptionID,
		UserID:         userID,
		EventType:      PaymentEventSubscriptionCancelled,
		Rail:           string(rail),
		Metadata:       CreateMetadataJSON(map[string]interface{}{"cancel_type": string(cancelType), "revoke_access": revokeAccess}),
		Timestamp:      at.UTC(),
	})
}

func (s *EventLogService) LogLifecycleFailure(ctx context.Context, subscriptionID uuid.UUID, userID string, rail models.Rail, finalStatus models.SubscriptionStatus, failureReason *string, failureCode *string, at time.Time) error {
	if s == nil {
		return nil
	}
	eventType := PaymentEventChargeFailure
	metadata := map[string]interface{}{
		"subscription_id": subscriptionID.String(),
		"final_status":    string(finalStatus),
	}
	if failureReason != nil {
		metadata["failure_reason"] = *failureReason
	}
	if failureCode != nil {
		metadata["failure_code"] = *failureCode
	}
	if finalStatus == models.StatusCancelled {
		eventType = PaymentEventSubscriptionExpired
	}
	return s.LogPaymentEvent(ctx, PaymentEventData{
		SubscriptionID: &subscriptionID,
		UserID:         userID,
		EventType:      eventType,
		Rail:           string(rail),
		Currency:       "",
		BillingInfo:    "{}",
		WebhookSource:  "lifecycle",
		Metadata:       CreateMetadataJSON(metadata),
		Timestamp:      at.UTC(),
	})
}

// enqueue helper
func (s *EventLogService) enqueue(kind string, v interface{}) error {
	if s.spool == nil {
		return fmt.Errorf("analytics spool unavailable")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal analytics event: %w", err)
	}
	if err := s.spool.Enqueue(&spool.Record{Kind: kind, Data: b}); err != nil {
		return fmt.Errorf("enqueue analytics spool record: %w", err)
	}
	return nil
}

// insert helpers used by both direct logging and background flush
func (s *EventLogService) insertSubscription(ctx context.Context, data SubscriptionEventData) error {
	query := `
        INSERT INTO subscription_events (
            event_id, merchant_id, subscription_id, user_id, event_type, status, cancel_type,
            rail, rail_subscription_id, rail_transaction_id,
            price_amount, price_currency, billing_cycle_days, product_id, price_id,
            metadata, timestamp
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `
	return s.clickhouseConn.Exec(ctx, query,
		data.EventID,
		data.MerchantID,
		data.SubscriptionID,
		data.UserID,
		data.EventType,
		data.Status,
		data.CancelType,
		data.Rail,
		nullableString2(data.RailSubscriptionID),
		nullableString2(data.RailTransactionID),
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
	batch, err := s.clickhouseConn.PrepareBatch(ctx, `INSERT INTO subscription_events (event_id, merchant_id, subscription_id, user_id, event_type, status, cancel_type, rail, rail_subscription_id, rail_transaction_id, price_amount, price_currency, billing_cycle_days, product_id, price_id, metadata, timestamp) VALUES`)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if err := batch.Append(d.EventID, d.MerchantID, d.SubscriptionID, d.UserID, d.EventType, d.Status, d.CancelType, d.Rail, nullableString2(d.RailSubscriptionID), nullableString2(d.RailTransactionID), d.PriceAmount, d.PriceCurrency, d.BillingCycleDays, nullableUUID(d.ProductID), nullableUUID(d.PriceID), d.Metadata, d.Timestamp); err != nil {
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
            event_id, merchant_id, subscription_id, user_id, event_type, rail,
            rail_transaction_id, amount, currency, billing_info,
            webhook_source, metadata, timestamp
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    `
	return s.clickhouseConn.Exec(ctx, query,
		data.EventID,
		data.MerchantID,
		data.SubscriptionID,
		data.UserID,
		data.EventType,
		data.Rail,
		data.RailTransactionID,
		data.Amount,
		data.Currency,
		data.BillingInfo,
		data.WebhookSource,
		data.Metadata,
		data.Timestamp,
	)
}

func (s *EventLogService) insertPaymentBatch(ctx context.Context, rows []PaymentEventData) error {
	batch, err := s.clickhouseConn.PrepareBatch(ctx, `INSERT INTO payment_events (event_id, merchant_id, subscription_id, user_id, event_type, rail, rail_transaction_id, amount, currency, billing_info, webhook_source, metadata, timestamp) VALUES`)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if err := batch.Append(d.EventID, d.MerchantID, d.SubscriptionID, d.UserID, d.EventType, d.Rail, d.RailTransactionID, d.Amount, d.Currency, d.BillingInfo, d.WebhookSource, d.Metadata, d.Timestamp); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (s *EventLogService) insertACUBatch(ctx context.Context, rows []ACUEventData) error {
	batch, err := s.clickhouseConn.PrepareBatch(ctx, `INSERT INTO acu_events (event_id, merchant_id, subscription_id, user_id, event_type, rail, rail_subscription_id, card_info, update_status, requires_action, reason, metadata, timestamp) VALUES`)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if err := batch.Append(d.EventID, d.MerchantID, d.SubscriptionID, d.UserID, d.EventType, d.Rail, d.RailSubscriptionID, d.CardInfo, d.UpdateStatus, d.RequiresAction, d.Reason, d.Metadata, d.Timestamp); err != nil {
			return err
		}
	}
	return batch.Send()
}

func (s *EventLogService) insertChargebackBatch(ctx context.Context, rows []ChargebackEventData) error {
	batch, err := s.clickhouseConn.PrepareBatch(ctx, `INSERT INTO chargeback_events (event_id, merchant_id, chargeback_id, batch_id, subscription_id, user_id, event_type, rail, rail_transaction_id, amount, currency, chargeback_type, reason, status, metadata, timestamp) VALUES`)
	if err != nil {
		return err
	}
	for _, d := range rows {
		if err := batch.Append(d.EventID, d.MerchantID, d.ChargebackID, d.BatchID, nullableUUID(d.SubscriptionID), nullableString2(d.UserID), d.EventType, d.Rail, nullableString2(d.RailTransactionID), d.Amount, d.Currency, d.ChargebackType, d.Reason, d.Status, d.Metadata, d.Timestamp); err != nil {
			return err
		}
	}
	return batch.Send()
}

// initClickHouseConnection creates a connection to ClickHouse
func initClickHouseConnection(ctx context.Context, cfg *config.ClickHouseConfig) (driver.Conn, error) {
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
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("failed to ping ClickHouse: %w", err)
	}

	log.Info("Successfully connected to ClickHouse for billing event logging")
	return conn, nil
}
