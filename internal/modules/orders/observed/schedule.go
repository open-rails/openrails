package observed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// SeedSchedule freezes the future recurring agreement when an accepted initial
// order is fulfilled. The caller supplies its accepted provider cadence and
// recurring amount (which can differ from a trial's initial charge). It must
// never be called to reconstruct a schedule from today's subscription period.
func SeedSchedule(ctx context.Context, d *db.DB, orderID, subscriptionID uuid.UUID, firstDue time.Time, cadenceHours int, recurringAmount int64, now time.Time) error {
	if firstDue.IsZero() || cadenceHours <= 0 || cadenceHours > math.MaxInt32 || recurringAmount <= 0 {
		return errors.New("accepted provider schedule terms are incomplete")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := d.Gen(ctx)
	order, err := q.GetOrder(ctx, gen.GetOrderParams{MerchantID: mid.UUID(), ID: orderID})
	if err != nil {
		return err
	}
	if order.CollectionOwner != OwnerNMISchedule && order.CollectionOwner != OwnerProvider || order.PspID == nil {
		return errors.New("accepted schedule order has no provider collection owner")
	}
	line, err := q.GetOrderLineBySubscription(ctx, gen.GetOrderLineBySubscriptionParams{MerchantID: mid.UUID(), OrderID: orderID, SubscriptionID: subscriptionID})
	if err != nil {
		return fmt.Errorf("accepted schedule subscription line: %w", err)
	}
	if line.PriceID == nil || line.ProductID == nil {
		return errors.New("accepted schedule line must identify its product and price")
	}
	unit := recurringAmount
	if line.Quantity != nil {
		unit /= int64(*line.Quantity)
	}
	bill := Bill{PSPID: *order.PspID, CustomerID: order.CustomerID, SubscriptionID: &subscriptionID, Owner: order.CollectionOwner,
		Kind: "renewal", SourceKind: "provider_cycle", Currency: order.Currency, Total: recurringAmount,
		Lines: []Line{{PriceID: line.PriceID, ProductID: line.ProductID, Description: line.Description,
			Quantity: models.DerefIntPtr(line.Quantity), UnitAmount: unit, Amount: recurringAmount,
			BillingIntervalHours: &cadenceHours, AccessDurationHours: models.DerefIntPtr(line.AccessDurationHours)}}}
	raw, err := json.Marshal(bill)
	if err != nil {
		return err
	}
	_, err = q.InsertProviderSchedule(ctx, gen.InsertProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID,
		CustomerID: order.CustomerID, PspID: *order.PspID, InitialOrderID: orderID, FirstDueAt: firstDue.UTC(), CadenceHours: int32(cadenceHours), AcceptedBill: raw, Now: now})
	if err != nil {
		return err
	}
	stored, err := q.GetProviderSchedule(ctx, gen.GetProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID})
	if err != nil {
		return err
	}
	if stored.InitialOrderID != orderID || stored.CustomerID != order.CustomerID || stored.PspID != *order.PspID || !stored.FirstDueAt.Equal(firstDue) || stored.CadenceHours != int32(cadenceHours) || !sameJSON(stored.AcceptedBill, raw) {
		return ErrConflict
	}
	return nil
}

// ScheduleReceipt is an executed provider attempt together with a verified
// schedule boundary. Retry counters and changed retry dates are not evidence.
type ScheduleReceipt struct {
	TransactionID             string
	Amount                    int64
	Currency                  string
	OccurredAt, NextBillingAt time.Time
	Approved                  bool
}

// ScheduleBill resolves a receipt to a previously accepted cycle. Replays use
// the persistent binding even after the provider moves its next billing date.
// New ambiguous receipts remain unresolved; a transaction ID alone never
// creates an obligation.
func ScheduleBill(ctx context.Context, d *db.DB, sub *models.Subscription, receipt ScheduleReceipt) (Bill, error) {
	if sub == nil || receipt.TransactionID == "" || receipt.OccurredAt.IsZero() {
		return Bill{}, errors.New("provider cycle receipt is incomplete")
	}
	q := d.Gen(ctx)
	schedule, err := q.GetProviderSchedule(ctx, gen.GetProviderScheduleParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID})
	if err != nil {
		return Bill{}, fmt.Errorf("provider cycle has no accepted enrollment agreement: %w", err)
	}
	var bill Bill
	if err := json.Unmarshal(schedule.AcceptedBill, &bill); err != nil {
		return Bill{}, err
	}
	if schedule.PspID != sub.PspID || schedule.CustomerID != sub.CustomerID || bill.SubscriptionID == nil || *bill.SubscriptionID != sub.ID || bill.Owner != string(sub.CollectionPolicy) || receipt.Amount != bill.Total || !strings.EqualFold(receipt.Currency, bill.Currency) {
		return Bill{}, ErrConflict
	}
	bound, err := q.GetProviderCycleReceipt(ctx, gen.GetProviderCycleReceiptParams{MerchantID: sub.MerchantID, PspID: sub.PspID, TransactionID: receipt.TransactionID})
	var due time.Time
	switch {
	case err == nil:
		if bound.SubscriptionID != sub.ID || bound.Amount != receipt.Amount || bound.Currency != bill.Currency || bound.Approved != receipt.Approved || !bound.OccurredAt.Equal(receipt.OccurredAt) {
			return Bill{}, ErrConflict
		}
		due = bound.DueAt
	case db.IsNotFound(err):
		due, err = scheduleReceiptCycle(schedule.FirstDueAt, int(schedule.CadenceHours), receipt)
		if err != nil {
			return Bill{}, err
		}
		if err := q.InsertProviderCycleReceipt(ctx, gen.InsertProviderCycleReceiptParams{MerchantID: sub.MerchantID, PspID: sub.PspID,
			TransactionID: receipt.TransactionID, SubscriptionID: sub.ID, DueAt: due, Amount: receipt.Amount, Currency: bill.Currency,
			OccurredAt: receipt.OccurredAt, Approved: receipt.Approved}); err != nil {
			return Bill{}, err
		}
	default:
		return Bill{}, err
	}
	bill.SourceKey = sub.ID.String() + ":" + due.UTC().Format(time.RFC3339Nano)
	bill.ProviderReference = sub.RailSubscriptionID
	bill.PeriodStart, bill.PeriodEnd, bill.DueAt = due, due.Add(time.Duration(schedule.CadenceHours)*time.Hour), due
	return bill, nil
}

// NMI and CCBill expose billing days. Match that declared boundary to the
// immutable accepted cadence; do not round an arbitrary changed retry date or
// infer from the subscription's mutable paid-through value.
func scheduleReceiptCycle(firstDue time.Time, cadenceHours int, receipt ScheduleReceipt) (time.Time, error) {
	if cadenceHours <= 0 || cadenceHours%24 != 0 || int64(cadenceHours) > math.MaxInt64/int64(time.Hour) || receipt.NextBillingAt.IsZero() {
		return time.Time{}, errors.New("provider receipt lacks a verified accepted schedule boundary")
	}
	period := time.Duration(cadenceHours) * time.Hour
	anchor := firstDue.UTC().Truncate(24 * time.Hour)
	next := receipt.NextBillingAt.UTC().Truncate(24 * time.Hour)
	delta := next.Sub(anchor)
	if delta < 0 || delta%period != 0 {
		return time.Time{}, errors.New("provider schedule boundary differs from the accepted cadence")
	}
	index := delta/period - 1
	day := receipt.OccurredAt.UTC().Truncate(24 * time.Hour)
	// On a decline the provider may leave the next billing boundary unchanged.
	// The executed receipt and the accepted schedule must still agree on one
	// half-open cycle; a scheduled retry timestamp cannot supply this boundary.
	if !receipt.Approved && !day.Before(next) && day.Before(next.Add(period)) {
		index++
	}
	if index < 0 {
		return time.Time{}, errors.New("provider receipt predates the first accepted renewal")
	}
	due := firstDue.UTC().Add(index * period)
	start := due.Truncate(24 * time.Hour)
	if day.Before(start) || !day.Before(start.Add(period)) {
		return time.Time{}, errors.New("provider receipt and schedule cannot prove one accepted cycle")
	}
	return due, nil
}
