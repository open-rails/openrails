package money

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/shared/apperr"
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

// PinnedCards are the rate cards in force for an event type in a currency,
// as a hold pins them and a gate's view prices with them; nil when none
// prices it.
func (s *MoneyService) PinnedCards(ctx context.Context, q *gen.Queries, eventType, currency string) ([]byte, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	cards, err := usageCards(ctx, q, mid.UUID(), strings.TrimSpace(eventType), normalizeCurrency(currency))
	var coded *apperr.Error
	switch {
	case errors.As(err, &coded):
		return nil, nil
	case err != nil:
		return nil, err
	case len(cards) == 0:
		return nil, nil
	}
	return json.Marshal(cards)
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

// CaptureHoldTx captures a gate's request hold with a usage item of its
// lease, in the caller's transaction under the customer's spend lock, as
// CaptureAdmission would. A hold already captured is a conflict: the item's
// own number answers a resend.
func (s *MoneyService) CaptureHoldTx(ctx context.Context, q *gen.Queries, row gen.BillingLimitHold, u billing.CaptureUsage, stated *int64) (*billing.CaptureReceipt, error) {
	if row.State == "captured" {
		return nil, apperr.New(http.StatusConflict, billing.CodeResourceConflict, "the request hold was already captured").WithParam("request_id")
	}
	u.EventType, u.Resource = strings.TrimSpace(u.EventType), strings.TrimSpace(u.Resource)
	u.Source, u.SourceID = cmp.Or(strings.TrimSpace(u.Source), "admit"), cmp.Or(strings.TrimSpace(u.SourceID), row.RequestID)
	terms, err := json.Marshal(captureTerms{CaptureUsage: u, Amount: stated})
	if err != nil {
		return nil, fmt.Errorf("capture terms: %w", err)
	}
	return s.captureOnce(ctx, q, row, terms, u, stated)
}

func (s *MoneyService) captureOnce(ctx context.Context, q *gen.Queries, row gen.BillingLimitHold, terms []byte, u billing.CaptureUsage, stated *int64) (*billing.CaptureReceipt, error) {
	original, err := spendgate.OriginalTerms(row)
	if err != nil {
		return nil, err
	}
	now := s.now()
	requestID := row.RequestID
	receipt := &billing.CaptureReceipt{RequestID: requestID, CustomerID: billing.CustomerID(row.CustomerID), Currency: catalog.Currency(row.Currency),
		EstimatedAmount: row.EstimatedAmount, Charges: []billing.UsageCharge{}}

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
	parts, err := q.ListHoldParts(ctx, gen.ListHoldPartsParams{MerchantID: row.MerchantID, HoldID: row.ID, HoldCreatedAt: row.CreatedAt})
	if err != nil {
		return nil, err
	}
	hold := &SettleHold{ID: row.ID, CreatedAt: row.CreatedAt}
	for _, p := range parts {
		hold.Parts = append(hold.Parts, HoldPart{Key: p.LimitKey, Unit: p.Unit, WindowStartsAt: p.WindowStartsAt, WindowEndsAt: p.WindowEndsAt, Held: p.Held})
	}
	// Usage is priced by the admission's pinned cards, or those in force when
	// it pinned none; a capture settles money, so usage no card prices is
	// refused.
	batch, err := s.PriceUsageTx(ctx, q, []UsageItem{{
		Customer: identity.CustomerID(row.CustomerID), Invoker: original.Invoker, Currency: row.Currency, EventType: u.EventType,
		Dimensions: u.Dimensions, Metadata: u.Metadata, Amount: stated, OccurredAt: now,
		Cards: row.RateCards, RequireCards: true, Hold: hold,
	}})
	if err != nil {
		return nil, err
	}
	result := batch.Results[0]
	if result.Err != nil {
		return nil, usageParam(result.Err)
	}
	receipt.Amount, receipt.Charges = result.Amount, result.Charges
	charges, err := json.Marshal(result.Charges)
	if err != nil {
		return nil, err
	}
	// The hold leaves the held total before the usage settles, in the same
	// transaction, so its accrual takes the balance the hold reserved. A late
	// capture after release or expiry still settles.
	if _, err := q.CaptureRequestHold(ctx, gen.CaptureRequestHoldParams{
		MerchantID: row.MerchantID, ID: row.ID, CreatedAt: row.CreatedAt, Amount: receipt.Amount, CapturedCharges: charges, AsOf: now, CaptureTerms: terms,
	}); err != nil {
		return nil, err
	}
	if err := s.SettleUsageTx(ctx, q, batch); err != nil {
		return nil, err
	}
	return receipt, nil
}

// captureReplay answers a captured admission's receipt for a retry with its
// exact terms. Its balance transaction is its accrual's first posting after
// the capture, once one posted.
func (s *MoneyService) captureReplay(ctx context.Context, q *gen.Queries, row gen.BillingLimitHold, terms []byte, _ billing.CaptureUsage) (*billing.CaptureReceipt, error) {
	matches, err := q.RequestHoldCaptureTermsMatch(ctx, gen.RequestHoldCaptureTermsMatchParams{
		MerchantID: row.MerchantID, ID: row.ID, CreatedAt: row.CreatedAt, CaptureTerms: terms,
	})
	if err != nil {
		return nil, err
	}
	requestID := row.RequestID
	if !matches {
		return nil, &IdempotencyConflict{Operation: string(OpCapture), Source: "admit", SourceID: requestID,
			Field: "usage", Committed: "original capture terms", Retried: "different capture terms"}
	}
	receipt := &billing.CaptureReceipt{RequestID: requestID, CustomerID: billing.CustomerID(row.CustomerID), Currency: catalog.Currency(row.Currency),
		EstimatedAmount: row.EstimatedAmount, Amount: derefInt(row.CapturedAmount), Charges: []billing.UsageCharge{}, Replayed: true}
	if len(row.CapturedCharges) > 0 {
		if err := json.Unmarshal(row.CapturedCharges, &receipt.Charges); err != nil {
			return nil, fmt.Errorf("decode captured charges: %w", err)
		}
	}
	if row.CapturedAt == nil || receipt.Amount == 0 {
		return receipt, nil
	}
	posting, err := s.accrualPostingSince(ctx, q, row.MerchantID, row.CustomerID, row.Currency, *row.CapturedAt)
	if err != nil {
		return nil, err
	}
	if posting != nil {
		txn := billing.BalanceTransactionID(*posting)
		receipt.BalanceTransactionID = &txn
	}
	return receipt, nil
}

func deref(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
