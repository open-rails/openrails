package books

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Overapplication is a payment whose cash the general ledger applied to bills
// beyond what it received: every payment's applications must stay within its
// receipt.
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

// UnscheduledLine is a completed bill's line that sells revenue but has no
// revenue schedule: money paid for it that recognises nothing.
type UnscheduledLine struct {
	OrderID     uuid.UUID `json:"order_id"`
	OrderLineID uuid.UUID `json:"order_line_id"`
	Currency    string    `json:"currency"`
	Amount      int64     `json:"amount"`
}

func (u UnscheduledLine) String() string {
	return fmt.Sprintf("unscheduled line: order=%s line=%s amount=%s", u.OrderID, u.OrderLineID, moneyutil.FormatAmount(u.Amount, u.Currency))
}

// CheckSchedules returns the merchant's completed revenue lines without a
// schedule. An empty slice is the healthy answer.
func CheckSchedules(ctx context.Context, q gen.DBTX, merchant uuid.UUID) ([]UnscheduledLine, error) {
	if merchant == uuid.Nil {
		return nil, fmt.Errorf("books: merchant is required")
	}
	rows, err := gen.New(q).ListUnscheduledOrderLines(ctx, merchant)
	if err != nil {
		return nil, fmt.Errorf("books: schedule check: %w", err)
	}
	var out []UnscheduledLine
	for _, r := range rows {
		out = append(out, UnscheduledLine{OrderID: r.OrderID, OrderLineID: r.OrderLineID, Currency: r.Currency, Amount: r.Amount})
	}
	return out, nil
}
