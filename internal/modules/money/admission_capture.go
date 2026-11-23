package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// UsageEstimate is an admission's usage estimate as it is stored and compared.
func UsageEstimate(est billing.UsageEstimate) ([]byte, error) {
	est.EventType = strings.TrimSpace(est.EventType)
	if est.EventType == "" {
		return nil, apperr.Invalidf("usage.event_type required").WithParam("usage.event_type")
	}
	if est.Amount != nil && *est.Amount < 0 {
		return nil, apperr.Invalidf("usage.amount must be nonnegative").WithParam("usage.amount")
	}
	return json.Marshal(est)
}

// RateUsageEstimate prices an admission's usage estimate at the customer's
// current period quantities, without advancing them, and answers the rate
// cards that priced it, to be pinned. An event type no rate card prices is
// not an estimate of money: it is refused, as a matrix gap is. The caller
// holds the customer's spend lock.
func (s *MoneyService) RateUsageEstimate(ctx context.Context, q *gen.Queries, payer identity.CustomerID, currency string, est billing.UsageEstimate) (int64, []byte, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return 0, nil, err
	}
	eventType := strings.TrimSpace(est.EventType)
	cards, err := usageCards(ctx, q, mid.UUID(), eventType, currency)
	if err != nil {
		return 0, nil, usageParam(err)
	}
	if len(cards) == 0 {
		return 0, nil, usageNotPriced("usage.event_type", "no rate card prices event type %q", eventType)
	}
	quantities, err := usageQuantities(cards, eventFields{Dimensions: est.Dimensions, Metadata: est.Metadata, Amount: est.Amount})
	if err != nil {
		return 0, nil, usageParam(err)
	}
	period, err := s.usagePeriodStart(ctx, q, mid.UUID(), payer.UUID(), currency, s.now())
	if err != nil {
		return 0, nil, err
	}
	var amount int64
	for _, u := range quantities {
		before, err := q.GetMeterPeriodQuantity(ctx, gen.GetMeterPeriodQuantityParams{
			MerchantID: mid.UUID(), CustomerID: payer.UUID(), Currency: currency, MeterKey: u.card.MeterKey, Cell: u.cell, PeriodStartsAt: period,
		})
		if err != nil {
			return 0, nil, err
		}
		charge, err := u.price(before)
		if err != nil {
			return 0, nil, err
		}
		if amount > maxQuantity-charge {
			return 0, nil, fmt.Errorf("usage estimate amount overflows int64")
		}
		amount += charge
	}
	pinned, err := json.Marshal(cards)
	return amount, pinned, err
}

// usageParam names a usage field of an admission or capture as the request
// sends it: under usage.
func usageParam(err error) error {
	var refusal *apperr.Error
	if errors.As(err, &refusal) && refusal.Param != "" && !strings.HasPrefix(refusal.Param, "usage.") {
		return refusal.WithParam("usage." + refusal.Param)
	}
	return err
}

// captureTerms are what a capture's first call fixes: its usage, and the
// host-stated amount when it named one.
type captureTerms struct {
	billing.CaptureUsage
	Amount *int64 `json:"amount,omitempty,string"`
}

// CaptureAdmission settles an admitted request in one transaction under the
// customer's spend lock: its hold leaves the held total and its charge
// posts, balance first and the rest owed, never clamped to the hold. Usage
// is priced by the admission's pinned rate cards (the cards in force when it
// pinned none), advancing quantities. Dynamic cards charge Amount. The first
// capture fixes its terms: an exact retry answers its receipt, even after a
// price change. Payer and invoker come from the admission.
func (s *MoneyService) CaptureAdmission(ctx context.Context, params billing.CaptureAdmissionParams) (*billing.CaptureReceipt, error) {
	requestID := strings.TrimSpace(params.RequestID)
	if requestID == "" {
		return nil, apperr.Invalidf("request_id required").WithParam("request_id")
	}
	if params.Amount != nil && *params.Amount < 0 {
		return nil, apperr.Invalidf("amount must be nonnegative").WithParam("amount")
	}
	u := params.Usage
	u.EventType = strings.TrimSpace(u.EventType)
	u.Resource = strings.TrimSpace(u.Resource)
	u.Source = strings.TrimSpace(u.Source)
	u.SourceID = strings.TrimSpace(u.SourceID)
	if u.EventType != "" {
		if u.Source == "" {
			u.Source = "admit"
		}
		if u.SourceID == "" {
			u.SourceID = requestID
		}
	}
	if u.EventType == "" {
		return nil, apperr.Invalidf("a capture names its usage").WithParam("usage.event_type")
	}
	terms, err := json.Marshal(captureTerms{CaptureUsage: u, Amount: params.Amount})
	if err != nil {
		return nil, fmt.Errorf("capture terms: %w", err)
	}
	gate := spendgate.New(s.db)
	gate.SetClock(s.now)
	s.db.EnsurePartitions(ctx, s.now())
	var receipt *billing.CaptureReceipt
	err = gate.WithOperation(ctx, requestID, func(ctx context.Context, d *db.DB, row gen.BillingLimitHold) error {
		txSvc := s.WithDatabase(d)
		if row.State == "captured" {
			var rerr error
			receipt, rerr = txSvc.captureReplay(ctx, d.Gen(ctx), row, terms, u)
			return rerr
		}
		var cerr error
		receipt, cerr = txSvc.captureOnce(ctx, d.Gen(ctx), row, terms, u, params.Amount)
		return cerr
	})
	return receipt, err
}

func (s *MoneyService) captureOnce(ctx context.Context, q *gen.Queries, row gen.BillingLimitHold, terms []byte, u billing.CaptureUsage, stated *int64) (*billing.CaptureReceipt, error) {
	original, err := spendgate.OriginalTerms(row)
	if err != nil {
		return nil, err
	}
	payer := identity.CustomerID(row.CustomerID)
	now := s.now()
	requestID := deref(row.RequestID)
	receipt := &billing.CaptureReceipt{RequestID: requestID, CustomerID: billing.CustomerID(row.CustomerID), Currency: row.Currency,
		EstimatedAmount: derefInt(row.EstimatedAmount), Charges: []billing.UsageCharge{}}

	// The admitted event type remains binding even when the host states money.
	if len(row.EstimateUsage) > 0 {
		var admitted billing.UsageEstimate
		if err := json.Unmarshal(row.EstimateUsage, &admitted); err != nil {
			return nil, fmt.Errorf("decode admission estimate: %w", err)
		}
		if admitted.EventType != u.EventType {
			return nil, apperr.Invalidf("usage.event_type must be the admitted %q", admitted.EventType).WithParam("usage.event_type")
		}
	}
	cards, err := s.captureCards(ctx, q, row, u.EventType)
	if err != nil {
		return nil, err
	}
	quantities, err := usageQuantities(cards, eventFields{Dimensions: u.Dimensions, Metadata: u.Metadata, Amount: stated})
	if err != nil {
		return nil, usageParam(err)
	}
	// The coordinate is claimed here, under the customer spend lock.
	keyFrom, keyTo := usageKeyWindow(now)
	_, err = q.GetUsageEventByCoords(ctx, gen.GetUsageEventByCoordsParams{
		MerchantID: row.MerchantID, CustomerID: row.CustomerID, Currency: row.Currency,
		EventType: u.EventType, Source: u.Source, SourceID: u.SourceID, OccurredFrom: keyFrom, OccurredTo: keyTo,
	})
	if err == nil {
		return nil, fmt.Errorf("%w: capture usage coordinate belongs to another operation", ErrIdempotencyKeyReused)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// What the work charges is priced first; the hold leaves the held total
	// before the charge posts, in the same transaction, so the charge draws
	// the balance the hold reserved. A late capture after release or expiry
	// still records the operation.
	eventID := uuidutil.NewV7()
	charges, err := s.priceUsageTx(ctx, q, payer, row.Currency, now, quantities)
	if err != nil {
		return nil, err
	}
	for _, c := range charges {
		receipt.Amount += c.Amount
	}
	if _, err := q.CaptureRequestHold(ctx, gen.CaptureRequestHoldParams{
		MerchantID: row.MerchantID, ID: row.ID, CreatedAt: row.CreatedAt, Amount: receipt.Amount, AsOf: now, CaptureTerms: terms,
	}); err != nil {
		return nil, err
	}
	// The usage settles into the windows its hold was issued in, in full,
	// and into the current windows of limits the hold did not take.
	if err := s.settleLimits(ctx, q, row, receipt.Amount, limits.Usage{EventType: u.EventType, Dimensions: u.Dimensions, Metadata: u.Metadata}, now); err != nil {
		return nil, err
	}
	transfer, err := s.postChargesTx(ctx, q, payer, original.Invoker, row.Currency, eventID, charges)
	if err != nil {
		return nil, err
	}
	if transfer != nil {
		txn := billing.BalanceTransactionID(*transfer)
		receipt.BalanceTransactionID = &txn
	}
	dimensions, err := toJSONBC(u.Dimensions)
	if err != nil {
		return nil, err
	}
	metadata, err := toJSONBC(u.Metadata)
	if err != nil {
		return nil, err
	}
	chargesJSON, err := json.Marshal(charges)
	if err != nil {
		return nil, err
	}
	err = q.InsertUsageEvent(ctx, gen.InsertUsageEventParams{
		ID: eventID, MerchantID: row.MerchantID, CustomerID: row.CustomerID,
		InvokerID: original.Invoker, Currency: row.Currency, Resource: nilIfEmpty(u.Resource),
		EventType: u.EventType, Dimensions: dimensions, Metadata: metadata, Amount: receipt.Amount, StatedAmount: stated,
		Source: u.Source, SourceID: u.SourceID, LedgerTransferID: transfer,
		OccurredAt: now, CreatedAt: now, Outcome: UsageSucceeded, Charges: chargesJSON,
	})
	if err != nil {
		var conflict *pgconn.PgError
		if errors.As(err, &conflict) && conflict.Code == "23505" && conflict.ConstraintName == "usage_events_idem_key" {
			return nil, fmt.Errorf("%w: capture usage coordinate belongs to another operation", ErrIdempotencyKeyReused)
		}
		return nil, err
	}
	id := billing.UsageEventID(eventID)
	receipt.UsageEventID = &id
	receipt.Charges = billingCharges(charges)
	return receipt, nil
}

// captureCards are the rate cards a capture is priced by: the admission's
// pinned cards, or those in force when it pinned none. A capture exists to
// settle money, so usage no card prices is refused.
func (s *MoneyService) captureCards(ctx context.Context, q *gen.Queries, row gen.BillingLimitHold, eventType string) ([]ratingCard, error) {
	var cards []ratingCard
	if len(row.RateCards) > 0 {
		if err := json.Unmarshal(row.RateCards, &cards); err != nil {
			return nil, fmt.Errorf("decode pinned rate cards: %w", err)
		}
	} else {
		var err error
		if cards, err = usageCards(ctx, q, row.MerchantID, eventType, row.Currency); err != nil {
			return nil, usageParam(err)
		}
	}
	if len(cards) == 0 {
		return nil, usageNotPriced("usage.event_type", "no rate card prices event type %q", eventType)
	}
	return cards, nil
}

// captureReplay answers a captured admission's receipt for a retry with its
// exact terms.
func (s *MoneyService) captureReplay(ctx context.Context, q *gen.Queries, row gen.BillingLimitHold, terms []byte, u billing.CaptureUsage) (*billing.CaptureReceipt, error) {
	matches, err := q.RequestHoldCaptureTermsMatch(ctx, gen.RequestHoldCaptureTermsMatchParams{
		MerchantID: row.MerchantID, ID: row.ID, CreatedAt: row.CreatedAt, CaptureTerms: terms,
	})
	if err != nil {
		return nil, err
	}
	requestID := deref(row.RequestID)
	if !matches {
		return nil, &IdempotencyConflict{Operation: string(OpCapture), Source: "admit", SourceID: requestID,
			Field: "usage", Committed: "original capture terms", Retried: "different capture terms"}
	}
	receipt := &billing.CaptureReceipt{RequestID: requestID, CustomerID: billing.CustomerID(row.CustomerID), Currency: row.Currency,
		EstimatedAmount: derefInt(row.EstimatedAmount), Amount: derefInt(row.CapturedAmount), Charges: []billing.UsageCharge{}, Replayed: true}
	if row.CapturedAt != nil {
		from, to := usageKeyWindow(*row.CapturedAt)
		event, err := q.GetUsageEventByCoords(ctx, gen.GetUsageEventByCoordsParams{
			MerchantID: row.MerchantID, CustomerID: row.CustomerID, Currency: row.Currency,
			EventType: u.EventType, Source: u.Source, SourceID: u.SourceID, OccurredFrom: from, OccurredTo: to,
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			ev, err := UsageEventFromRow(event)
			if err != nil {
				return nil, err
			}
			id := billing.UsageEventID(ev.ID)
			receipt.UsageEventID, receipt.Charges = &id, billingCharges(ev.Charges)
			if ev.LedgerTransferID != nil {
				txn := billing.BalanceTransactionID(*ev.LedgerTransferID)
				receipt.BalanceTransactionID = &txn
			}
			return receipt, nil
		}
	}
	return receipt, nil
}

// billingCharges is the wire form of an event's meter charges.
func billingCharges(in []models.UsageCharge) []billing.UsageCharge {
	out := make([]billing.UsageCharge, len(in))
	for i, c := range in {
		out[i] = billing.UsageCharge{MeterKey: c.MeterKey, Model: c.Model, Quantity: c.Quantity, Amount: c.Amount}
		if c.Cell != "" {
			cell := c.Cell
			out[i].Cell = &cell
		}
	}
	return out
}

// settleLimits settles a captured hold's usage: its parts' windows take the
// charge (a currency limit or spend window) or the units its rate cards
// count (a named unit); declared limits it took no part of settle in their
// current windows. Nothing is clamped.
func (s *MoneyService) settleLimits(ctx context.Context, q *gen.Queries, row gen.BillingLimitHold, amount int64, u limits.Usage, now time.Time) error {
	parts, err := q.ListHoldParts(ctx, gen.ListHoldPartsParams{MerchantID: row.MerchantID, HoldID: row.ID, HoldCreatedAt: row.CreatedAt})
	if err != nil {
		return err
	}
	defs, err := limits.Definitions(ctx, q, row.MerchantID)
	if err != nil {
		return err
	}
	if len(defs) == 0 && len(parts) == 0 {
		return nil
	}
	meters, err := limits.Meters(ctx, q, row.MerchantID)
	if err != nil {
		return err
	}
	reader := limits.Customer{Q: q, MerchantID: row.MerchantID, CustomerID: row.CustomerID, Now: now,
		Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
			return s.PricingPeriod(ctx, q, row.MerchantID, row.CustomerID, unit, at)
		}}
	capacities, err := reader.Capacities(ctx)
	if err != nil {
		return err
	}
	byKey := make(map[string]limits.Definition, len(defs))
	for _, def := range defs {
		byKey[def.Key] = def
	}
	held := map[string]bool{}
	var settlements []limits.Settlement
	for _, p := range parts {
		held[p.LimitKey] = true
		w := limits.Window{StartsAt: p.WindowStartsAt.UTC(), EndsAt: p.WindowEndsAt, Open: true}
		switch def, declared := byKey[p.LimitKey]; {
		case p.LimitKey == catalog.BalanceLimit:
		case strings.HasPrefix(p.LimitKey, limits.SpendKeyPrefix):
			settlements = append(settlements, limits.Settlement{Key: p.LimitKey, Reset: catalog.LimitReset{Kind: catalog.ResetFixed}, Window: w, KeepCapacity: true, Units: amount})
		case declared:
			units := amount
			if def.Unit != row.Currency {
				if units, err = limits.Units(def, meters, u); err != nil {
					return usageParam(err)
				}
			}
			settlements = append(settlements, limits.Settlement{Key: def.Key, Reset: def.Reset, Window: w, Capacity: capacities[def.Key], Units: units})
		}
	}
	current, err := reader.Settlements(ctx, defs, meters, row.Currency, amount, u, held)
	if err != nil {
		return usageParam(err)
	}
	settlements = append(settlements, current...)
	return limits.Holder{Q: q, MerchantID: row.MerchantID, CustomerID: row.CustomerID, Now: now}.Settle(ctx, settlements)
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
