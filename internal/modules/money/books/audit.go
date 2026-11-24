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
// schedule: those that fail the books, then the warnings. A line priced in a
// merchant's own unit has a schedule for each currency its lots' money
// released into it; one with a release and no schedule fails, one paid only
// with given units has nothing to schedule.
func CheckSchedules(ctx context.Context, q gen.DBTX, merchant uuid.UUID) (failing, warnings []UnscheduledLine, err error) {
	if merchant == uuid.Nil {
		return nil, nil, fmt.Errorf("books: merchant is required")
	}
	rows, err := gen.New(q).ListUnscheduledOrderLines(ctx, merchant)
	if err != nil {
		return nil, nil, fmt.Errorf("books: schedule check: %w", err)
	}
	// An order whose receipts have nothing left unapplied, its money
	// applied or returned (a refund before a timed line's access), owes its
	// lines nothing more.
	spent := map[uuid.UUID]bool{}
	for _, r := range rows {
		// A unit line is checked by its releases, below.
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
			done, known := spent[r.OrderID]
			if !known {
				if done, err = orderSpent(ctx, gen.New(q), merchant, r.OrderID); err != nil {
					return nil, nil, err
				}
				spent[r.OrderID] = done
			}
			if !done {
				failing = append(failing, line)
			}
			continue
		}
		warnings = append(warnings, line)
	}
	released, err := gen.New(q).ListUnscheduledCreditReleases(ctx, merchant)
	if err != nil {
		return nil, nil, fmt.Errorf("books: schedule check: %w", err)
	}
	for _, r := range released {
		failing = append(failing, UnscheduledLine{OrderID: r.OrderID, OrderLineID: r.OrderLineID, Currency: r.Currency, Amount: r.Amount, Reason: LineUnscheduled})
	}
	return failing, warnings, nil
}

// orderSpent reports whether every receipt of an order has nothing left
// unapplied in the books: none of its money waits for a line.
func orderSpent(ctx context.Context, q *gen.Queries, merchant, order uuid.UUID) (bool, error) {
	receipts, err := q.ListOrderReceipts(ctx, gen.ListOrderReceiptsParams{MerchantID: merchant, OrderID: order})
	if err != nil {
		return false, err
	}
	if len(receipts) == 0 {
		return false, nil
	}
	for _, p := range receipts {
		pb, err := paymentBooksOf(ctx, q, merchant, p.ID)
		if err != nil {
			return false, err
		}
		if pb.received == 0 || pb.unapplied() > 0 {
			return false, nil
		}
	}
	return true, nil
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

// MisbalancedFindingType is the finding ledger-audit raises for a reversal
// whose postings moved other cash than its amount.
const MisbalancedFindingType = "books.reversal.misbalanced"

// MisbalancedReversal is a settled reversal whose general-ledger cash legs,
// its own and its credit lot's, do not sum to its amount.
type MisbalancedReversal struct {
	PaymentID uuid.UUID `json:"payment_id"`
	Kind      string    `json:"kind"`
	Currency  string    `json:"currency"`
	Amount    int64     `json:"amount"`
	Moved     int64     `json:"moved"`
}

func (m MisbalancedReversal) String() string {
	return fmt.Sprintf("misbalanced %s: payment=%s amount=%s moved=%s", m.Kind, m.PaymentID,
		moneyutil.FormatAmount(m.Amount, m.Currency), moneyutil.FormatAmount(m.Moved, m.Currency))
}

// CheckReversalCash returns the merchant's settled reversals, stored since
// the books started, whose cash legs sum to other than their amount. An empty
// slice is the healthy answer.
func CheckReversalCash(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]MisbalancedReversal, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("books: merchant is required")
	}
	rows, err := gen.New(q).ListMisbalancedReversals(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("books: reversal cash check: %w", err)
	}
	var out []MisbalancedReversal
	for _, r := range rows {
		if CheckCurrency(r.Currency) != nil {
			continue
		}
		kind := ""
		if r.ReversalKind != nil {
			kind = *r.ReversalKind
		}
		out = append(out, MisbalancedReversal{PaymentID: r.ID, Kind: kind, Currency: r.Currency, Amount: r.Amount, Moved: r.Moved})
	}
	return out, nil
}

// SyncPostingFindings raises a finding for each payment the general ledger
// holds no posting of and each reversal it posted other cash for, and closes
// those no longer reported. The gap is recorded, never refused.
func SyncPostingFindings(ctx context.Context, q *gen.Queries, merchant uuid.UUID, unposted []UnpostedPayment, misbalanced []MisbalancedReversal) error {
	raise := func(typ string, id uuid.UUID, evidence map[string]string, action string) (string, error) {
		subject := "payment:" + id.String()
		raw, err := json.Marshal(evidence)
		if err != nil {
			return "", err
		}
		if _, err := q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{MerchantID: merchant, FindingType: typ,
			SubjectKey: subject, Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: raw}); err != nil {
			return "", fmt.Errorf("books: raise %s: %w", typ, err)
		}
		return subject, nil
	}
	reported := map[string][]string{UnpostedFindingType: {}, MisbalancedFindingType: {}}
	for _, u := range unposted {
		subject, err := raise(UnpostedFindingType, u.PaymentID, map[string]string{"payment_id": u.PaymentID.String(), "kind": u.Kind, "currency": u.Currency, "amount": strconv.FormatInt(u.Amount, 10)},
			"The payment was stored without its general-ledger posting. Find the writer that skipped the poster; the books' rebuild posts the payment.")
		if err != nil {
			return err
		}
		reported[UnpostedFindingType] = append(reported[UnpostedFindingType], subject)
	}
	for _, m := range misbalanced {
		subject, err := raise(MisbalancedFindingType, m.PaymentID, map[string]string{"payment_id": m.PaymentID.String(), "kind": m.Kind, "currency": m.Currency,
			"amount": strconv.FormatInt(m.Amount, 10), "moved": strconv.FormatInt(m.Moved, 10)},
			"The reversal's general-ledger postings moved other cash than its amount. Find the poster that split it twice; the books' rebuild reposts it.")
		if err != nil {
			return err
		}
		reported[MisbalancedFindingType] = append(reported[MisbalancedFindingType], subject)
	}
	for typ, subjects := range reported {
		if _, err := q.ResolveVanishedBooksFindings(ctx, gen.ResolveVanishedBooksFindingsParams{MerchantID: merchant, FindingType: typ, Reported: subjects}); err != nil {
			return fmt.Errorf("books: close %s: %w", typ, err)
		}
	}
	return nil
}
