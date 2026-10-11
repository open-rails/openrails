package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/orders/orderevent"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Usage billed in arrears is an order (origin usage): a period's owed charges
// in one currency, one line per accrual source, due at finalization plus the
// customer's net terms. A funded balance, remittances and collections pay it,
// in part or whole; it is numbered and complete once paid. The money module
// owns the ledger side of each transition; these are the order's.

// maxLines is the most lines a usage order takes; further sources share
// the last one.
const maxLines = 100

// Line is a period's pending charges from one accrual source.
type Line struct {
	Source string
	Amount int64
}

// Order is a new usage order.
type Order struct {
	CustomerID  uuid.UUID
	Currency    string
	PeriodStart time.Time
	PeriodEnd   time.Time
	DueAt       time.Time
	// Paid is what a funded balance already repaid of the lines' total.
	Paid             int64
	Lines            []Line
	CollectionMethod string
	PONumber         *string
	Memo             *string
	Tax              map[string]any
	BillingContacts  any
}

// Create opens the usage order of u's period; one the balance already
// paid in full is complete at once.
func Create(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, u Order, now time.Time) (gen.BillingOrder, error) {
	var total int64
	for _, l := range u.Lines {
		total += l.Amount
	}
	if u.CustomerID == uuid.Nil || len(u.Lines) == 0 || total <= 0 || u.Paid < 0 || u.Paid > total || !u.PeriodEnd.After(u.PeriodStart) {
		return gen.BillingOrder{}, errors.New("a usage order needs its customer, period and charges")
	}
	if err := db.EnsureCustomerRowQ(ctx, q, merchantID, u.CustomerID); err != nil {
		return gen.BillingOrder{}, err
	}
	tax, err := json.Marshal(nonNilMap(u.Tax))
	if err != nil {
		return gen.BillingOrder{}, err
	}
	contacts := []byte("[]")
	if u.BillingContacts != nil {
		if contacts, err = json.Marshal(u.BillingContacts); err != nil {
			return gen.BillingOrder{}, err
		}
		if string(contacts) == "null" {
			contacts = []byte("[]")
		}
	}
	var number *string
	if u.Paid == total {
		n, err := q.NextDocumentNumber(ctx, gen.NextDocumentNumberParams{MerchantID: merchantID, Now: now})
		if err != nil {
			return gen.BillingOrder{}, err
		}
		number = new(orderevent.Number(n))
	}
	id := uuidutil.NewV7()
	if err := q.CreateUsageOrder(ctx, gen.CreateUsageOrderParams{MerchantID: merchantID, ID: id, CustomerID: u.CustomerID, Currency: u.Currency,
		Total: total, AmountPaid: u.Paid, Number: number, PeriodStartsAt: u.PeriodStart.UTC(), PeriodEndsAt: u.PeriodEnd.UTC(), DueAt: u.DueAt.UTC(),
		CollectionMethod: u.CollectionMethod, PoNumber: u.PONumber, Memo: u.Memo, Tax: tax, BillingContacts: contacts, Now: now}); err != nil {
		return gen.BillingOrder{}, err
	}
	for i, l := range foldLines(u.Lines) {
		if err := q.CreateUsageOrderLine(ctx, gen.CreateUsageOrderLineParams{MerchantID: merchantID, ID: uuidutil.NewV7(), OrderID: id,
			CustomerID: u.CustomerID, Position: int32(i), Source: l.Source, Amount: l.Amount, Now: now}); err != nil {
			return gen.BillingOrder{}, err
		}
	}
	order, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: merchantID, ID: id})
	if err != nil || number == nil {
		return order, err
	}
	return order, orderevent.Enqueue(ctx, q, &order, "order.completed", now, map[string]any{"number": *number})
}

// foldLines keeps the first maxLines-1 sources and folds the rest
// into one line.
func foldLines(lines []Line) []Line {
	if len(lines) <= maxLines {
		return lines
	}
	out := append([]Line{}, lines[:maxLines-1]...)
	rest := Line{Source: "other"}
	for _, l := range lines[maxLines-1:] {
		rest.Amount += l.Amount
	}
	return append(out, rest)
}

func nonNilMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// Pay applies amount to a usage order, completing it when that pays it
// in full: numbered, with payment (nil for a balance repayment) and
// order.completed. attempt is the live collection that paid it, nil for any
// other payment, which a live collection's claim refuses.
func Pay(ctx context.Context, q *gen.Queries, merchantID, orderID uuid.UUID, amount int64, payment, attempt *uuid.UUID, now time.Time) (gen.BillingOrder, error) {
	order, err := q.PayUsageOrder(ctx, gen.PayUsageOrderParams{MerchantID: merchantID, ID: orderID, Amount: amount, AttemptID: attempt, Now: now})
	if err != nil {
		return order, err
	}
	if order.AmountPaid == nil || *order.AmountPaid != order.Total {
		return order, nil
	}
	n, err := q.NextDocumentNumber(ctx, gen.NextDocumentNumberParams{MerchantID: merchantID, Now: now})
	if err != nil {
		return order, err
	}
	number := orderevent.Number(n)
	if c, err := q.CompleteUsageOrder(ctx, gen.CompleteUsageOrderParams{MerchantID: merchantID, ID: orderID, Number: number, PaymentID: payment, Now: now}); err != nil || c != 1 {
		return order, errors.Join(err, fmt.Errorf("usage order %s did not complete", orderID))
	}
	order.Status, order.PaymentStatus, order.Number = string(billing.OrderComplete), string(billing.OrderPaymentSucceeded), &number
	extra := map[string]any{"number": number}
	if payment != nil {
		extra["payment_id"] = payment.String()
	}
	return order, orderevent.Enqueue(ctx, q, &order, "order.completed", now, extra)
}

// StartCollection puts one collection operation on an open (or
// uncollectible) usage order: its attempt, live until the operation ends.
func StartCollection(ctx context.Context, q *gen.Queries, order gen.BillingOrder, operation, psp uuid.UUID, rail string, method uuid.UUID, amount int64, now time.Time) error {
	state, err := json.Marshal(map[string]any{"operation_id": operation.String()})
	if err != nil {
		return err
	}
	if err := q.CreateOrderAttempt(ctx, gen.CreateOrderAttemptParams{ID: operation, MerchantID: order.MerchantID, CustomerID: order.CustomerID, OrderID: order.ID,
		Rail: rail, Status: "processing", Amount: amount, Currency: order.Currency, ExpiresAt: now.AddDate(1, 0, 0), RailState: state, PspID: psp, Now: now}); err != nil {
		return err
	}
	n, err := q.StartUsageCollection(ctx, gen.StartUsageCollectionParams{MerchantID: order.MerchantID, ID: order.ID, AttemptID: operation, PaymentMethodID: method, PspID: psp, Now: now})
	if err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("usage order %s took no collection", order.ID))
	}
	return nil
}

// EndCollection ends a collection that moved no money (not executed) or
// whose payment Pay already applied: the attempt is closed, and an
// order still owed takes payment again, next scheduled at next (nil: none).
func EndCollection(ctx context.Context, q *gen.Queries, merchantID, orderID, attempt uuid.UUID, succeeded bool, next *time.Time, now time.Time) error {
	status := "failed"
	if succeeded {
		status = "succeeded"
	}
	if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: merchantID, ID: attempt, Status: status, Now: now}); err != nil {
		return err
	}
	_, err := q.EndUsageCollection(ctx, gen.EndUsageCollectionParams{MerchantID: merchantID, ID: orderID, AttemptID: attempt, NextAttemptAt: next, Now: now})
	return err
}

// DeclineCollection records a declined collection: the order is open
// again with why, its next try at next (nil: charging stops until the
// customer acts), or uncollectible when terminal.
func DeclineCollection(ctx context.Context, q *gen.Queries, merchantID, orderID, attempt uuid.UUID, failure billing.PaymentFailure, terminal bool, next *time.Time, now time.Time) error {
	if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: merchantID, ID: attempt, Status: "failed", Now: now}); err != nil {
		return err
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	n, err := q.RecordUsageCollectionFailure(ctx, gen.RecordUsageCollectionFailureParams{MerchantID: merchantID, ID: orderID, AttemptID: attempt,
		Terminal: terminal, NextAttemptAt: next, LastPaymentError: raw, Now: now})
	if err != nil || n == 0 {
		return err
	}
	order, err := q.LockOrder(ctx, gen.LockOrderParams{MerchantID: merchantID, ID: orderID})
	if err != nil {
		return err
	}
	return orderevent.Enqueue(ctx, q, &order, "order.payment_failed", now, map[string]any{"reason": failure.Reason})
}

// StopCollection stops collecting an open usage order until the
// customer acts, with why.
func StopCollection(ctx context.Context, q *gen.Queries, merchantID, orderID uuid.UUID, failure billing.PaymentFailure, now time.Time) error {
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	n, err := q.StopUsageCollection(ctx, gen.StopUsageCollectionParams{MerchantID: merchantID, ID: orderID, LastPaymentError: raw, Now: now})
	if err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("usage order %s collection did not stop", orderID))
	}
	return nil
}

// VoidOrder voids an unpaid usage order, with order.voided; false when it
// was not open or uncollectible.
func VoidOrder(ctx context.Context, q *gen.Queries, order gen.BillingOrder, now time.Time) (bool, error) {
	n, err := q.VoidUsageOrder(ctx, gen.VoidUsageOrderParams{MerchantID: order.MerchantID, ID: order.ID, Now: now})
	if err != nil || n == 0 {
		return false, err
	}
	order.Status, order.PaymentStatus = string(billing.OrderVoid), string(billing.OrderPaymentRequiresPaymentMethod)
	return true, orderevent.Enqueue(ctx, q, &order, "order.voided", now, nil)
}

// MarkUncollectible writes an open usage order off; collecting it
// again reopens it.
func MarkUncollectible(ctx context.Context, q *gen.Queries, order gen.BillingOrder, now time.Time) (bool, error) {
	n, err := q.MarkUsageOrderUncollectible(ctx, gen.MarkUsageOrderUncollectibleParams{MerchantID: order.MerchantID, ID: order.ID, Now: now})
	return n == 1, err
}

// Due is what is still owed on a usage order.
func Due(o gen.BillingOrder) int64 {
	if o.AmountPaid == nil {
		return 0
	}
	return o.Total - *o.AmountPaid
}

// Paid is what of a usage order's total is paid.
func Paid(o gen.BillingOrder) int64 {
	if o.AmountPaid == nil {
		return 0
	}
	return *o.AmountPaid
}
