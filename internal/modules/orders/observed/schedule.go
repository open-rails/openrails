package observed

import (
	"context"
	"crypto/sha256"
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
	firstDue = firstDue.UTC().Truncate(time.Microsecond)
	if firstDue.IsZero() || !validCadence(cadenceHours, 0) || recurringAmount <= 0 {
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
	sub, err := q.GetSubscriptionByID(ctx, gen.GetSubscriptionByIDParams{MerchantID: mid.UUID(), ID: subscriptionID})
	if err != nil || strings.TrimSpace(models.DerefStr(sub.RailSubscriptionID)) == "" {
		return errors.New("accepted schedule requires its provider agreement reference")
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
		Kind: "renewal", SourceKind: "provider_cycle", ProviderReference: models.DerefStr(sub.RailSubscriptionID), Currency: order.Currency, Total: recurringAmount,
		Lines: []Line{{PriceID: line.PriceID, ProductID: line.ProductID, Description: line.Description,
			Quantity: models.DerefIntPtr(line.Quantity), UnitAmount: unit, Amount: recurringAmount,
			BillingIntervalHours: &cadenceHours, AccessDurationHours: models.DerefIntPtr(line.AccessDurationHours)}}}
	raw, err := json.Marshal(bill)
	if err != nil {
		return err
	}
	_, err = q.InsertProviderSchedule(ctx, gen.InsertProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID,
		CustomerID: order.CustomerID, PspID: *order.PspID, InitialOrderID: &orderID, SourceReference: "order:" + orderID.String(), ProviderReference: bill.ProviderReference, FirstDueAt: firstDue.UTC(), CadenceHours: int32(cadenceHours), AcceptedBill: raw, Now: now})
	if err != nil {
		return err
	}
	stored, err := q.GetProviderSchedule(ctx, gen.GetProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID, SourceReference: "order:" + orderID.String()})
	if err != nil {
		return err
	}
	if stored.InitialOrderID == nil || *stored.InitialOrderID != orderID || stored.CustomerID != order.CustomerID || stored.PspID != *order.PspID || !stored.FirstDueAt.Equal(firstDue) || stored.CadenceHours != int32(cadenceHours) || !sameJSON(stored.AcceptedBill, raw) {
		return ErrConflict
	}
	return nil
}

// DeclareSchedule accepts an operator-verified forward import agreement. No
// order, cash movement or historical settlement is invented by this declaration.
func DeclareSchedule(ctx context.Context, d *db.DB, source string, bill Bill, firstDue time.Time, cadenceHours, cadenceMonths int, now time.Time) error {
	firstDue = firstDue.UTC().Truncate(time.Microsecond)
	if bill.SubscriptionID == nil || strings.TrimSpace(bill.ProviderReference) == "" || firstDue.IsZero() || !validCadence(cadenceHours, cadenceMonths) || strings.TrimSpace(source) == "" || len(source) > 512 {
		return errors.New("declared provider schedule is incomplete")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	bill.Kind, bill.SourceKind = "renewal", "provider_cycle"
	bill.SourceKey = ""
	bill.PeriodStart, bill.PeriodEnd, bill.DueAt = time.Time{}, time.Time{}, time.Time{}
	if bill.Payment != nil || bill.Settled || bill.Total <= 0 || len(bill.Lines) != 1 || bill.Lines[0].Amount != bill.Total {
		return errors.New("declared schedule is an accepted agreement, not historical money")
	}
	raw, err := json.Marshal(bill)
	if err != nil {
		return err
	}
	q := d.Gen(ctx)
	_, err = q.InsertProviderSchedule(ctx, gen.InsertProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: *bill.SubscriptionID,
		CustomerID: bill.CustomerID, PspID: bill.PSPID, SourceReference: source, ProviderReference: bill.ProviderReference, FirstDueAt: firstDue.UTC(), CadenceHours: int32(cadenceHours), CadenceMonths: int32(cadenceMonths), AcceptedBill: raw, Now: now})
	if err != nil {
		return err
	}
	stored, err := q.GetProviderSchedule(ctx, gen.GetProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: *bill.SubscriptionID, SourceReference: source})
	if err != nil {
		return err
	}
	if stored.CadenceMonths != int32(cadenceMonths) || stored.SourceReference != source || stored.CustomerID != bill.CustomerID || stored.PspID != bill.PSPID || !stored.FirstDueAt.Equal(firstDue) || stored.CadenceHours != int32(cadenceHours) || !sameJSON(stored.AcceptedBill, raw) {
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
	receipt.OccurredAt = receipt.OccurredAt.UTC().Truncate(time.Microsecond)
	if sub == nil || receipt.TransactionID == "" || receipt.OccurredAt.IsZero() {
		return Bill{}, errors.New("provider cycle receipt is incomplete")
	}
	q := d.Gen(ctx)
	bound, err := q.GetProviderCycleReceipt(ctx, gen.GetProviderCycleReceiptParams{MerchantID: sub.MerchantID, PspID: sub.PspID, TransactionID: receipt.TransactionID})
	var schedule gen.BillingProviderSchedule
	var due time.Time
	switch {
	case err == nil:
		if bound.SubscriptionID != sub.ID || bound.Amount != receipt.Amount && receipt.Amount != 0 || bound.Currency != receipt.Currency && receipt.Currency != "" || bound.Approved != receipt.Approved || !bound.OccurredAt.Equal(receipt.OccurredAt) {
			return Bill{}, ErrConflict
		}
		schedule, err = q.GetProviderSchedule(ctx, gen.GetProviderScheduleParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, SourceReference: bound.ScheduleSourceReference})
		if err != nil {
			return Bill{}, err
		}
		due = bound.DueAt
	case db.IsNotFound(err):
		schedules, err := q.ListProviderSchedules(ctx, gen.ListProviderSchedulesParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, ProviderReference: sub.RailSubscriptionID})
		if err != nil {
			return Bill{}, err
		}
		matches := 0
		selected := receipt
		for i, candidate := range schedules {
			var terms Bill
			if err := json.Unmarshal(candidate.AcceptedBill, &terms); err != nil {
				return Bill{}, err
			}
			attempt := receipt
			if !attempt.Approved && attempt.Amount == 0 {
				attempt.Amount, attempt.Currency = terms.Total, terms.Currency
			}
			if candidate.PspID != sub.PspID || candidate.CustomerID != sub.CustomerID || terms.SubscriptionID == nil || *terms.SubscriptionID != sub.ID || terms.Owner != string(sub.CollectionPolicy) || attempt.Amount != terms.Total || !strings.EqualFold(attempt.Currency, terms.Currency) {
				continue
			}
			if !attempt.Approved {
				prior, err := q.LatestSettledProviderCycle(ctx, gen.LatestSettledProviderCycleParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, ScheduleSourceReference: candidate.SourceReference})
				if err != nil && !db.IsNotFound(err) {
					return Bill{}, err
				}
				attempt.NextBillingAt = candidate.FirstDueAt
				if err == nil {
					index, err := scheduleBoundaryIndex(candidate.FirstDueAt, int(candidate.CadenceHours), int(candidate.CadenceMonths), prior)
					if err != nil {
						return Bill{}, err
					}
					attempt.NextBillingAt = scheduleBoundary(candidate.FirstDueAt, int(candidate.CadenceHours), int(candidate.CadenceMonths), index+1)
				}
			}
			candidateDue, err := scheduleReceiptCycle(candidate.FirstDueAt, int(candidate.CadenceHours), int(candidate.CadenceMonths), attempt)
			if err != nil || i+1 < len(schedules) && !candidateDue.Before(schedules[i+1].FirstDueAt) {
				continue
			}
			matches++
			schedule, due, selected = candidate, candidateDue, attempt
		}
		if matches != 1 {
			return Bill{}, errors.New("provider receipt cannot prove exactly one accepted schedule version")
		}
		if err := q.InsertProviderCycleReceipt(ctx, gen.InsertProviderCycleReceiptParams{MerchantID: sub.MerchantID, PspID: sub.PspID,
			TransactionID: receipt.TransactionID, SubscriptionID: sub.ID, ScheduleSourceReference: schedule.SourceReference, DueAt: due, Amount: selected.Amount, Currency: selected.Currency,
			OccurredAt: receipt.OccurredAt, Approved: receipt.Approved}); err != nil {
			return Bill{}, err
		}
	default:
		return Bill{}, err
	}
	var bill Bill
	if err := json.Unmarshal(schedule.AcceptedBill, &bill); err != nil {
		return Bill{}, err
	}
	if schedule.PspID != sub.PspID || schedule.CustomerID != sub.CustomerID || bill.SubscriptionID == nil || *bill.SubscriptionID != sub.ID || bill.Owner != string(sub.CollectionPolicy) {
		return Bill{}, ErrConflict
	}
	version := sha256.Sum256([]byte(schedule.SourceReference))
	bill.SourceKey = fmt.Sprintf("%s:%x:%s", sub.ID, version[:16], due.UTC().Format(time.RFC3339Nano))
	bill.ProviderReference = schedule.ProviderReference
	index, err := scheduleBoundaryIndex(schedule.FirstDueAt, int(schedule.CadenceHours), int(schedule.CadenceMonths), due)
	if err != nil {
		return Bill{}, err
	}
	bill.PeriodStart, bill.PeriodEnd, bill.DueAt = due, scheduleBoundary(schedule.FirstDueAt, int(schedule.CadenceHours), int(schedule.CadenceMonths), index+1), due
	return bill, nil
}

// NMI and CCBill expose billing days. Match that declared boundary to the
// immutable accepted cadence; do not round an arbitrary changed retry date or
// infer from the subscription's mutable paid-through value.
func validCadence(hours, months int) bool {
	return hours > 0 && int64(hours) <= math.MaxInt64/int64(time.Hour) && hours%24 == 0 && months == 0 || hours == 0 && months > 0 && months <= 1200
}

// Calendar boundaries retain the original day (clamped in shorter months), so
// a January 31 schedule returns to March 31 after February's shorter month.
func scheduleBoundary(first time.Time, hours, months, index int) time.Time {
	first = first.UTC()
	if months == 0 {
		return first.Add(time.Duration(index) * time.Duration(hours) * time.Hour)
	}
	monthStart := time.Date(first.Year(), first.Month()+time.Month(index*months), 1, first.Hour(), first.Minute(), first.Second(), first.Nanosecond(), time.UTC)
	lastDay := monthStart.AddDate(0, 1, -1).Day()
	day := min(first.Day(), lastDay)
	return monthStart.AddDate(0, 0, day-1)
}

func scheduleBoundaryIndex(first time.Time, hours, months int, boundary time.Time) (int, error) {
	if !validCadence(hours, months) || boundary.IsZero() {
		return 0, errors.New("provider receipt lacks a verified accepted schedule boundary")
	}
	anchor, target := first.UTC().Truncate(24*time.Hour), boundary.UTC().Truncate(24*time.Hour)
	index := 0
	if months > 0 {
		delta := (target.Year()-anchor.Year())*12 + int(target.Month()-anchor.Month())
		if delta < 0 || delta%months != 0 {
			return 0, errors.New("provider schedule boundary differs from accepted calendar cadence")
		}
		index = delta / months
	} else {
		period := time.Duration(hours) * time.Hour
		delta := target.Sub(anchor)
		if delta < 0 || delta%period != 0 {
			return 0, errors.New("provider schedule boundary differs from accepted cadence")
		}
		index = int(delta / period)
	}
	if !scheduleBoundary(first, hours, months, index).Truncate(24 * time.Hour).Equal(target) {
		return 0, errors.New("provider schedule boundary differs from accepted billing day")
	}
	return index, nil
}

func scheduleReceiptCycle(firstDue time.Time, cadenceHours, cadenceMonths int, receipt ScheduleReceipt) (time.Time, error) {
	index, err := scheduleBoundaryIndex(firstDue, cadenceHours, cadenceMonths, receipt.NextBillingAt)
	if err != nil {
		return time.Time{}, err
	}
	day := receipt.OccurredAt.UTC().Truncate(24 * time.Hour)
	if receipt.Approved {
		index--
	}
	if index < 0 {
		return time.Time{}, errors.New("provider receipt predates the first accepted renewal")
	}
	due := scheduleBoundary(firstDue, cadenceHours, cadenceMonths, index)
	end := scheduleBoundary(firstDue, cadenceHours, cadenceMonths, index+1)
	if day.Before(due.Truncate(24*time.Hour)) || !day.Before(end.Truncate(24*time.Hour)) {
		return time.Time{}, errors.New("provider receipt and schedule cannot prove one accepted cycle")
	}
	return due, nil
}
