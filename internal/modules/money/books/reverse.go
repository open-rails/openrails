package books

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// The operations of a reversal's legs, each at (operation, payment, the
// reversal's id) and the invoice line it moves.
const (
	OpRefund     Operation = "refund"
	OpChargeback Operation = "chargeback"
	// OpDisputeWon restores what a chargeback took from unapplied cash and
	// from the contra; OpDisputeWonDeferred what it took from deferred
	// revenue; OpDisputeWonReopened the owed a usage invoice's chargeback
	// reopened, collected again (ResettleUsage).
	OpDisputeWon         Operation = "dispute_won"
	OpDisputeWonDeferred Operation = "dispute_won_deferred"
	OpDisputeWonReopened Operation = "dispute_won_reopened"
)

// reversal is how one kind of reversal posts.
type reversal struct {
	op                   Operation
	contra               AccountType
	deferred, recognized TransferType
}

var (
	refundReversal     = reversal{op: OpRefund, contra: Refunds, deferred: RefundDeferred, recognized: RefundRecognized}
	chargebackReversal = reversal{op: OpChargeback, contra: Chargebacks, deferred: ChargebackDeferred, recognized: ChargebackRecognized}
)

// PostReversal books a settled reversal of a payment, once. A refund's or a
// chargeback's money leaves its cash account from what the payment left
// unapplied first, then from each schedule it funded, pro rata. A schedule
// still serving takes a price reduction with catch-up: what it earned by
// then is recognised, the reduction's earned share goes to refunds or
// chargebacks and the rest comes out of deferred revenue. A schedule whose
// service has ended gives up its unrecognised revenue first, then the
// contra. A won dispute's recovery brings back, pro rata, what its
// chargeback took. A reversal of money the books never received (a payment
// stored before them) is drawn from unapplied cash: never refused, it
// records a sign breach. now decides whether a service has ended.
func PostReversal(ctx context.Context, d *db.DB, reversalID uuid.UUID, now time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := d.Gen(ctx)
	r, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: reversalID})
	if err != nil {
		return err
	}
	if r.RefundedPaymentID == nil || r.ReversalKind == nil || r.Status != "succeeded" || r.DeletedAt != nil || r.Amount == 0 || CheckCurrency(r.Currency) != nil {
		return nil
	}
	p, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: *r.RefundedPaymentID})
	if err != nil {
		return err
	}
	_, ok := ReceiptOf(p)
	if !ok {
		return nil
	}
	posted, err := q.PaymentPosted(ctx, gen.PaymentPostedParams{MerchantID: mid.UUID(), PaymentID: r.ID.String()})
	if err != nil || posted {
		return err
	}
	// The credit lot the payment bought is first: its poster has moved its
	// share already (books.ReverseCredit, books.RecoverCredit). The rest is
	// this one's, all of it when the lot was never booked.
	lot, err := q.GetLotReversalCash(ctx, gen.GetLotReversalCashParams{MerchantID: mid.UUID(), CustomerID: r.CustomerID, Currency: r.Currency, ReversalID: r.ID.String()})
	if err != nil {
		return err
	}
	amount := -r.Amount - lot.Returned
	if r.Amount > 0 {
		amount = r.Amount - lot.Restored
	}
	if amount <= 0 {
		return nil
	}
	usage := false
	if p.InvoiceID != nil {
		invoice, err := q.GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: mid.UUID(), ID: *p.InvoiceID})
		if err != nil {
			return err
		}
		usage = invoice.Origin == "usage"
	}
	pb, err := paymentBooksOf(ctx, q, mid.UUID(), p.ID)
	if err != nil {
		return err
	}
	w := reverser{b: New(q, mid.UUID()), q: q, d: d, merchant: mid.UUID(), reversal: r, cash: CashAccount(p), at: SettledAt(r), now: now.UTC().Truncate(time.Microsecond)}
	switch {
	case *r.ReversalKind == "refund" && r.Amount < 0 && usage:
		// A merchant's refund of usage it delivered reopens nothing owed:
		// beyond what the payment left unapplied it is the usage's contra.
		fromUnapplied := min(amount, max(pb.unapplied(), 0))
		if fromUnapplied > 0 {
			if err := w.post(ctx, Ref{Type: UnappliedCash}, w.cash, CashReturned, OpRefund, fromUnapplied, nil); err != nil {
				return err
			}
		}
		if contra := amount - fromUnapplied; contra > 0 {
			return w.post(ctx, Ref{Type: Refunds}, w.cash, RefundRecognized, OpRefund, contra, nil)
		}
		return nil
	case usage:
		// A usage invoice's chargeback reopens what the customer owes and its
		// won dispute collects it again: owed.SyncUsageReversals books both
		// (ReopenUsage, ResettleUsage) before this would.
		return nil
	case *r.ReversalKind == "refund" && r.Amount < 0:
		return w.reverse(ctx, refundReversal, amount, pb)
	case *r.ReversalKind == "chargeback" && r.Amount < 0:
		return w.reverse(ctx, chargebackReversal, amount, pb)
	case *r.ReversalKind == "dispute_reversal" && r.Amount > 0:
		return w.restore(ctx, amount, pb)
	}
	return nil
}

// SettledAt is when a reversal settled, the date of its legs: the rail's
// confirmation, or the provider's occurrence time when that is what is known.
func SettledAt(r gen.BillingPayment) time.Time {
	return r.PurchasedAt.UTC().Truncate(time.Microsecond)
}

// taken is what a reversal took from one schedule: from its deferred
// revenue and from its recognised revenue (the contra). A won dispute's legs
// are negative.
type taken struct{ deferred, contra int64 }

func (t taken) total() int64 { return t.deferred + t.contra }

// reversalLegs is what one reversal of a payment posted: from unapplied
// cash, as owed reopened (a usage invoice's chargeback) and per schedule.
type reversalLegs struct {
	chargeback         bool
	returned, reopened int64
	schedules          map[uuid.UUID]taken
}

// paymentBooks is what the general ledger holds of one payment and its
// reversals.
type paymentBooks struct {
	received int64
	// applied is its cash applied to each schedule; other, applied to none
	// (a credit line's lot, a usage receivable).
	applied map[uuid.UUID]int64
	other   int64
	// returned is what its reversals took from unapplied cash, net of what
	// won disputes brought back.
	returned int64
	legs     map[string]*reversalLegs
}

func paymentBooksOf(ctx context.Context, q *gen.Queries, merchant, payment uuid.UUID) (paymentBooks, error) {
	rows, err := q.ListPaymentBooksLegs(ctx, gen.ListPaymentBooksLegsParams{MerchantID: merchant, PaymentID: payment})
	if err != nil {
		return paymentBooks{}, err
	}
	return paymentBooksFrom(payment, rows), nil
}

// paymentBooksFrom sums the legs of a payment and its reversals.
func paymentBooksFrom(payment uuid.UUID, rows []gen.ListPaymentBooksLegsRow) paymentBooks {
	pb := paymentBooks{applied: map[uuid.UUID]int64{}, legs: map[string]*reversalLegs{}}
	own := payment.String()
	for _, row := range rows {
		typ, op := TransferType(row.TransferType), Operation(row.Operation)
		if row.SourceID == own {
			switch {
			case typ == CashReceived:
				pb.received += row.Amount
			case typ == CashApplied && row.RevenueScheduleID != nil:
				pb.applied[*row.RevenueScheduleID] += row.Amount
			case typ == CashApplied, typ == UsageCollected:
				// A usage payment's cash settling its receivable is applied
				// too, to no schedule.
				pb.other += row.Amount
			}
			continue
		}
		l := pb.legs[row.SourceID]
		if l == nil {
			l = &reversalLegs{schedules: map[uuid.UUID]taken{}}
			pb.legs[row.SourceID] = l
		}
		var s taken
		switch {
		case typ == CashReturned:
			l.returned += row.Amount
		case typ == UsageReopened && row.RevenueScheduleID == nil:
			l.reopened += row.Amount
		case typ == DisputeWon && (op == OpDisputeWonReopened || op == OpDisputeWonRevenue):
			l.reopened -= row.Amount
		case typ == DisputeWon && op == OpDisputeWonLoss:
			// A usage chargeback's loss given back: no unapplied cash moves.
		case typ == CashApplied && row.RevenueScheduleID == nil:
			// A credit lot funded from what a usage invoice payment's won
			// dispute returned to unapplied cash takes it from there again.
			l.returned += row.Amount
		case typ == DisputeWon && row.RevenueScheduleID == nil:
			l.returned -= row.Amount
		case row.RevenueScheduleID == nil:
		case typ == RefundDeferred || typ == ChargebackDeferred:
			s.deferred = row.Amount
		case typ == RefundRecognized || typ == ChargebackRecognized:
			s.contra = row.Amount
		case typ == DisputeWon && op == OpDisputeWonDeferred:
			s.deferred = -row.Amount
		case typ == DisputeWon:
			s.contra = -row.Amount
		}
		l.chargeback = l.chargeback || typ == ChargebackDeferred || typ == ChargebackRecognized || op == OpChargeback
		if row.RevenueScheduleID != nil && s != (taken{}) {
			t := l.schedules[*row.RevenueScheduleID]
			t.deferred, t.contra = t.deferred+s.deferred, t.contra+s.contra
			l.schedules[*row.RevenueScheduleID] = t
		}
	}
	for _, l := range pb.legs {
		pb.returned += l.returned
	}
	return pb
}

// unapplied is what of the payment's money no bill applied and no reversal
// took.
func (pb paymentBooks) unapplied() int64 {
	u := pb.received - pb.other - pb.returned
	for _, a := range pb.applied {
		u -= a
	}
	return u
}

// used is what of the payment's money its bills and reversals took.
func (pb paymentBooks) used() int64 { return pb.received - pb.unapplied() }

// held is what each schedule still holds of the payment's money.
func (pb paymentBooks) held() map[uuid.UUID]int64 {
	out := map[uuid.UUID]int64{}
	for id, a := range pb.applied {
		left := a
		for _, l := range pb.legs {
			left -= l.schedules[id].total()
		}
		if left > 0 {
			out[id] = left
		}
	}
	return out
}

// bySchedule invoices amounts by schedule.
func bySchedule(amounts map[uuid.UUID]int64) ([]uuid.UUID, []int64) {
	ids := make([]uuid.UUID, 0, len(amounts))
	for id := range amounts {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	out := make([]int64, len(ids))
	for i, id := range ids {
		out[i] = amounts[id]
	}
	return ids, out
}

// reverser posts one reversal.
type reverser struct {
	b        *Books
	q        *gen.Queries
	d        *db.DB
	merchant uuid.UUID
	reversal gen.BillingPayment
	cash     Ref
	at, now  time.Time
	loc      *time.Location
}

// reverse takes amount from the payment: unapplied cash first, then each
// schedule it funded, pro rata, cumulatively rounded. Anything left (money
// the books never received) comes from unapplied cash too.
func (w *reverser) reverse(ctx context.Context, k reversal, amount int64, pb paymentBooks) error {
	fromUnapplied := min(amount, max(pb.unapplied(), 0))
	ids, held := bySchedule(pb.held())
	parts := spread(min(amount-fromUnapplied, sum(held)), held)
	fromUnapplied = amount - sum(parts)
	if fromUnapplied > 0 {
		if err := w.post(ctx, Ref{Type: UnappliedCash}, w.cash, CashReturned, k.op, fromUnapplied, nil); err != nil {
			return err
		}
	}
	for i, id := range ids {
		if parts[i] > 0 {
			if err := w.adjust(ctx, id, -parts[i], 0, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// restore brings back, pro rata, what the chargeback a won dispute reverses
// took: to unapplied cash and to each schedule. A recovery its chargeback
// accounts for none of (one the books never held) stays in unapplied cash.
func (w *reverser) restore(ctx context.Context, amount int64, pb paymentBooks) error {
	reversals, err := reversalsOf(ctx, w.q, w.merchant, *w.reversal.RefundedPaymentID)
	if err != nil {
		return err
	}
	c := pb.chargebackLegs(reversals, rowOf(w.reversal), amount)
	ids, shares := bySchedule(c.totals())
	legs := append([]int64{max(c.returned, 0)}, shares...)
	parts := spread(min(amount, sum(legs)), legs)
	if toUnapplied := amount - sum(parts[1:]); toUnapplied > 0 {
		if err := w.post(ctx, w.cash, Ref{Type: UnappliedCash}, DisputeWon, OpDisputeWon, toUnapplied, nil); err != nil {
			return err
		}
	}
	for i, id := range ids {
		part := parts[i+1]
		if part <= 0 {
			continue
		}
		t := c.schedules[id]
		contra := spread(part, []int64{max(t.contra, 0), max(t.deferred, 0)})[0]
		if err := w.adjust(ctx, id, part, contra, chargebackReversal); err != nil {
			return err
		}
	}
	return nil
}

// reversalRow is one reversal of a payment, as its row records it.
type reversalRow struct {
	id, kind, reverses string
	// at is when it occurred; recorded, when it was stored.
	at, recorded time.Time
}

func rowOf(r gen.BillingPayment) reversalRow {
	kind := ""
	if r.ReversalKind != nil {
		kind = *r.ReversalKind
	}
	return reversalRow{id: r.ID.String(), kind: kind, reverses: reversesOf(r), at: r.PurchasedAt, recorded: r.CreatedAt}
}

// reversalsOf reads a payment's settled reversals, oldest first by when they
// occurred.
func reversalsOf(ctx context.Context, q *gen.Queries, merchant, payment uuid.UUID) ([]reversalRow, error) {
	rows, err := q.ListRefundsForPayment(ctx, gen.ListRefundsForPaymentParams{MerchantID: merchant, RefundedPaymentID: &payment})
	if err != nil {
		return nil, err
	}
	var out []reversalRow
	for _, r := range rows {
		if r.Status == "succeeded" && r.ReversalKind != nil {
			out = append(out, rowOf(r))
		}
	}
	slices.SortFunc(out, func(a, b reversalRow) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	return out, nil
}

// reversesOf is the chargeback a won dispute's recovery names, if any.
func reversesOf(r gen.BillingPayment) string {
	var meta struct {
		Reverses string `json:"reverses_payment_id"`
	}
	if len(r.Metadata) > 0 && json.Unmarshal(r.Metadata, &meta) == nil {
		return meta.Reverses
	}
	return ""
}

// chargebackLegs is what a recovery brings back. Naming a chargeback, what
// earlier recoveries left of it; naming none, amount of what the chargebacks
// stored before it have left, the oldest by occurrence first, each in its own
// proportions. Earlier recoveries are replayed in the order they were stored,
// each against the chargebacks stored by then, so a chargeback observed late
// never takes back what an earlier recovery restored.
func (pb paymentBooks) chargebackLegs(reversals []reversalRow, current reversalRow, amount int64) *reversalLegs {
	brought := map[string]*reversalLegs{}
	var chargebacks []reversalRow
	for _, r := range reversals {
		if r.kind == "chargeback" && pb.legs[r.id] != nil {
			chargebacks = append(chargebacks, r)
			brought[r.id] = newLegs()
		}
	}
	left := func(id string) *reversalLegs { return pb.legs[id].less(brought[id]) }
	fifo := func(by reversalRow, amount int64) *reversalLegs {
		out := newLegs()
		for _, c := range chargebacks {
			if c.recorded.After(by.recorded) {
				continue
			}
			l := left(c.id)
			portion := min(amount, l.total())
			if portion <= 0 {
				continue
			}
			amount -= portion
			part := l.share(portion)
			brought[c.id].add(part)
			out.add(part)
		}
		return out
	}
	var earlier []reversalRow
	for _, r := range reversals {
		if r.kind == "dispute_reversal" && r.id != current.id && pb.legs[r.id] != nil && r.storedBefore(current) {
			earlier = append(earlier, r)
		}
	}
	slices.SortFunc(earlier, func(a, b reversalRow) int {
		if c := a.recorded.Compare(b.recorded); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	for _, r := range earlier {
		got := pb.legs[r.id].negated()
		switch {
		case r.reverses == "":
			fifo(r, got.total())
		case brought[r.reverses] != nil:
			brought[r.reverses].add(got)
		}
	}
	switch {
	case current.reverses == "":
		return fifo(current, amount)
	case brought[current.reverses] != nil:
		return left(current.reverses)
	}
	return newLegs()
}

func (r reversalRow) storedBefore(s reversalRow) bool {
	if c := r.recorded.Compare(s.recorded); c != 0 {
		return c < 0
	}
	return r.id < s.id
}

func newLegs() *reversalLegs { return &reversalLegs{schedules: map[uuid.UUID]taken{}} }

func (l *reversalLegs) add(o *reversalLegs) {
	l.returned += o.returned
	l.reopened += o.reopened
	for id, t := range o.schedules {
		s := l.schedules[id]
		l.schedules[id] = taken{deferred: s.deferred + t.deferred, contra: s.contra + t.contra}
	}
}

// negated is a recovery's legs as what it brought back.
func (l *reversalLegs) negated() *reversalLegs {
	out := &reversalLegs{returned: -l.returned, reopened: -l.reopened, schedules: map[uuid.UUID]taken{}}
	for id, t := range l.schedules {
		out.schedules[id] = taken{deferred: -t.deferred, contra: -t.contra}
	}
	return out
}

// less is what of a chargeback's legs recoveries bringing back brought leave,
// never below nothing: each schedule's rest in the chargeback's own split.
func (l *reversalLegs) less(brought *reversalLegs) *reversalLegs {
	out := &reversalLegs{chargeback: true, returned: max(l.returned-brought.returned, 0), reopened: max(l.reopened-brought.reopened, 0), schedules: map[uuid.UUID]taken{}}
	for id, t := range l.schedules {
		rest := max(t.total()-brought.schedules[id].total(), 0)
		if rest == 0 {
			continue
		}
		contra := spread(rest, []int64{max(t.contra, 0), max(t.deferred, 0)})[0]
		out.schedules[id] = taken{contra: contra, deferred: rest - contra}
	}
	return out
}

// share is amount of l, spread over its parts in their proportions.
func (l *reversalLegs) share(amount int64) *reversalLegs {
	out := newLegs()
	ids, shares := bySchedule(l.totals())
	parts := spread(amount, append([]int64{max(l.returned, 0), max(l.reopened, 0)}, shares...))
	out.returned, out.reopened = parts[0], parts[1]
	for k, id := range ids {
		t := l.schedules[id]
		contra := spread(parts[k+2], []int64{max(t.contra, 0), max(t.deferred, 0)})[0]
		out.schedules[id] = taken{contra: contra, deferred: parts[k+2] - contra}
	}
	return out
}

// totals is what a reversal took from each schedule.
func (l *reversalLegs) totals() map[uuid.UUID]int64 {
	out := map[uuid.UUID]int64{}
	for id, t := range l.schedules {
		if t.total() > 0 {
			out[id] = t.total()
		}
	}
	return out
}

// total is all a reversal took (negative: all a recovery restored).
func (l *reversalLegs) total() int64 {
	t := l.returned + l.reopened
	for _, s := range l.schedules {
		t += s.total()
	}
	return t
}

// adjust moves one schedule's funding by delta: a reversal's share
// (negative) or a won dispute's (positive; contra the part of it that went
// to recognised revenue). The schedule first recognises what its service
// earned: up to the reversal while it serves, else to its end, where an
// early end's forfeit waits for the job, after the reversal.
func (w *reverser) adjust(ctx context.Context, id uuid.UUID, delta, contra int64, k reversal) error {
	row, err := w.q.LockRevenueSchedule(ctx, gen.LockRevenueScheduleParams{MerchantID: w.merchant, ID: id})
	if err != nil {
		return err
	}
	if w.loc == nil {
		if w.loc, err = AccountingZone(ctx, w.d); err != nil {
			return err
		}
	}
	s, contra, deferred, earned, err := w.b.moveFunding(ctx, row, delta, contra, w.at, w.now, w.loc)
	if err != nil {
		return err
	}
	if delta < 0 {
		if contra > 0 {
			if err := w.post(ctx, Ref{Type: k.contra}, w.cash, k.recognized, k.op, contra, &row); err != nil {
				return err
			}
		}
		if deferred > 0 {
			if err := w.post(ctx, Ref{Type: DeferredRevenue}, w.cash, k.deferred, k.op, deferred, &row); err != nil {
				return err
			}
		}
	} else {
		if contra > 0 {
			if err := w.post(ctx, w.cash, Ref{Type: Chargebacks}, DisputeWon, OpDisputeWon, contra, &row); err != nil {
				return err
			}
		}
		if deferred > 0 {
			if err := w.post(ctx, w.cash, Ref{Type: DeferredRevenue}, DisputeWon, OpDisputeWonDeferred, deferred, &row); err != nil {
				return err
			}
		}
		if earned > 0 {
			if err := w.post(ctx, Ref{Type: DeferredRevenue}, Ref{Type: AccountType(row.RevenueAccount)}, RevenueRecognized, OpDisputeWon, earned, &row); err != nil {
				return err
			}
		}
	}
	return w.b.saveSchedule(ctx, id, s, w.loc)
}

// moveFunding changes a locked schedule's funding by delta: a reversal's
// share (negative) or a recovery's (positive, contra the part of it that
// went to recognised revenue). The schedule first recognises what its
// service earned: up to the change while it serves, else to now. It answers
// the schedule changed and the change's contra, deferred and earned parts.
func (b *Books) moveFunding(ctx context.Context, row gen.BillingRevenueSchedule, delta, contra int64, at, now time.Time, loc *time.Location) (s schedule, c, deferred, earned int64, err error) {
	s = scheduleOf(row)
	end := s.end()
	t := maxTime(at, s.through)
	serving := now.Before(end) && t.Before(end)
	var steps []recognition
	if serving {
		if steps, _, err = s.progress(ctx, loc, t, false); err != nil {
			return s, 0, 0, 0, err
		}
		more, _ := s.catchUp(t, loc)
		steps = append(steps, more...)
	} else if steps, _, err = s.progress(ctx, loc, now, false); err != nil {
		return s, 0, 0, 0, err
	}
	if err = b.recognize(ctx, row, steps); err != nil {
		return s, 0, 0, 0, err
	}
	if delta < 0 {
		c, deferred = s.reduce(-delta, serving)
	} else {
		c, deferred, earned = s.restore(delta, contra, serving, end)
	}
	return s, c, deferred, earned, nil
}

// saveSchedule records a schedule's change.
func (b *Books) saveSchedule(ctx context.Context, id uuid.UUID, s schedule, loc *time.Location) error {
	n, err := b.q.AdjustRevenueSchedule(ctx, gen.AdjustRevenueScheduleParams{
		MerchantID: b.merchant, ID: id, Funded: s.funded, RecognizedNet: s.recognized, RecognizedThroughAt: s.through,
		NextRecognitionAt: s.nextDue(loc), Reinstatements: int32(s.reinstatements), // #nosec G115 -- one per undone end
		Forfeited: s.forfeited,
	})
	if err == nil && n != 1 {
		err = fmt.Errorf("books: revenue schedule %s did not record its change", id)
	}
	return err
}

// post books one leg of the reversal, attributed to the schedule's line
// when it moves one.
func (w *reverser) post(ctx context.Context, debit, credit Ref, typ TransferType, op Operation, amount int64, row *gen.BillingRevenueSchedule) error {
	p := Posting{
		Debit: debit, Credit: credit, Amount: amount, Currency: w.reversal.Currency, Type: typ,
		Coord:       Coord{Operation: op, Source: "payment", SourceID: w.reversal.ID.String()},
		EffectiveAt: w.at, Customer: &w.reversal.CustomerID, Invoice: w.reversal.InvoiceID,
	}
	if row != nil {
		p.Invoice, p.InvoiceLine, p.Product, p.RevenueSchedule = &row.InvoiceID, &row.InvoiceLineID, row.ProductID, &row.ID
	}
	_, applied, err := w.b.Post(ctx, p)
	if err == nil && !applied {
		err = fmt.Errorf("%w: reversal %s already posted %s", ErrCoordinateReused, w.reversal.ID, p.Coord)
	}
	return err
}

// reduce takes x from the schedule's funding, past what its service earned
// so far. Serving, it is a price reduction: recognised revenue falls to what
// the lower price earned by then, the difference the contra. Ended,
// unrecognised revenue goes first, then the contra. It returns both legs.
func (s *schedule) reduce(x int64, serving bool) (contra, deferred int64) {
	if serving {
		s.funded -= x
		was := s.recognized
		s.recognized = s.service.target(s.funded, s.through)
		contra = was - s.recognized
		return contra, x - contra
	}
	deferred = min(x, s.funded-s.recognized)
	contra = x - deferred
	s.funded -= x
	s.recognized -= contra
	// The contra comes out of what an early end forfeited first: no undone
	// end can take it back.
	s.forfeited -= min(contra, s.forfeited)
	return contra, deferred
}

// restore brings y back to the schedule's funding. Serving, it is a price
// restored: recognised revenue rises to what the higher price earned by
// then. Ended, contra of it returns to recognised revenue, the share of the
// rest the service earned by end is recognised too, and what remains waits
// for an early end's forfeit, or is forfeited now (earned) when none is to
// come.
func (s *schedule) restore(y, contra int64, serving bool, end time.Time) (c, deferred, earned int64) {
	if serving {
		s.funded += y
		was := s.recognized
		s.recognized = s.service.target(s.funded, s.through)
		c = s.recognized - was
		return c, y - c, 0
	}
	pending := s.ended != nil && s.recognized < s.funded
	c = min(contra, y)
	s.funded += y
	s.recognized += c
	earned = max(s.service.target(s.funded, end)-s.recognized, 0)
	if !pending {
		// Forfeited now: what the service did not earn by its end.
		s.forfeited += s.funded - s.recognized - earned
		earned = s.funded - s.recognized
	}
	s.recognized += earned
	return c, y - c, earned
}
