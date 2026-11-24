package books

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Overapplication is a payment whose cash the general ledger applied to bills
// and the credit lot it bought beyond what it received: every payment's
// applications must stay within its receipt.
type Overapplication struct {
	PaymentID string `json:"payment_id"`
	Currency  string `json:"currency"`
	Received  int64  `json:"received"`
	Applied   int64  `json:"applied"`
}

func (o Overapplication) String() string {
	return fmt.Sprintf("overapplied payment: %s received=%s applied=%s",
		o.PaymentID, moneyutil.FormatAmount(o.Received, o.Currency), moneyutil.FormatAmount(o.Applied, o.Currency))
}

// CheckApplications returns the merchant's payments applied beyond their
// receipts. An empty slice is the healthy answer.
func CheckApplications(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]Overapplication, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("books: merchant is required")
	}
	rows, err := gen.New(q).ListOverappliedPayments(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("books: application check: %w", err)
	}
	var out []Overapplication
	for _, r := range rows {
		out = append(out, Overapplication{PaymentID: r.PaymentID, Currency: r.Currency, Received: r.Received, Applied: r.Applied})
	}
	return out, nil
}

// UnscheduledLine is a completed bill's revenue line with no revenue
// schedule. Reason says why: unscheduled (money paid for it recognises
// nothing: a failure), before_books (its order was made and completed before
// the merchant's books started; the books' rebuild schedules it) or
// adjustment (a provider's tax or shipping, unapplied until its tax is
// booked). The last two are warnings.
type UnscheduledLine struct {
	OrderID     uuid.UUID `json:"order_id"`
	OrderLineID uuid.UUID `json:"order_line_id"`
	Currency    string    `json:"currency"`
	Amount      int64     `json:"amount"`
	Reason      string    `json:"reason"`
}

// Reasons a revenue line has no schedule.
const (
	LineUnscheduled = "unscheduled"
	LineBeforeBooks = "before_books"
	LineAdjustment  = "adjustment"
)

func (u UnscheduledLine) String() string {
	amount := moneyutil.FormatAmount(u.Amount, u.Currency)
	switch u.Reason {
	case LineBeforeBooks:
		return fmt.Sprintf("unscheduled line: order=%s line=%s amount=%s completed before the books started, awaiting their rebuild (warning)", u.OrderID, u.OrderLineID, amount)
	case LineAdjustment:
		return fmt.Sprintf("unapplied adjustment: order=%s line=%s amount=%s awaits its tax booking (warning)", u.OrderID, u.OrderLineID, amount)
	}
	return fmt.Sprintf("unscheduled line: order=%s line=%s amount=%s", u.OrderID, u.OrderLineID, amount)
}

// CheckSchedules returns the merchant's completed revenue lines without a
// schedule: those that fail the books, then the warnings. An order in a
// currency the books do not keep (a merchant's own unit) is never applied
// and is not listed.
func CheckSchedules(ctx context.Context, q gen.DBTX, merchant uuid.UUID) (failing, warnings []UnscheduledLine, err error) {
	if merchant == uuid.Nil {
		return nil, nil, fmt.Errorf("books: merchant is required")
	}
	rows, err := gen.New(q).ListUnscheduledOrderLines(ctx, merchant)
	if err != nil {
		return nil, nil, fmt.Errorf("books: schedule check: %w", err)
	}
	for _, r := range rows {
		if CheckCurrency(r.Currency) != nil {
			continue
		}
		line := UnscheduledLine{OrderID: r.OrderID, OrderLineID: r.OrderLineID, Currency: r.Currency, Amount: r.Amount, Reason: LineUnscheduled}
		switch {
		case r.Adjustment:
			line.Reason = LineAdjustment
		case r.BeforeBooks:
			line.Reason = LineBeforeBooks
		default:
			failing = append(failing, line)
			continue
		}
		warnings = append(warnings, line)
	}
	return failing, warnings, nil
}

// UnpostedFindingType is the finding ledger-audit raises for a payment the
// general ledger holds no posting of.
const UnpostedFindingType = "books.entry.missing"

// UnpostedPayment is a receipt, or a settled reversal of one, that the
// general ledger holds no posting of: its writer stored it past the poster.
type UnpostedPayment struct {
	PaymentID uuid.UUID `json:"payment_id"`
	// Kind is receipt, refund, chargeback or dispute_reversal.
	Kind     string `json:"kind"`
	Currency string `json:"currency"`
	Amount   int64  `json:"amount"`
}

func (u UnpostedPayment) String() string {
	return fmt.Sprintf("unposted %s: payment=%s amount=%s", u.Kind, u.PaymentID, moneyutil.FormatAmount(u.Amount, u.Currency))
}

// CheckPostings returns the merchant's receipts and settled reversals with
// no general-ledger posting, stored since the books started (a payment from
// before them waits for their rebuild). An empty slice is the healthy
// answer.
func CheckPostings(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]UnpostedPayment, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("books: merchant is required")
	}
	rows, err := gen.New(q).ListUnpostedPayments(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("books: posting check: %w", err)
	}
	var out []UnpostedPayment
	for _, r := range rows {
		if CheckCurrency(r.Currency) != nil {
			continue
		}
		kind := "receipt"
		if r.ReversalKind != nil {
			kind = *r.ReversalKind
		}
		out = append(out, UnpostedPayment{PaymentID: r.ID, Kind: kind, Currency: r.Currency, Amount: r.Amount})
	}
	return out, nil
}

// RaiseUnposted raises a finding for each payment the general ledger holds
// no posting of. The gap is recorded, never refused.
func RaiseUnposted(ctx context.Context, q *gen.Queries, merchant uuid.UUID, unposted []UnpostedPayment) error {
	for _, u := range unposted {
		evidence, err := json.Marshal(map[string]string{"payment_id": u.PaymentID.String(), "kind": u.Kind, "currency": u.Currency, "amount": strconv.FormatInt(u.Amount, 10)})
		if err != nil {
			return err
		}
		action := "The payment was stored without its general-ledger posting. Find the writer that skipped the poster; the books' rebuild posts the payment."
		if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: merchant, FindingType: UnpostedFindingType,
			SubjectKey: "payment:" + u.PaymentID.String(), Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: evidence}); err != nil {
			return fmt.Errorf("books: raise %s: %w", UnpostedFindingType, err)
		}
	}
	return nil
}
