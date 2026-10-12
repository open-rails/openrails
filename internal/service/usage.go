package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/models"
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

// RecordUsageInput is one host-reported metered usage event.
//
// Key is required: the idempotency coordinate within (merchant, payer,
// currency), claimed under the payer's spend lock over the ingest window and,
// for a priced event, permanently by the ledger. Build it with
// NewUsageIdempotencyKey(EventType, source, sourceID); its operation must match
// EventType, so two event types at one (source, source_id) are two charges,
// not a collision. Both halves must be reproducible across retries of the same
// logical event: a value minted per attempt passes every check and guarantees
// nothing.
//
// A replay with the same Amount succeeds without re-recording or re-charging;
// a different Amount is refused with money.ErrIdempotencyKeyReused, so a
// corrected charge never silently keeps the original number.
//
// Amount is the host-priced cost in the currency's internal precision; 0
// leaves the event to its meters' rate cards, which price its Dimensions (and
// metadata) when it is recorded.
//
// Failed records failed usage (see billing.UsageFailed); the windows it counts
// toward are resolved from InvokerType.
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

// RecordUsage durably records a metered usage event and posts its charge: a
// non-zero Amount, else what its meters' rate cards charge it. Idempotent on
// (merchant, payer, currency, usage:<event_type>, source, source_id); a
// replay with a different Amount returns money.ErrIdempotencyKeyReused.
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
		Source: ev.Source, SourceID: ev.SourceID, Charges: usageCharges(ev.Charges), OccurredAt: ev.OccurredAt, CreatedAt: ev.CreatedAt, Replayed: ev.Replayed,
	}
	if ev.LedgerTransferID != nil {
		txn := billing.BalanceTransactionID(*ev.LedgerTransferID)
		out.BalanceTransactionID = &txn
	}
	return out, nil
}

// usageCharges is the wire form of an event's meter charges.
func usageCharges(in []models.UsageCharge) []billing.UsageCharge {
	out := make([]billing.UsageCharge, len(in))
	for i, c := range in {
		out[i] = billing.UsageCharge{MeterKey: c.MeterKey, Quantity: c.Quantity, Amount: c.Amount}
		if c.Cell != "" {
			cell := c.Cell
			out[i].Cell = &cell
		}
	}
	return out
}
