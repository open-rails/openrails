package books

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// Receipt is money a payment brought: taken at a rail on a PSP, or received
// outside OpenRails.
type Receipt struct {
	Payment, Customer uuid.UUID
	Order             *uuid.UUID
	// PSP took the money; nil for money received outside OpenRails.
	PSP      *uuid.UUID
	Amount   int64
	Currency string
	At       time.Time
}

// ReceiptOf is the money a payment row brought; false for one that brought
// none: pending, failed, a bookkeeping row, or a reversal, whose postings are
// its own.
func ReceiptOf(p gen.BillingPayment) (Receipt, bool) {
	if p.Amount <= 0 || p.RefundedPaymentID != nil || p.ReversalKind != nil || p.DeletedAt != nil || p.Status != "succeeded" && p.Status != "refunded" {
		return Receipt{}, false
	}
	r := Receipt{Payment: p.ID, Customer: p.CustomerID, Order: p.OrderID, Amount: p.Amount, Currency: p.Currency, At: p.PurchasedAt}
	switch {
	case p.Channel == "manual":
	case p.MoneyMovement == "rail" && p.PspID != nil:
		r.PSP = p.PspID
	default:
		return Receipt{}, false
	}
	return r, true
}

// Receive books a receipt once, when the money arrived: DR cash_clearing of
// its PSP, or external_receipts / CR unapplied_cash. The money stays
// unapplied until a completed bill applies it to its lines: a payment on a
// closed or already paid order never is.
func (b *Books) Receive(ctx context.Context, r Receipt) error {
	// The general ledger holds registry currencies only; no payment is
	// taken in anything else.
	if CheckCurrency(r.Currency) != nil {
		return nil
	}
	debit := Ref{Type: ExternalReceipts}
	if r.PSP != nil {
		debit = Ref{Type: CashClearing, PSP: r.PSP}
	}
	_, _, err := b.Post(ctx, Posting{
		Debit: debit, Credit: Ref{Type: UnappliedCash}, Amount: r.Amount, Currency: r.Currency, Type: CashReceived,
		Coord:       Coord{Operation: OpCash, Source: "payment", SourceID: r.Payment.String()},
		EffectiveAt: r.At, Customer: &r.Customer, Order: r.Order,
	})
	return err
}

// ReceivePayment books the money a stored payment row brought, if any.
func ReceivePayment(ctx context.Context, q *gen.Queries, p gen.BillingPayment) error {
	r, ok := ReceiptOf(p)
	if !ok {
		return nil
	}
	return New(q, p.MerchantID).Receive(ctx, r)
}

// CashAccount is where a payment's cash sits, by ReceiptOf's rule: its PSP's
// clearing account, or external_receipts for money that came outside
// OpenRails. A reversal returns cash from, and a recovery restores it to,
// the account its payment's receipt debited.
func CashAccount(p gen.BillingPayment) Ref {
	if p.PspID != nil && p.Channel != "manual" && p.MoneyMovement == "rail" {
		psp := *p.PspID
		return Ref{Type: CashClearing, PSP: &psp}
	}
	return Ref{Type: ExternalReceipts}
}

// Started records, once, that the merchant's books started: when the payment
// writer first booked a receipt, by its clock. A later receipt, or a booking
// of an older payment, never moves it.
func Started(ctx context.Context, q *gen.Queries, p gen.BillingPayment, now time.Time) error {
	if _, ok := ReceiptOf(p); !ok || CheckCurrency(p.Currency) != nil {
		return nil
	}
	return q.RecordBooksStart(ctx, gen.RecordBooksStartParams{MerchantID: p.MerchantID, StartedAt: now.UTC().Truncate(time.Microsecond)})
}
