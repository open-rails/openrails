package books

// Credit lots in the general ledger.
//
// A paid lot, U units bought for P in a registry currency (whatever the lot's
// own unit), defers P to its customer's deferred_revenue_credit when it is
// issued. Its units are worth P at the rate P/U: n units left are worth
// Worth(n) = P - floor(P*(U-n)/U), so the last unit carries the remainder.
// After every movement of its units the poster brings the lot's deferred money
// to the worth of what is left, posting the difference where the units went:
// usage revenue (or an order line's deferred revenue) for a spend,
// breakage_revenue for an expiry or a revocation, back from breakage_revenue
// for a reinstatement. Being cumulative, rounding never drifts.
//
// A gift (no paid amount) defers nothing. A spend of a gift in a registry
// currency grosses up usage revenue at face against promotional_credit; a
// gift in a merchant's own unit posts nothing.
//
// Owed repaid from a lot is repaid at face: usage_receivable falls by the
// units, the lot's released money pays for them and promotional_credit the
// rest.
//
// The poster for #1379's merchant units is the same: a lot in a merchant unit
// keeps its units in the units book and its money here, in its paid currency.
// A caller moves the lot's units in the units book first, reads the lot
// (CreditLotOf), then calls MoveCredit with that leg's coordinate and instant.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// OpCreditIssue is the operation a paid lot's price is deferred at.
const OpCreditIssue Operation = "credit_issue"

// OpCreditRefund is the operation a reversal of a lot's payment posts at, in
// both books.
const OpCreditRefund Operation = "purchased_credit_refund"

// CreditLot is a credit lot as the general ledger books it.
type CreditLot struct {
	ID, Customer uuid.UUID
	Unit         string
	Units        int64
	// Funding is credit_grants.funding_source.
	Funding string
	// PaidAmount is what was paid for the lot in PaidCurrency; 0 for a gift.
	PaidAmount   int64
	PaidCurrency string
	Product      *uuid.UUID
	CreatedAt    time.Time
	// Remaining is the units left on the lot's balance.
	Remaining                         int64
	DeferredCredited, DeferredDebited int64
}

// CreditLotOf reads a lot's row.
func CreditLotOf(g gen.BillingCreditGrant) CreditLot {
	lot := CreditLot{
		ID: g.ID, Customer: g.CustomerID, Unit: g.Unit, Units: g.Units, Funding: g.FundingSource,
		Product: g.ProductID, CreatedAt: g.CreatedAt, Remaining: g.UnitsCredited - g.UnitsDebited,
		DeferredCredited: g.DeferredCredited, DeferredDebited: g.DeferredDebited,
	}
	switch {
	case g.PaidAmount != nil && g.PaidCurrency != nil:
		lot.PaidAmount, lot.PaidCurrency = *g.PaidAmount, *g.PaidCurrency
	case g.DeferredCredited > 0:
		// A lot given back for revenue taken back (ReverseUsage) is priced
		// at face.
		lot.PaidAmount, lot.PaidCurrency = g.Units, g.Unit
	}
	return lot
}

// Paid reports whether the lot was paid for.
func (l CreditLot) Paid() bool { return l.PaidAmount > 0 }

// Booked reports whether the general ledger deferred the paid lot's price. A
// paid lot from before the credit books is not booked until the books are
// rebuilt; nothing posts for it.
func (l CreditLot) Booked() bool { return l.Paid() && l.DeferredCredited > 0 }

// Deferred is the lot's money still deferred.
func (l CreditLot) Deferred() int64 { return l.DeferredCredited - l.DeferredDebited }

// Worth is what n of the lot's units are worth at its rate: P less the money of
// the units drawn, rounded down, so drawing every unit releases exactly P.
func (l CreditLot) Worth(n int64) int64 {
	if !l.Paid() || l.Units <= 0 {
		return 0
	}
	n = min(max(n, 0), l.Units)
	return l.PaidAmount - mulDiv(l.PaidAmount, l.Units-n, l.Units)
}

// mulDiv is floor(a*b/c) for non-negative a, b and positive c, exact past int64.
func mulDiv(a, b, c int64) int64 {
	if a <= 0 || b <= 0 || c <= 0 {
		return 0
	}
	p := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return p.Quo(p, big.NewInt(c)).Int64()
}

func (l CreditLot) deferredRef() Ref {
	return Ref{Type: DeferredRevenueCredit, Customer: ptrUUID(l.Customer)}
}

// post posts one leg of the lot's money, naming the lot, its customer and
// product; a zero amount posts nothing.
func (b *Books) postLot(ctx context.Context, l CreditLot, debit, credit Ref, amount int64, currency string, typ TransferType, coord Coord, at time.Time) error {
	if amount <= 0 {
		return nil
	}
	customer, lot := l.Customer, l.ID
	_, _, err := b.Post(ctx, Posting{
		Debit: debit, Credit: credit, Amount: amount, Currency: currency, Type: typ, Coord: coord,
		EffectiveAt: at, Customer: &customer, CreditGrant: &lot, Product: l.Product,
	})
	if err != nil {
		return fmt.Errorf("books: credit lot %s %s: %w", l.ID, typ, err)
	}
	return nil
}

// ErrCreditUnfunded is a paid lot issued with neither its payment nor an
// import's declaration: its caller defers it once the payment is recorded.
var ErrCreditUnfunded = errors.New("books: a paid credit lot is deferred from its payment")

// UnappliedCreditFindingType is the finding a paid lot raises when its
// payment brought less cash than its price beyond what was already applied.
const UnappliedCreditFindingType = "books.credit.unapplied"

// IssueCredit defers a paid lot's price, effective when the lot was
// recorded: DR unapplied_cash / CR deferred_revenue_credit at (credit_issue,
// payment, payment id), what its payment applies like a bill's line. It
// applies no more than the payment's receipt left unapplied: what does not
// fit raises UnappliedCreditFindingType, so a payment is never applied beyond
// what it brought. An import that declares a paid amount without a payment is
// deferred from opening_balance_equity at (credit_issue, credit_grant, lot).
// A gift, or a lot already issued, posts nothing.
func (b *Books) IssueCredit(ctx context.Context, l CreditLot, payment *gen.BillingPayment) error {
	if !l.Paid() || l.DeferredCredited > 0 {
		return nil
	}
	if err := CheckCurrency(l.PaidCurrency); err != nil {
		return err
	}
	switch {
	case payment != nil:
		if payment.CustomerID != l.Customer || payment.Currency != l.PaidCurrency {
			return fmt.Errorf("books: payment %s did not pay for credit lot %s", payment.ID, l.ID)
		}
		if err := ReceivePayment(ctx, b.q, *payment); err != nil {
			return err
		}
		cash, err := b.q.GetPaymentCashApplication(ctx, gen.GetPaymentCashApplicationParams{MerchantID: b.merchant, PaymentID: payment.ID.String()})
		if err != nil {
			return fmt.Errorf("books: read payment %s's application: %w", payment.ID, err)
		}
		fits := min(l.PaidAmount, max(cash.Received-cash.Applied, 0))
		if fits < l.PaidAmount {
			if err := b.unappliedCredit(ctx, l, payment.ID, cash.Received, cash.Applied); err != nil {
				return err
			}
		}
		coord := Coord{Operation: OpCreditIssue, Source: "payment", SourceID: payment.ID.String()}
		return b.postLot(ctx, l, Ref{Type: UnappliedCash}, l.deferredRef(), fits, l.PaidCurrency, CashApplied, coord, l.CreatedAt)
	case l.Funding == "import":
		coord := Coord{Operation: OpCreditIssue, Source: "credit_grant", SourceID: l.ID.String()}
		return b.postLot(ctx, l, Ref{Type: OpeningBalanceEquity}, l.deferredRef(), l.PaidAmount, l.PaidCurrency, OpeningBalance, coord, l.CreatedAt)
	}
	return fmt.Errorf("%w: lot %s", ErrCreditUnfunded, l.ID)
}

// unappliedCredit records a paid lot whose payment brought too little cash
// to defer its whole price.
func (b *Books) unappliedCredit(ctx context.Context, l CreditLot, payment uuid.UUID, received, applied int64) error {
	raw, err := json.Marshal(map[string]any{"credit_grant_id": l.ID, "payment_id": payment, "currency": l.PaidCurrency,
		"paid_amount": l.PaidAmount, "received": received, "applied": applied})
	if err != nil {
		return err
	}
	action := "The payment that bought this credit lot brought less cash than its price beyond what the books had already applied of it. The lot defers only what fit; review the payment's receipt and applications."
	_, err = b.q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: b.merchant, FindingType: UnappliedCreditFindingType, SubjectKey: "credit_grant:" + l.ID.String(),
		Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: raw,
	})
	if err != nil {
		return fmt.Errorf("books: raise %s: %w", UnappliedCreditFindingType, err)
	}
	return nil
}

// CreditMoveKind is what happened to a lot's units.
type CreditMoveKind string

const (
	CreditSpent      CreditMoveKind = "spent"
	CreditExpired    CreditMoveKind = "expired"
	CreditRevoked    CreditMoveKind = "revoked"
	CreditReinstated CreditMoveKind = "reinstated"
)

// CreditMove is one units-book movement of a lot's units.
type CreditMove struct {
	Kind CreditMoveKind
	// Units is how many units moved; a gift's spend grosses them up.
	Units int64
	// Coord and At are the units-book leg's coordinate and effective_at.
	Coord Coord
	At    time.Time
	// Into is the order line a spend pays, priced in the lot's unit: its
	// release goes to that line's deferred revenue, which its revenue
	// schedule recognises. nil is usage revenue.
	Into *Line
}

// Line is an order line a spend of credit pays.
type Line struct {
	OrderLine uuid.UUID
	Product   *uuid.UUID
}

// MoveCredit posts the money of a movement of the lot's units, l being the lot
// after the units book moved them. A paid lot's deferred money comes to the
// worth of its units left; a paid lot from before the credit books posts
// nothing.
func (b *Books) MoveCredit(ctx context.Context, l CreditLot, m CreditMove) error {
	if !l.Paid() {
		if m.Kind != CreditSpent || m.Units <= 0 || CheckCurrency(l.Unit) != nil {
			return nil
		}
		return b.postInto(ctx, l, Ref{Type: PromotionalCredit}, m.Units, l.Unit, PromotionalCreditApplied, m)
	}
	if !l.Booked() {
		return nil
	}
	diff := l.Deferred() - l.Worth(l.Remaining)
	switch m.Kind {
	case CreditSpent:
		return b.postInto(ctx, l, l.deferredRef(), diff, l.PaidCurrency, CreditReleased, m)
	case CreditExpired:
		return b.postLot(ctx, l, l.deferredRef(), Ref{Type: BreakageRevenue}, diff, l.PaidCurrency, CreditBreakage, m.Coord, m.At)
	case CreditRevoked:
		return b.postLot(ctx, l, l.deferredRef(), Ref{Type: BreakageRevenue}, diff, l.PaidCurrency, CreditForfeited, m.Coord, m.At)
	case CreditReinstated:
		return b.postLot(ctx, l, Ref{Type: BreakageRevenue}, l.deferredRef(), -diff, l.PaidCurrency, CreditForfeitReversed, m.Coord, m.At)
	}
	return fmt.Errorf("books: unknown credit movement %q", m.Kind)
}

// postInto credits a spend's revenue: usage_revenue, or the deferred revenue
// of the order line it pays.
func (b *Books) postInto(ctx context.Context, l CreditLot, debit Ref, amount int64, currency string, typ TransferType, m CreditMove) error {
	if amount <= 0 {
		return nil
	}
	if m.Into == nil {
		return b.postLot(ctx, l, debit, Ref{Type: UsageRevenue}, amount, currency, typ, m.Coord, m.At)
	}
	customer, lot, line := l.Customer, l.ID, m.Into.OrderLine
	product := m.Into.Product
	if product == nil {
		product = l.Product
	}
	_, _, err := b.Post(ctx, Posting{
		Debit: debit, Credit: Ref{Type: DeferredRevenue}, Amount: amount, Currency: currency, Type: typ, Coord: m.Coord,
		EffectiveAt: m.At, Customer: &customer, CreditGrant: &lot, OrderLine: &line, Product: product,
	})
	if err != nil {
		return fmt.Errorf("books: credit lot %s %s into line %s: %w", l.ID, typ, line, err)
	}
	return nil
}

// RepayFromCredit posts units of a lot repaying owed at face, l being the lot
// after the units book drew them. Of the units, as many as the general
// ledger holds receivable repay it: usage_receivable falls by them, the
// lot's money at its rate pays for them and promotional_credit for the rest
// (a gift's all promotional). The rest repay owed that accrued before the
// credit books, which has no receivable: they are revenue, as a spend is.
// The split is a function of the books when it posts, so a rebuild in time
// order makes the same one.
func (b *Books) RepayFromCredit(ctx context.Context, l CreditLot, units int64, coord Coord, at time.Time) error {
	if units <= 0 || CheckCurrency(l.Unit) != nil {
		return nil
	}
	if l.Paid() && (!l.Booked() || l.PaidCurrency != l.Unit) {
		return b.MoveCredit(ctx, l, CreditMove{Kind: CreditSpent, Units: units, Coord: coord, At: at})
	}
	receivable, err := b.Receivable(ctx, l.Customer, l.Unit)
	if err != nil {
		return err
	}
	accrued := min(max(receivable, 0), units)
	receivableRef := Ref{Type: UsageReceivable, Customer: ptrUUID(l.Customer)}
	money, repaid := int64(0), int64(0)
	if l.Paid() {
		money = max(l.Deferred()-l.Worth(l.Remaining), 0)
		repaid = min(money, accrued)
		if accrued < units {
			repaid = min(mulDiv(accrued, l.PaidAmount, l.Units), repaid)
		}
	}
	if err := b.postLot(ctx, l, l.deferredRef(), receivableRef, repaid, l.Unit, UsageRepaid, coord, at); err != nil {
		return err
	}
	if err := b.postLot(ctx, l, Ref{Type: PromotionalCredit}, receivableRef, accrued-repaid, l.Unit, PromotionalCreditApplied, coord, at); err != nil {
		return err
	}
	if l.Paid() {
		return b.postLot(ctx, l, l.deferredRef(), Ref{Type: UsageRevenue}, money-repaid, l.Unit, CreditReleased, coord, at)
	}
	unaccrued := coord
	unaccrued.Source += ":unaccrued"
	return b.postLot(ctx, l, Ref{Type: PromotionalCredit}, Ref{Type: UsageRevenue}, units-accrued, l.Unit, PromotionalCreditApplied, unaccrued, at)
}

// CreditReversal is a refund or chargeback of a booked lot's payment, l
// being the lot after the units book took its units.
type CreditReversal struct {
	// Reversal is the negative payment.
	Reversal   uuid.UUID
	Chargeback bool
	// Cash is what the reversal returned of the lot's price, from CashAccount.
	Cash        int64
	CashAccount Ref
	// Repaid is the units of it that had repaid owed: owed again, at face.
	Repaid int64
	At     time.Time
}

// The labels of a reversal's legs, by what of the lot's money they return.
const (
	reversedDeferred   = "money_deferred"
	reversedExcess     = "money_deferred_excess"
	reversedRepaid     = "money_repaid"
	reversedPromotion  = "money_repaid_promotion"
	reversedRecognized = "money_recognized"
)

// ReverseCredit posts a reversal of a booked lot's payment at
// (purchased_credit_refund, label, reversal), as the units book allocated it:
// the lot's deferred money comes to the worth of its units left, the cash
// paying for it (refund_deferred or chargeback_deferred) and any of it beyond
// the cash going back against the contra account, so none is stranded; owed
// it had repaid is receivable again at face, the money at the lot's rate
// returning in cash and promotional_credit the rest (usage_reopened); the
// rest of the cash was recognised, expired, forfeited or consumed, and is
// contra-revenue (refund_recognized or chargeback_recognized).
func (b *Books) ReverseCredit(ctx context.Context, l CreditLot, r CreditReversal) error {
	if !l.Booked() || r.Cash <= 0 {
		return nil
	}
	coord := func(label string) Coord {
		return Coord{Operation: OpCreditRefund, Source: label, SourceID: r.Reversal.String()}
	}
	deferred := max(l.Deferred()-l.Worth(l.Remaining), 0)
	paid := min(deferred, r.Cash)
	repaid := min(mulDiv(r.Repaid, l.PaidAmount, l.Units), r.Cash-paid, r.Repaid)
	contra, deferredType, recognizedType := Refunds, RefundDeferred, RefundRecognized
	if r.Chargeback {
		contra, deferredType, recognizedType = Chargebacks, ChargebackDeferred, ChargebackRecognized
	}
	receivableRef := Ref{Type: UsageReceivable, Customer: ptrUUID(l.Customer)}
	c := l.PaidCurrency
	for _, leg := range []struct {
		debit, credit Ref
		amount        int64
		typ           TransferType
		label         string
	}{
		{l.deferredRef(), r.CashAccount, paid, deferredType, reversedDeferred},
		{l.deferredRef(), Ref{Type: contra}, deferred - paid, deferredType, reversedExcess},
		{receivableRef, r.CashAccount, repaid, UsageReopened, reversedRepaid},
		{receivableRef, Ref{Type: PromotionalCredit}, r.Repaid - repaid, UsageReopened, reversedPromotion},
		{Ref{Type: contra}, r.CashAccount, r.Cash - paid - repaid, recognizedType, reversedRecognized},
	} {
		if err := b.postLot(ctx, l, leg.debit, leg.credit, leg.amount, c, leg.typ, coord(leg.label), r.At); err != nil {
			return err
		}
	}
	return nil
}

// CreditRecovery is a won dispute restoring cash an earlier reversal of a
// booked lot's payment took, l being the lot after the units book restored
// its units.
type CreditRecovery struct {
	// Recovery is the positive payment; Origin the reversal it recovers.
	Recovery, Origin uuid.UUID
	Chargeback       bool
	// Cash is what this recovery restores of the origin's cash.
	Cash        int64
	CashAccount Ref
	// Repaid is the units of owed the recovery repaid again; Retired the
	// units it restored to a revoked or lapsed lot's retired account, whose
	// money the caller then forfeits or breaks (MoveCredit).
	Repaid, Retired int64
	At              time.Time
}

// RecoverCredit posts a recovery as the units book allocated it, mirroring
// ReverseCredit (dispute_won): the lot's deferred money comes to the worth of
// its units left and retired, from the cash and, beyond it, back against the
// contra account; owed it repaid again leaves the receivable at face, the
// money at the lot's rate in cash and promotional_credit the rest; the rest
// of the cash reverses contra-revenue.
func (b *Books) RecoverCredit(ctx context.Context, l CreditLot, r CreditRecovery) error {
	if !l.Booked() || r.Cash <= 0 {
		return nil
	}
	receivable, err := b.Receivable(ctx, l.Customer, l.Unit)
	if err != nil {
		return err
	}
	deferred := max(l.Worth(l.Remaining+r.Retired)-l.Deferred(), 0)
	paid := min(deferred, r.Cash)
	owed := min(r.Repaid, max(receivable, 0))
	repaid := min(mulDiv(owed, l.PaidAmount, l.Units), r.Cash-paid, owed)
	contra := Refunds
	if r.Chargeback {
		contra = Chargebacks
	}
	coord := func(label string) Coord {
		return Coord{Operation: OpCreditRefund, Source: label + "_restore:" + r.Origin.String(), SourceID: r.Recovery.String()}
	}
	receivableRef := Ref{Type: UsageReceivable, Customer: ptrUUID(l.Customer)}
	for _, leg := range []struct {
		debit, credit Ref
		amount        int64
		label         string
	}{
		{r.CashAccount, l.deferredRef(), paid, reversedDeferred},
		{Ref{Type: contra}, l.deferredRef(), deferred - paid, reversedExcess},
		{r.CashAccount, receivableRef, repaid, reversedRepaid},
		{Ref{Type: PromotionalCredit}, receivableRef, owed - repaid, reversedPromotion},
		{r.CashAccount, Ref{Type: contra}, r.Cash - paid - repaid, reversedRecognized},
	} {
		if err := b.postLot(ctx, l, leg.debit, leg.credit, leg.amount, l.PaidCurrency, DisputeWon, coord(leg.label), r.At); err != nil {
			return err
		}
	}
	return nil
}

func ptrUUID(id uuid.UUID) *uuid.UUID { return &id }
