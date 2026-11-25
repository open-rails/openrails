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
	"github.com/open-rails/openrails/internal/modules/invoices/invoiceevent"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/notificationinbox"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Usage billed in arrears is an invoice (origin usage): a period's owed charges
// in one currency, one line per accrual source, due at finalization plus the
// customer's net terms. A funded balance, remittances and collections pay it,
// in part or whole; it is numbered and complete once paid. The money module
// owns the ledger side of each transition; these are the invoice's.

// Changed runs in the transaction of every change to what a usage invoice has
// due or whether it is open, with the invoice as it now stands and d bound to
// that transaction: delinquency evaluates the invoice's payer there. The
// delinquency module installs it.
var Changed func(ctx context.Context, d *db.DB, invoice gen.BillingInvoice, now time.Time) error

// changed brings the invoice's allowance for doubtful accounts to its state,
// then runs Changed.
func changed(ctx context.Context, d *db.DB, invoice gen.BillingInvoice, now time.Time) error {
	if err := books.New(d.Gen(ctx), invoice.MerchantID).SettleAllowance(ctx, invoice.ID, now); err != nil {
		return err
	}
	if Changed == nil {
		return nil
	}
	return Changed(ctx, d, invoice, now)
}

// maxLines is the most lines a usage invoice takes; further sources share
// the last one.
const maxLines = 100

// Line is a period's pending charges from one accrual source.
type Line struct {
	Source string
	Amount int64
}

// Invoice is a new usage invoice.
type Invoice struct {
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

// Create opens the usage invoice of u's period; one the balance already
// paid in full is complete at once.
func Create(ctx context.Context, d *db.DB, merchantID uuid.UUID, u Invoice, now time.Time) (gen.BillingInvoice, error) {
	q := d.Gen(ctx)
	var total int64
	for _, l := range u.Lines {
		total += l.Amount
	}
	if u.CustomerID == uuid.Nil || len(u.Lines) == 0 || total <= 0 || u.Paid < 0 || u.Paid > total || !u.PeriodEnd.After(u.PeriodStart) {
		return gen.BillingInvoice{}, errors.New("a usage invoice needs its customer, period and charges")
	}
	if err := db.EnsureCustomerRowQ(ctx, q, merchantID, u.CustomerID); err != nil {
		return gen.BillingInvoice{}, err
	}
	tax, err := json.Marshal(nonNilMap(u.Tax))
	if err != nil {
		return gen.BillingInvoice{}, err
	}
	contacts := []byte("[]")
	if u.BillingContacts != nil {
		if contacts, err = json.Marshal(u.BillingContacts); err != nil {
			return gen.BillingInvoice{}, err
		}
		if string(contacts) == "null" {
			contacts = []byte("[]")
		}
	}
	var number *int64
	if u.Paid == total {
		n, err := q.NextInvoiceNumber(ctx)
		if err != nil {
			return gen.BillingInvoice{}, err
		}
		number = &n
	}
	id := uuidutil.NewV7()
	if err := q.CreateUsageInvoice(ctx, gen.CreateUsageInvoiceParams{MerchantID: merchantID, ID: id, CustomerID: u.CustomerID, Currency: u.Currency,
		Total: total, AmountPaid: u.Paid, Number: number, PeriodStartsAt: u.PeriodStart.UTC(), PeriodEndsAt: u.PeriodEnd.UTC(), DueAt: u.DueAt.UTC(),
		CollectionMethod: u.CollectionMethod, PoNumber: u.PONumber, Memo: u.Memo, Tax: tax, BillingContacts: contacts, Now: now}); err != nil {
		return gen.BillingInvoice{}, err
	}
	for i, l := range foldLines(u.Lines) {
		if err := q.CreateUsageInvoiceLine(ctx, gen.CreateUsageInvoiceLineParams{MerchantID: merchantID, ID: uuidutil.NewV7(), InvoiceID: id,
			CustomerID: u.CustomerID, Position: int32(i), Source: l.Source, Amount: l.Amount, Now: now}); err != nil {
			return gen.BillingInvoice{}, err
		}
	}
	invoice, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: merchantID, ID: id})
	if err != nil {
		return invoice, err
	}
	if number == nil {
		return invoice, changed(ctx, d, invoice, now)
	}
	return invoice, invoiceevent.Enqueue(ctx, q, &invoice, "invoice.completed", now, map[string]any{"number": *number})
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

// Pay applies amount to a usage invoice, completing it when that pays it
// in full: numbered, with payment (nil for a balance repayment) and
// invoice.completed. attempt is the live collection that paid it, nil for any
// other payment, which a live collection's claim refuses.
func Pay(ctx context.Context, d *db.DB, merchantID, invoiceID uuid.UUID, amount int64, payment, attempt *uuid.UUID, now time.Time) (gen.BillingInvoice, error) {
	q := d.Gen(ctx)
	invoice, err := q.PayUsageInvoice(ctx, gen.PayUsageInvoiceParams{MerchantID: merchantID, ID: invoiceID, Amount: amount, AttemptID: attempt, Now: now})
	if err != nil {
		return invoice, err
	}
	if invoice.AmountPaid == nil || *invoice.AmountPaid != invoice.Total {
		return invoice, changed(ctx, d, invoice, now)
	}
	n, err := q.NextInvoiceNumber(ctx)
	if err != nil {
		return invoice, err
	}
	number := n
	if c, err := q.CompleteUsageInvoice(ctx, gen.CompleteUsageInvoiceParams{MerchantID: merchantID, ID: invoiceID, Number: number, PaymentID: payment, Now: now}); err != nil || c != 1 {
		return invoice, errors.Join(err, fmt.Errorf("usage invoice %s did not complete", invoiceID))
	}
	invoice.Status, invoice.PaymentStatus, invoice.Number = string(billing.InvoiceComplete), string(billing.InvoicePaymentSucceeded), &number
	extra := map[string]any{"number": number}
	if payment != nil {
		extra["payment_id"] = payment.String()
	}
	if err := invoiceevent.Enqueue(ctx, q, &invoice, "invoice.completed", now, extra); err != nil {
		return invoice, err
	}
	if err := notificationinbox.WithdrawInvoice(ctx, d, invoice.CustomerID, invoice.ID); err != nil {
		return invoice, err
	}
	return invoice, changed(ctx, d, invoice, now)
}

// StartCollection puts one collection operation on an open (or
// uncollectible) usage invoice: its attempt, live until the operation ends.
func StartCollection(ctx context.Context, d *db.DB, invoice gen.BillingInvoice, operation, psp uuid.UUID, rail string, method uuid.UUID, amount int64, now time.Time) error {
	q := d.Gen(ctx)
	state, err := json.Marshal(map[string]any{"operation_id": operation.String()})
	if err != nil {
		return err
	}
	if err := q.CreateInvoiceAttempt(ctx, gen.CreateInvoiceAttemptParams{ID: operation, MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, InvoiceID: invoice.ID,
		Rail: rail, Status: "processing", Amount: amount, Currency: invoice.Currency, ExpiresAt: now.AddDate(1, 0, 0), RailState: state, PspID: psp, Now: now}); err != nil {
		return err
	}
	n, err := q.StartUsageCollection(ctx, gen.StartUsageCollectionParams{MerchantID: invoice.MerchantID, ID: invoice.ID, AttemptID: operation, PaymentMethodID: method, PspID: psp, Now: now})
	if err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("usage invoice %s took no collection", invoice.ID))
	}
	if invoice.Status != string(billing.InvoiceUncollectible) {
		return nil
	}
	// Collecting an uncollectible invoice reopens it.
	return changed(ctx, d, invoice, now)
}

// EndCollection ends a collection that moved no money (not executed) or
// whose payment Pay already applied: the attempt is closed, and an
// invoice still owed takes payment again, next scheduled at next (nil: none).
func EndCollection(ctx context.Context, d *db.DB, merchantID, invoiceID, attempt uuid.UUID, succeeded bool, next *time.Time, now time.Time) error {
	q := d.Gen(ctx)
	status := "failed"
	if succeeded {
		status = "succeeded"
	}
	if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: merchantID, ID: attempt, Status: status, Now: now}); err != nil {
		return err
	}
	n, err := q.EndUsageCollection(ctx, gen.EndUsageCollectionParams{MerchantID: merchantID, ID: invoiceID, AttemptID: attempt, NextAttemptAt: next, Now: now})
	if err != nil || n == 0 {
		return err
	}
	invoice, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: merchantID, ID: invoiceID})
	if err != nil {
		return err
	}
	return changed(ctx, d, invoice, now)
}

// DeclineCollection records a declined collection: the invoice is open
// again with why, its next try at next (nil: charging stops until the
// customer acts), or uncollectible when terminal.
func DeclineCollection(ctx context.Context, d *db.DB, merchantID, invoiceID, attempt uuid.UUID, failure billing.PaymentFailure, terminal bool, next *time.Time, now time.Time) error {
	q := d.Gen(ctx)
	if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: merchantID, ID: attempt, Status: "failed", Now: now}); err != nil {
		return err
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	n, err := q.RecordUsageCollectionFailure(ctx, gen.RecordUsageCollectionFailureParams{MerchantID: merchantID, ID: invoiceID, AttemptID: attempt,
		Terminal: terminal, NextAttemptAt: next, LastPaymentError: raw, Now: now})
	if err != nil || n == 0 {
		return err
	}
	invoice, err := q.LockInvoice(ctx, gen.LockInvoiceParams{MerchantID: merchantID, ID: invoiceID})
	if err != nil {
		return err
	}
	if err := invoiceevent.Enqueue(ctx, q, &invoice, "invoice.payment_failed", now, map[string]any{"reason": failure.Reason}); err != nil {
		return err
	}
	return changed(ctx, d, invoice, now)
}

// StopCollection stops collecting an open usage invoice until the
// customer acts, with why.
func StopCollection(ctx context.Context, q *gen.Queries, merchantID, invoiceID uuid.UUID, failure billing.PaymentFailure, now time.Time) error {
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	n, err := q.StopUsageCollection(ctx, gen.StopUsageCollectionParams{MerchantID: merchantID, ID: invoiceID, LastPaymentError: raw, Now: now})
	if err != nil || n != 1 {
		return errors.Join(err, fmt.Errorf("usage invoice %s collection did not stop", invoiceID))
	}
	return nil
}

// VoidInvoice voids an unpaid usage invoice, with invoice.voided; false when it
// was not open or uncollectible.
func VoidInvoice(ctx context.Context, d *db.DB, invoice gen.BillingInvoice, now time.Time) (bool, error) {
	q := d.Gen(ctx)
	n, err := q.VoidUsageInvoice(ctx, gen.VoidUsageInvoiceParams{MerchantID: invoice.MerchantID, ID: invoice.ID, Now: now})
	if err != nil || n == 0 {
		return false, err
	}
	invoice.Status, invoice.PaymentStatus = string(billing.InvoiceVoid), string(billing.InvoicePaymentRequiresPaymentMethod)
	if err := invoiceevent.Enqueue(ctx, q, &invoice, "invoice.voided", now, nil); err != nil {
		return false, err
	}
	if err := notificationinbox.WithdrawInvoice(ctx, d, invoice.CustomerID, invoice.ID); err != nil {
		return false, err
	}
	return true, changed(ctx, d, invoice, now)
}

// MarkUncollectible writes an open usage invoice off; collecting it
// again reopens it.
func MarkUncollectible(ctx context.Context, d *db.DB, invoice gen.BillingInvoice, now time.Time) (bool, error) {
	n, err := d.Gen(ctx).MarkUsageInvoiceUncollectible(ctx, gen.MarkUsageInvoiceUncollectibleParams{MerchantID: invoice.MerchantID, ID: invoice.ID, Now: now})
	if err != nil || n != 1 {
		return false, err
	}
	invoice.Status = string(billing.InvoiceUncollectible)
	return true, changed(ctx, d, invoice, now)
}

// Due is what is still owed on a usage invoice.
func Due(o gen.BillingInvoice) int64 {
	if o.AmountPaid == nil {
		return 0
	}
	return o.Total - *o.AmountPaid
}

// Paid is what of a usage invoice's total is paid.
func Paid(o gen.BillingInvoice) int64 {
	if o.AmountPaid == nil {
		return 0
	}
	return *o.AmountPaid
}
