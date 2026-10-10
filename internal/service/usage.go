package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/modules/money"
)

// UsageIdempotencyKey is the reproducible coordinate for one host-reported
// usage event.
type UsageIdempotencyKey = money.IdempotencyKey

// NewUsageIdempotencyKey builds a usage key whose operation is bound to
// eventType. source and sourceID must identify the same logical event across
// retries.
func NewUsageIdempotencyKey(eventType, source, sourceID string) (UsageIdempotencyKey, error) {
	return money.NewIdempotencyKey(money.UsageOperation(eventType), source, sourceID)
}

// RecordUsageInput is one host-reported metered usage event (#797).
//
// Key is REQUIRED and is the idempotency coordinate within
// (merchant, payer, currency): claimed under the payer's spend lock by a lookup
// over the ingest window, and for a priced event by the ledger's operation
// coordinate for good. Build it with
// NewUsageIdempotencyKey(EventType, source, sourceID); its operation must match
// EventType, so two different event types at one
// (source, source_id) are two charges rather than one collision (or#894). Both
// halves must be REPRODUCIBLE by the caller across retries of the same logical
// event: a value minted per attempt passes every check here and guarantees
// nothing.
//
// A replay under the same key with the SAME Amount returns success and neither
// re-records nor re-charges. A replay with a DIFFERENT Amount is refused with
// money.ErrIdempotencyKeyReused (or#891) rather than answered with the first
// event, so a corrected charge cannot silently keep the original number.
//
// Amount is the host-priced cost in the currency's internal precision;
// 0 records a free/metered-only event whose Dimensions still aggregate
// through rate-card rating (gauge meters report unit-second quantities).
//
// Failed records usage that failed (see billing.UsageFailed): the windows it
// counts toward are resolved here from InvokerType.
type RecordUsageInput struct {
	CustomerID  identity.CustomerID
	Invoker     string
	InvokerType billing.InvokerType
	Failed      bool
	Currency    string
	EventType   string
	Dimensions  map[string]int64
	Amount      int64
	Resource    string
	Metadata    map[string]any
	Key         UsageIdempotencyKey
	// OccurredAt places the event in its rating window (zero = now).
	OccurredAt time.Time
}

// RecordUsage durably records a metered usage event (and debits the ledger for
// a non-zero Amount) via money.RecordUsage (#289/#797). Idempotent on
// (merchant, payer, currency, usage:<event_type>, source, source_id); a replay
// carrying a different Amount returns money.ErrIdempotencyKeyReused.
func (s *Service) RecordUsage(ctx context.Context, in RecordUsageInput) (*billing.UsageEvent, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if in.CustomerID.IsZero() {
		return nil, fmt.Errorf("customer_id required")
	}
	cur, err := requireCurrency(in.Currency)
	if err != nil {
		return nil, err
	}
	metadata := in.Metadata
	if in.Resource = strings.TrimSpace(in.Resource); in.Resource != "" {
		if metadata == nil {
			metadata = map[string]any{}
		}
		if _, ok := metadata["resource"]; !ok {
			metadata["resource"] = in.Resource
		}
	}
	payer := in.CustomerID
	params := money.RecordUsageParams{
		Payer:      &payer,
		Invoker:    strings.TrimSpace(in.Invoker),
		Currency:   cur,
		EventType:  strings.TrimSpace(in.EventType),
		Dimensions: in.Dimensions,
		Amount:     in.Amount,
		Key:        in.Key,
		Metadata:   metadata,
		OccurredAt: in.OccurredAt,
		Failed:     in.Failed,
		Delegated:  in.InvokerType == billing.InvokerTypeDelegated,
	}
	if in.Failed {
		if params.FailedWindows, err = s.failedUsageWindows(ctx, payer, in.InvokerType, cur); err != nil {
			return nil, err
		}
	}
	ev, err := s.moneyService().RecordUsage(ctx, params)
	if err != nil {
		return nil, err
	}
	out := &billing.UsageEvent{
		ID: billing.UsageEventID(ev.ID), CustomerID: billing.CustomerID(ev.CustomerID), Invoker: ev.Invoker, Currency: ev.Currency,
		EventType: ev.EventType, Dimensions: ev.Dimensions, Outcome: billing.UsageOutcome(ev.Outcome), Amount: ev.Amount, ForgivenAmount: ev.ForgivenAmount,
		Resource: ev.Resource, Metadata: ev.Metadata,
		Source: ev.Source, SourceID: ev.SourceID, OccurredAt: ev.OccurredAt, CreatedAt: ev.CreatedAt, Replayed: ev.Replayed,
	}
	if ev.LedgerTransferID != nil {
		txn := billing.BalanceTransactionID(*ev.LedgerTransferID)
		out.BalanceTransactionID = &txn
	}
	return out, nil
}

// FinalizeInvoice closes the rating window [from, to) for one payer: the
// metered rating sweep rates reported usage through the catalog rate cards
// (allowances + per-period watermarks) and the resulting statement is
// finalized as an invoice (#797 public export). Idempotent per window.
func (s *Service) FinalizeInvoice(ctx context.Context, payer identity.CustomerID, currency string, from, to time.Time) (*billing.Invoice, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	cur, err := requireCurrency(currency)
	if err != nil {
		return nil, err
	}
	inv, err := s.moneyService().FinalizeInvoice(ctx, payer, cur, from, to)
	if err != nil {
		return nil, err
	}
	out := InvoiceView(inv, s.now())
	if err := s.markDelinquent(ctx, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
