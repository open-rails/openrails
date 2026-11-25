package owed

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ReopenedSource is the source of a usage order line billing owed a
// chargeback reopened: usage billing bills it whatever its minimum.
const ReopenedSource = "chargeback"

// SyncUsageReversals books, once each and in the order they were stored, the
// chargebacks and won disputes of a usage order payment, in the caller's
// transaction. A chargeback reopens the owed the payment paid, less what its
// refunds already gave back: the usage was delivered and the network took the
// cash back, so the debt stands again, owed again in the units book
// (owed_reopened), billed at once on a usage order whatever its minimum, and
// receivable again in the general ledger. Its won dispute settles the
// reopened owed again as funding would, the orders that claim the customer's
// debt first; what it brings beyond that is the customer's, credited as a
// lot at face with a books.dispute.customer_credit finding. A merchant's
// refund reopens nothing: the refund is the usage's contra revenue. A payment
// that settled no owed (not a usage order's) has nothing to sync.
func SyncUsageReversals(ctx context.Context, d *db.DB, merchantID, paymentID uuid.UUID, now time.Time) error {
	q := d.Gen(ctx)
	p, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: merchantID, ID: paymentID})
	if err != nil || p.LedgerTransferID == nil || p.RefundedPaymentID != nil || books.CheckCurrency(p.Currency) != nil {
		return err
	}
	paid, err := q.GetUsageRecoveryLedgerTransfer(ctx, gen.GetUsageRecoveryLedgerTransferParams{MerchantID: merchantID, TransferID: *p.LedgerTransferID})
	if err != nil || paid.TransferType != string(ledger.OwedPayment) {
		return err
	}
	reversals, err := q.ListUsagePaymentReversals(ctx, gen.ListUsagePaymentReversalsParams{MerchantID: merchantID, PaymentID: p.ID})
	if err != nil {
		return err
	}
	source := "usage_payment:" + p.ID.String()
	legs, err := q.ListUsagePaymentReopenings(ctx, gen.ListUsagePaymentReopeningsParams{MerchantID: merchantID, Source: source})
	if err != nil {
		return err
	}
	// What each reversal already posted in the units book, by reversal.
	posted := map[string]int64{}
	for _, leg := range legs {
		id, _, _ := strings.Cut(leg.SourceID, ":")
		posted[id] += leg.Amount
	}
	// And in the general ledger: a reversal already booked is read as it
	// posted, never recomputed, so a refund completed after it changes
	// nothing.
	glRows, err := q.ListPaymentBooksLegs(ctx, gen.ListPaymentBooksLegsParams{MerchantID: merchantID, PaymentID: p.ID})
	if err != nil {
		return err
	}
	booked := map[string]*bookedReversal{}
	for _, row := range glRows {
		b := booked[row.SourceID]
		if b == nil {
			b = &bookedReversal{}
			booked[row.SourceID] = b
		}
		switch typ, op := books.TransferType(row.TransferType), books.Operation(row.Operation); {
		case typ == books.UsageReopened:
			b.reopened += row.Amount
		case typ == books.ChargebackRecognized && row.RevenueScheduleID == nil && op == books.OpChargeback:
			b.loss += row.Amount
		case typ == books.DisputeWon && op == books.OpDisputeWonLoss:
			b.loss += row.Amount
		}
	}
	s := reversalSync{d: d, q: q, l: ledger.New(q, merchantID), gl: books.New(q, merchantID), merchant: merchantID, p: p, source: source, now: now}
	// The owed the payment's chargebacks have reopened, its refunds given
	// back and its chargebacks lost to chargebacks, as of each reversal in
	// turn.
	var reopened, refunded, lost int64
	for _, r := range reversals {
		kind := ""
		if r.ReversalKind != nil {
			kind = *r.ReversalKind
		}
		units, done := posted[r.ID.String()]
		b := booked[r.ID.String()]
		switch {
		case kind == "refund" && r.Amount < 0:
			refunded += -r.Amount
		case kind == "chargeback" && r.Amount < 0 && b != nil:
			reopened, lost = reopened+b.reopened, lost+b.loss
		case kind == "chargeback" && r.Amount < 0:
			owed := min(-r.Amount, max(paid.Amount-reopened-refunded, 0))
			loss, err := s.chargeback(ctx, r, owed, done)
			if err != nil {
				return err
			}
			reopened, lost = reopened+owed, lost+loss
		case kind == "dispute_reversal" && r.Amount > 0 && b != nil:
			reopened, lost = reopened-units, lost-b.loss
		case kind == "dispute_reversal" && r.Amount > 0:
			settled, reversed, err := s.recovery(ctx, r, min(r.Amount, reopened), lost, done, units)
			if err != nil {
				return err
			}
			reopened, lost = reopened-settled, lost-reversed
		}
	}
	return nil
}

// bookedReversal is what the general ledger holds of one reversal of a
// usage order payment: the owed it reopened, and its chargeback's loss or
// the loss a recovery gave back.
type bookedReversal struct{ reopened, loss int64 }

type reversalSync struct {
	d        *db.DB
	q        *gen.Queries
	l        *ledger.Ledger
	gl       *books.Books
	merchant uuid.UUID
	p        gen.BillingPayment
	source   string
	now      time.Time
}

// chargeback, one the general ledger has not booked, owes owed again unless
// its units leg posted, and posts its general-ledger legs, returning its loss
// to chargebacks.
func (s reversalSync) chargeback(ctx context.Context, r gen.BillingPayment, owed int64, done bool) (int64, error) {
	if !done && owed > 0 {
		if _, err := s.l.ReopenOwed(ctx, s.p.CustomerID, s.p.Currency, owed, ledger.Coord{Operation: books.OpChargeback, Source: s.source, SourceID: r.ID.String()}, s.p.OrderID); err != nil {
			return 0, err
		}
		meta, err := json.Marshal(map[string]any{"source": ReopenedSource, "payment_id": s.p.ID.String(), "chargeback_id": r.ID.String()})
		if err != nil {
			return 0, err
		}
		if err := s.q.InsertPendingOwedItem(ctx, gen.InsertPendingOwedItemParams{ID: uuidutil.NewV7(), MerchantID: s.merchant, CustomerID: s.p.CustomerID,
			Currency: s.p.Currency, SourceType: string(ledger.OwedReopened), SourceID: r.ID.String(), AccruedAt: s.now, Amount: owed, Metadata: meta,
			CreatedAt: s.now, UpdatedAt: s.now}); err != nil {
			return 0, err
		}
		// Bill it at once: the customer is never held to debt no order lets
		// them pay.
		if err := s.q.FlagUsageCollectionDue(ctx, gen.FlagUsageCollectionDueParams{MerchantID: s.merchant, CustomerID: s.p.CustomerID, Currency: s.p.Currency, Now: s.now}); err != nil {
			return 0, err
		}
	}
	if err := collectBeforeReopening(ctx, s.q, s.gl, s.p); err != nil {
		return 0, err
	}
	unapplied, err := s.q.GetPaymentUnappliedCash(ctx, gen.GetPaymentUnappliedCashParams{MerchantID: s.merchant, PaymentID: s.p.ID})
	if err != nil {
		return 0, err
	}
	return s.gl.ReopenUsage(ctx, books.UsageReversal{Reversal: r, Original: s.p, Owed: owed, Unapplied: unapplied})
}

// recovery, one the general ledger has not booked, settles up to reopened of
// the owed again unless its units legs posted (units is then what they
// settled), and posts its general-ledger
// legs: it gives back up to lost of the chargebacks' loss, and the cash
// beyond becomes the customer's credit. It returns the owed settled and the
// loss given back.
func (s reversalSync) recovery(ctx context.Context, r gen.BillingPayment, reopened, lost int64, done bool, units int64) (settled, reversed int64, err error) {
	settled = units
	if !done {
		recovery := r.ID
		settled, err = settle(ctx, s.d, s.merchant, s.p.CustomerID, s.p.Currency, reopened, &recovery, s.now, func(amount int64, order *uuid.UUID) error {
			coord := ledger.Coord{Operation: books.OpDisputeWonReopened, Source: s.source, SourceID: r.ID.String()}
			if order != nil {
				coord.SourceID += ":" + order.String()
			}
			_, err := s.l.PayOwed(ctx, s.p.CustomerID, s.p.Currency, amount, coord, order)
			return err
		})
		if err != nil {
			return 0, 0, err
		}
	}
	reversed, excess, err := s.gl.ResettleUsage(ctx, books.UsageReversal{Reversal: r, Original: s.p, Owed: settled, Loss: lost})
	if err != nil || excess <= 0 {
		return settled, reversed, err
	}
	// The cash beyond the owed and the loss is the customer's: credit at
	// face, funded from the unapplied cash it returned to, as a card's
	// rounding change is.
	lots := credits.New(s.q, s.merchant, func() time.Time { return s.now })
	currency := s.p.Currency
	lot, err := lots.Issue(ctx, credits.Issue{Customer: s.p.CustomerID, Unit: currency, Units: excess, Funding: credits.Purchase,
		PaidAmount: &excess, PaidCurrency: &currency, Source: "dispute_won_credit", SourceID: r.ID.String(), Invoker: s.p.CustomerID.String(), Payment: &r.ID})
	if err != nil {
		return settled, reversed, err
	}
	return settled, reversed, s.gl.RaiseCustomerCredit(ctx, r, lot.ID, excess)
}

// collectBeforeReopening books the collection of a payment whose cash the
// general ledger received but applied to nothing (collected before the
// receivable's postings existed), so its chargeback reopens what it
// collected.
func collectBeforeReopening(ctx context.Context, q *gen.Queries, gl *books.Books, p gen.BillingPayment) error {
	posted, err := q.UsageCollectionPosted(ctx, gen.UsageCollectionPostedParams{MerchantID: p.MerchantID, PaymentID: p.ID.String()})
	if err != nil || posted {
		return err
	}
	paid, err := q.GetUsageRecoveryLedgerTransfer(ctx, gen.GetUsageRecoveryLedgerTransferParams{MerchantID: p.MerchantID, TransferID: *p.LedgerTransferID})
	if err != nil {
		return err
	}
	cash, err := q.GetPaymentCashApplication(ctx, gen.GetPaymentCashApplicationParams{MerchantID: p.MerchantID, PaymentID: p.ID.String()})
	if err != nil || cash.Received <= 0 {
		return err
	}
	return gl.CollectUsage(ctx, books.UsageCollection{Payment: p.ID, Customer: p.CustomerID, Order: p.OrderID, Currency: p.Currency,
		Amount: min(paid.Amount, cash.Received-cash.Applied), At: p.PurchasedAt})
}
