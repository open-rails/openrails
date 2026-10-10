package money

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// ErrIdempotencyKeyReused refuses a key replayed with different charging terms.
//
// A money write's idempotency key is (Operation, Source, SourceID) within
// (merchant, customer, currency), and in this package:
//
//   - every part is required: a missing key is refused, never skipped;
//   - the operation is engine-composed, so two operations sharing a caller's
//     (source, source_id) never alias at the ledger;
//   - a key is a claim about the body: a retry with different terms gets this
//     error, never the first result.
//
// The key must be reproducible across retries of one logical operation; one
// minted per attempt (uuid.New() in a handler) guarantees nothing.
var ErrIdempotencyKeyReused = errors.New("idempotency_key_reused")

// Operation is the engine-composed kind of money write; see ledger.Operation.
type Operation = ledger.Operation

// The operation vocabulary, re-exported so callers never reach into the ledger
// package to name the kind of write they are making.
const (
	OpSpend    = ledger.OpSpend
	OpCapture  = ledger.OpCapture
	OpWithdraw = ledger.OpWithdraw
	OpDeposit  = ledger.OpDeposit

	OpCreditExpire    = ledger.OpCreditExpire
	OpCreditRevoke    = ledger.OpCreditRevoke
	OpCreditReinstate = ledger.OpCreditReinstate

	OpArrearsAccrual   = ledger.OpArrearsAccrual
	OpMeteredRating    = ledger.OpMeteredRating
	OpInvoicePayment   = ledger.OpInvoicePayment
	OpManualInvoicePay = ledger.OpManualInvoicePay
	OpInvoiceVoid      = ledger.OpInvoiceVoid
)

// UsageOperation is the operation kind of a metered usage charge (usage:<event_type>).
func UsageOperation(eventType string) Operation { return ledger.UsageOperation(eventType) }

// IdempotencyKey is the coordinate a durable money write is idempotent on. The
// zero value is unusable; NewIdempotencyKey, which refuses a blank part,
// builds one.
type IdempotencyKey struct {
	coord ledger.Coord
}

// NewIdempotencyKey builds the coordinate for one money write. op is supplied
// by the ENGINE at the call site making the write, never by an external caller;
// source and sourceID are the caller's reproducible key.
func NewIdempotencyKey(op Operation, source, sourceID string) (IdempotencyKey, error) {
	c := ledger.Coord{
		Operation: Operation(strings.TrimSpace(string(op))),
		Source:    strings.TrimSpace(source),
		SourceID:  strings.TrimSpace(sourceID),
	}
	if err := c.Validate(); err != nil {
		return IdempotencyKey{}, err
	}
	return IdempotencyKey{coord: c}, nil
}

// MustIdempotencyKey is NewIdempotencyKey for engine-internal coordinates whose
// parts are structurally non-empty (a uuid, a literal). It panics on a blank
// part, which would be a programming error, not a caller error.
func MustIdempotencyKey(op Operation, source, sourceID string) IdempotencyKey {
	k, err := NewIdempotencyKey(op, source, sourceID)
	if err != nil {
		panic(err)
	}
	return k
}

func (k IdempotencyKey) IsZero() bool         { return k.coord.Operation == "" }
func (k IdempotencyKey) Operation() Operation { return k.coord.Operation }
func (k IdempotencyKey) Source() string       { return k.coord.Source }
func (k IdempotencyKey) SourceID() string     { return k.coord.SourceID }
func (k IdempotencyKey) Coord() ledger.Coord  { return k.coord }
func (k IdempotencyKey) String() string       { return k.coord.String() }

// RequireOperation rejects a zero key and a key built for a DIFFERENT operation than
// the write about to be made. Mis-pairing is loud rather than a silent write at
// someone else's coordinate.
func (k IdempotencyKey) RequireOperation(op Operation) error {
	if k.IsZero() {
		return fmt.Errorf("%s: idempotency key required (build one with money.NewIdempotencyKey)", op)
	}
	if k.coord.Operation != op {
		return fmt.Errorf("%s: idempotency key carries operation %q", op, k.coord.Operation)
	}
	return k.coord.Validate()
}

// IdempotencyConflict reports a reused key whose retry carries different
// charging terms than the write that key already committed.
type IdempotencyConflict struct {
	Operation string // the money write that refused: "spend", "capture", "withdraw", "usage:<event_type>"
	Source    string
	SourceID  string
	Field     string // the charging term that differs
	Committed any    // what the key already committed
	Retried   any    // what this retry asked for
}

func (e *IdempotencyConflict) Error() string {
	return fmt.Sprintf(
		"%s: %s replayed idempotency key (%s, %s) with %s=%v, but that key already committed %s=%v",
		ErrIdempotencyKeyReused, e.Operation, e.Source, e.SourceID, e.Field, e.Retried, e.Field, e.Committed,
	)
}

func (e *IdempotencyConflict) Unwrap() error { return ErrIdempotencyKeyReused }

// committedAt reports the total already posted at this key's coordinate and
// how many legs carry it. A spend fans out one credit_spend per FIFO lot plus
// at most one owed_accrual, so only the sum is the operation's amount.
func (k IdempotencyKey) committedAt(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string) (total int64, legs int64, err error) {
	row, err := q.SumLedgerSpendByCoords(ctx, gen.SumLedgerSpendByCoordsParams{
		MerchantID: merchantID, CustomerID: customerID, Currency: currency,
		Operation: string(k.coord.Operation), Source: k.coord.Source, SourceID: k.coord.SourceID,
	})
	if err != nil {
		return 0, 0, err
	}
	return row.Total, row.Transfers, nil
}

// requireSameAmount refuses a replay whose amount differs from what this key
// already committed, and reports whether the key was already committed at all.
func (k IdempotencyKey) requireSameAmount(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, currency string, amount int64) (committed bool, err error) {
	total, legs, err := k.committedAt(ctx, q, merchantID, customerID, currency)
	if err != nil {
		return false, err
	}
	if legs == 0 {
		return false, nil
	}
	if total != amount {
		return true, &IdempotencyConflict{
			Operation: string(k.coord.Operation), Source: k.coord.Source, SourceID: k.coord.SourceID,
			Field: "amount", Committed: total, Retried: amount,
		}
	}
	return true, nil
}

// invoiceItemSourceID is the pending-invoice-item key for this coordinate.
// invoice_items_customer_id_currency_source_type_source_id_key is (merchant, customer, currency, source_type,
// source_id), so the OPERATION has to be in the string for the same reason it
// is in the ledger coordinate — otherwise a capture's owed leg and a usage
// charge's owed leg at one (source, source_id) collide into one accrued item.
func (k IdempotencyKey) invoiceItemSourceID() string {
	return string(k.coord.Operation) + ":" + k.coord.Source + ":" + k.coord.SourceID
}
