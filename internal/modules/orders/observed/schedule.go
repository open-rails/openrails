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
	"github.com/open-rails/openrails/internal/shared/calendarutil"
)

// SeedSchedule freezes the future recurring agreement when an accepted initial
// order is fulfilled. The caller supplies its accepted provider cadence and
// recurring amount (which can differ from a trial's initial charge). It must
// never be called to reconstruct a schedule from today's subscription period.
func SeedSchedule(ctx context.Context, d *db.DB, orderID, subscriptionID uuid.UUID, firstDue time.Time, cadenceHours int, recurringAmount int64, now time.Time) error {
	return SeedScheduleCadence(ctx, d, orderID, subscriptionID, firstDue, nil, cadenceHours, 0, recurringAmount, now)
}

// SeedScheduleCadence freezes an accepted provider amendment without turning
// its native calendar months into estimated hours.
func SeedScheduleCadence(ctx context.Context, d *db.DB, orderID, subscriptionID uuid.UUID, firstDue time.Time, calendarAnchor *time.Time, cadenceHours, cadenceMonths int, recurringAmount int64, now time.Time) error {
	firstDue = firstDue.UTC().Truncate(time.Microsecond)
	if firstDue.IsZero() || !validCadence(cadenceHours, cadenceMonths) || recurringAmount <= 0 {
		return errors.New("accepted provider schedule terms are incomplete")
	}
	if calendarAnchor != nil {
		if cadenceMonths == 0 || calendarAnchor.IsZero() {
			return errors.New("accepted calendar schedule anchor contradicts its cadence")
		}
		if _, err := scheduleBoundaryIndex(*calendarAnchor, cadenceHours, cadenceMonths, firstDue); err != nil {
			return err
		}
	}
	cadenceHours32, err := int32Value(cadenceHours)
	if err != nil {
		return errors.New("accepted provider schedule cadence is out of range")
	}
	cadenceMonths32, err := int32Value(cadenceMonths)
	if err != nil {
		return errors.New("accepted provider schedule cadence is out of range")
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
	sub, err := q.GetSubscriptionByIDForUpdate(ctx, gen.GetSubscriptionByIDForUpdateParams{MerchantID: mid.UUID(), ID: subscriptionID})
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
	var interval *int
	if cadenceHours > 0 {
		interval = &cadenceHours
	}
	bill := Bill{CalendarAnchor: calendarAnchor, PSPID: *order.PspID, CustomerID: order.CustomerID, SubscriptionID: &subscriptionID, Owner: order.CollectionOwner,
		Kind: "renewal", SourceKind: "provider_cycle", ProviderReference: models.DerefStr(sub.RailSubscriptionID), Currency: order.Currency, Total: recurringAmount,
		Lines: []Line{{PriceID: line.PriceID, ProductID: line.ProductID, Description: line.Description,
			Quantity: models.DerefIntPtr(line.Quantity), UnitAmount: unit, Amount: recurringAmount,
			Ownership: models.DerefStr(line.Ownership), ClaimKey: models.DerefStr(line.ClaimKey),
			BillingIntervalHours: interval, AccessDurationHours: models.DerefIntPtr(line.AccessDurationHours), ProviderAccessDefault: line.ProviderAccessDefault}}}
	raw, err := json.Marshal(bill)
	if err != nil {
		return err
	}
	_, err = q.InsertProviderSchedule(ctx, gen.InsertProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID,
		CustomerID: order.CustomerID, PspID: *order.PspID, InitialOrderID: &orderID, SourceReference: "order:" + orderID.String(), ProviderReference: bill.ProviderReference, FirstDueAt: firstDue.UTC(), CadenceHours: cadenceHours32, CadenceMonths: cadenceMonths32, AcceptedBill: raw, Now: now})
	if err != nil {
		return err
	}
	stored, err := q.GetProviderSchedule(ctx, gen.GetProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: subscriptionID, SourceReference: "order:" + orderID.String()})
	if err != nil {
		return err
	}
	if stored.InitialOrderID == nil || *stored.InitialOrderID != orderID || stored.CustomerID != order.CustomerID || stored.PspID != *order.PspID || !stored.FirstDueAt.Equal(firstDue) || stored.CadenceHours != cadenceHours32 || stored.CadenceMonths != cadenceMonths32 || !sameJSON(stored.AcceptedBill, raw) {
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
	cadenceHours32, err := int32Value(cadenceHours)
	if err != nil {
		return errors.New("declared provider schedule cadence is out of range")
	}
	cadenceMonths32, err := int32Value(cadenceMonths)
	if err != nil {
		return errors.New("declared provider schedule cadence is out of range")
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
	q := d.Gen(ctx)
	// Serialize accepted amendments. A second confirmed change for the same
	// future cycle supersedes the first by durable acceptance order; source
	// replay keeps its original version and cannot become current again.
	locked, err := q.GetSubscriptionByIDForUpdate(ctx, gen.GetSubscriptionByIDForUpdateParams{MerchantID: mid.UUID(), ID: *bill.SubscriptionID})
	if err != nil {
		return err
	}

	prior, err := q.GetProviderSchedule(ctx, gen.GetProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: *bill.SubscriptionID, SourceReference: source})
	if err != nil && !db.IsNotFound(err) {
		return err
	}
	bill.Lines = append([]Line(nil), bill.Lines...)
	line := &bill.Lines[0]
	if err == nil {
		var accepted Bill
		if err := json.Unmarshal(prior.AcceptedBill, &accepted); err != nil {
			return err
		}
		// A replay keeps the accepted spelling of the same provider
		// subscription, which the provider may have respelled since.
		if accepted.ProviderReference != bill.ProviderReference {
			same, err := q.SameProviderSubscription(ctx, gen.SameProviderSubscriptionParams{Rail: locked.Rail, A: accepted.ProviderReference, B: bill.ProviderReference})
			if err != nil {
				return err
			}
			if same {
				bill.ProviderReference = accepted.ProviderReference
			}
		}
		// A replay keeps the accepted line's frozen name and ownership.
		if len(accepted.Lines) == 1 && sameID(line.PriceID, accepted.Lines[0].PriceID) && sameID(line.ProductID, accepted.Lines[0].ProductID) {
			line.Description = accepted.Lines[0].Description
			if line.Ownership == "" {
				line.Ownership, line.ClaimKey = accepted.Lines[0].Ownership, accepted.Lines[0].ClaimKey
			}
		}
	}
	if err := freezeLineOwnership(ctx, q, mid.UUID(), line, true); err != nil {
		return err
	}
	raw, err := json.Marshal(bill)
	if err != nil {
		return err
	}
	_, err = q.InsertProviderSchedule(ctx, gen.InsertProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: *bill.SubscriptionID,
		CustomerID: bill.CustomerID, PspID: bill.PSPID, SourceReference: source, ProviderReference: bill.ProviderReference, FirstDueAt: firstDue.UTC(), CadenceHours: cadenceHours32, CadenceMonths: cadenceMonths32, AcceptedBill: raw, Now: now})
	if err != nil {
		return err
	}
	stored, err := q.GetProviderSchedule(ctx, gen.GetProviderScheduleParams{MerchantID: mid.UUID(), SubscriptionID: *bill.SubscriptionID, SourceReference: source})
	if err != nil {
		return err
	}
	if stored.CadenceMonths != cadenceMonths32 || stored.SourceReference != source || stored.CustomerID != bill.CustomerID || stored.PspID != bill.PSPID || !stored.FirstDueAt.Equal(firstDue) || stored.CadenceHours != cadenceHours32 || !sameJSON(stored.AcceptedBill, raw) {
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
	// DueAt is the billing day the provider says a declined attempt was for.
	DueAt    time.Time
	Approved bool
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
			if _, _, err := scheduleTimebase(candidate); err != nil {
				return Bill{}, err
			}
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
			unpaid, err := firstUnsettledCycle(ctx, q, sub, candidate)
			if err != nil {
				return Bill{}, err
			}
			var candidateDue time.Time
			if attempt.Approved {
				candidateDue, err = approvedReceiptCycle(candidate, unpaid, attempt)
			} else {
				candidateDue, err = declinedReceiptCycle(candidate, unpaid, attempt)
			}
			if err != nil {
				if !errors.Is(err, errNoCycle) {
					return Bill{}, err
				}
				continue
			}
			if i+1 < len(schedules) && (!candidateDue.Before(schedules[i+1].FirstDueAt) || !receipt.OccurredAt.Before(schedules[i+1].FirstDueAt.UTC().Truncate(24*time.Hour))) {
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
	bill.SourceKey = cycleSourceKey(sub.ID, schedule.SourceReference, due)
	bill.ProviderReference = schedule.ProviderReference
	anchor, offset, err := scheduleTimebase(schedule)
	if err != nil {
		return Bill{}, err
	}
	index, err := scheduleBoundaryIndex(anchor, int(schedule.CadenceHours), int(schedule.CadenceMonths), due)
	if err != nil {
		return Bill{}, err
	}
	if index < offset {
		return Bill{}, ErrConflict
	}
	bill.PeriodStart, bill.PeriodEnd, bill.DueAt = due, scheduleBoundary(anchor, int(schedule.CadenceHours), int(schedule.CadenceMonths), index+1), due
	return bill, nil
}

var errNoCycle = errors.New("provider receipt matches no accepted cycle")

// firstUnsettledCycle is the index of the earliest cycle after the latest one
// an approved receipt settled, skipping cycles whose order another verified
// payment (such as OpenRails' own decline recovery) completed.
func firstUnsettledCycle(ctx context.Context, q *gen.Queries, sub *models.Subscription, schedule gen.BillingProviderSchedule) (int, error) {
	hours, months := int(schedule.CadenceHours), int(schedule.CadenceMonths)
	anchor, offset, err := scheduleTimebase(schedule)
	if err != nil {
		return 0, err
	}
	index := 0
	prior, err := q.LatestSettledProviderCycle(ctx, gen.LatestSettledProviderCycleParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, ScheduleSourceReference: schedule.SourceReference})
	switch {
	case err == nil:
		last, err := scheduleBoundaryIndex(anchor, hours, months, prior)
		if err != nil {
			return 0, err
		}
		if last < offset {
			return 0, ErrConflict
		}
		index = last - offset + 1
	case !db.IsNotFound(err):
		return 0, err
	}
	for ; index < maxCycleSkip+1; index++ {
		complete, err := cycleComplete(ctx, q, sub, scheduleBoundary(anchor, hours, months, offset+index))
		if err != nil || !complete {
			return index, err
		}
	}
	return index, nil
}

// maxCycleSkip bounds how many recovered cycles one receipt may step over.
const maxCycleSkip = 1 << 10

func cycleComplete(ctx context.Context, q *gen.Queries, sub *models.Subscription, due time.Time) (bool, error) {
	day := due.UTC().Truncate(24 * time.Hour)
	return q.SubscriptionRenewalSettledOn(ctx, gen.SubscriptionRenewalSettledOnParams{MerchantID: sub.MerchantID, SubscriptionID: sub.ID, DayStart: day, DayEnd: day.Add(24 * time.Hour)})
}

func cycleSourceKey(subscriptionID uuid.UUID, scheduleSource string, due time.Time) string {
	version := sha256.Sum256([]byte(scheduleSource))
	return fmt.Sprintf("%s:%x:%s", subscriptionID, version[:16], due.UTC().Format(time.RFC3339Nano))
}

// approvedReceiptCycle applies an approved schedule charge to the earliest
// unsettled cycle the provider has executed by its next boundary, the way
// unattributed cash pays the oldest open bill. Receipts of one batch replay in
// time order, so catch-up charges each settle their own cycle. When every
// executed cycle is settled the charge is an extra capture of the newest one.
func approvedReceiptCycle(schedule gen.BillingProviderSchedule, unpaid int, receipt ScheduleReceipt) (time.Time, error) {
	first, offset, err := scheduleTimebase(schedule)
	if err != nil {
		return time.Time{}, err
	}
	hours, months := int(schedule.CadenceHours), int(schedule.CadenceMonths)
	day := receipt.OccurredAt.UTC().Truncate(24 * time.Hour)
	next, err := scheduleBoundaryIndex(first, hours, months, receipt.NextBillingAt)
	if err != nil {
		// No boundary on the accepted cadence (the schedule ended, or the
		// announced date is not one of its billing days): the charge settles
		// the unpaid cycle only inside that cycle's own window.
		due, end := scheduleBoundary(first, hours, months, offset+unpaid), scheduleBoundary(first, hours, months, offset+unpaid+1)
		if day.Before(due.Truncate(24*time.Hour)) || !day.Before(end.Truncate(24*time.Hour)) {
			return time.Time{}, errNoCycle
		}
		return due, nil
	}
	if next < offset+1 {
		return time.Time{}, errNoCycle
	}
	if !day.Before(receipt.NextBillingAt.UTC().Truncate(24 * time.Hour)) {
		return time.Time{}, errNoCycle
	}
	if offset+unpaid < next {
		if due := scheduleBoundary(first, hours, months, offset+unpaid); !day.Before(due.Truncate(24 * time.Hour)) {
			return due, nil
		}
	}
	if newest := scheduleBoundary(first, hours, months, next-1); !day.Before(newest.Truncate(24 * time.Hour)) {
		return newest, nil
	}
	return time.Time{}, errNoCycle
}

// declinedReceiptCycle is the unpaid cycle a decline attempted: the billing
// day the provider names, when it is an unpaid day of the accepted cadence,
// else the unpaid cycle whose window holds the attempt.
func declinedReceiptCycle(schedule gen.BillingProviderSchedule, unpaid int, receipt ScheduleReceipt) (time.Time, error) {
	first, offset, err := scheduleTimebase(schedule)
	if err != nil {
		return time.Time{}, err
	}
	hours, months := int(schedule.CadenceHours), int(schedule.CadenceMonths)
	if !receipt.DueAt.IsZero() {
		index, err := scheduleBoundaryIndex(first, hours, months, receipt.DueAt)
		if err != nil || index < offset+unpaid {
			return time.Time{}, errNoCycle
		}
		return scheduleBoundary(first, hours, months, index), nil
	}
	receipt.NextBillingAt = scheduleBoundary(first, hours, months, offset+unpaid)
	due, err := scheduleReceiptCycle(first, hours, months, receipt)
	if err != nil {
		return time.Time{}, errNoCycle
	}
	return due, nil
}

// scheduleTimebase separates the original calendar anchor from the first cycle
// governed by this immutable version. Existing snapshots use FirstDueAt as
// their anchor, preserving their accepted interpretation exactly.
func scheduleTimebase(schedule gen.BillingProviderSchedule) (time.Time, int, error) {
	if !validCadence(int(schedule.CadenceHours), int(schedule.CadenceMonths)) || schedule.FirstDueAt.IsZero() {
		return time.Time{}, 0, ErrConflict
	}
	var bill Bill
	if err := json.Unmarshal(schedule.AcceptedBill, &bill); err != nil {
		return time.Time{}, 0, err
	}
	if bill.CalendarAnchor == nil {
		return schedule.FirstDueAt, 0, nil
	}
	if schedule.CadenceMonths == 0 || bill.CalendarAnchor.IsZero() {
		return time.Time{}, 0, ErrConflict
	}
	index, err := scheduleBoundaryIndex(*bill.CalendarAnchor, int(schedule.CadenceHours), int(schedule.CadenceMonths), schedule.FirstDueAt)
	return bill.CalendarAnchor.UTC(), index, err
}

// CalendarScheduleAnchor proves a future amendment boundary on the original
// immutable calendar agreement; a clamped February date never replaces its day.
func CalendarScheduleAnchor(schedule gen.BillingProviderSchedule, boundary time.Time) (*time.Time, error) {
	anchor, offset, err := scheduleTimebase(schedule)
	if err != nil || schedule.CadenceMonths == 0 {
		return nil, ErrConflict
	}
	index, err := scheduleBoundaryIndex(anchor, int(schedule.CadenceHours), int(schedule.CadenceMonths), boundary)
	if err != nil || index < offset {
		return nil, ErrConflict
	}
	return &anchor, nil
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
	return calendarutil.AddMonths(first, index*months)
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
