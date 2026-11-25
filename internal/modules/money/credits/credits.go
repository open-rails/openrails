// Package credits keeps the credit lots on customers' balances. A lot is a
// credit_grants row; every movement of its units is a units-book transfer
// naming it, and the ledger counts the legs on its balance account, so
// remaining is units_credited - units_debited. A paid lot's money moves with
// its units in the general ledger (books.MoveCredit). Revocations and
// reinstatements are credit_grant_events.
package credits

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// FundingSource is where a lot's units came from.
type FundingSource string

const (
	// Purchase was paid by a one-off invoice line or a charge's change.
	Purchase FundingSource = "purchase"
	// Recurring was paid by a cycle of a recurring price.
	Recurring FundingSource = "recurring"
	// Grant and Promotion were given by the merchant.
	Grant     FundingSource = "grant"
	Promotion FundingSource = "promotion"
	// Import came from another system and may declare what was paid.
	Import FundingSource = "import"
)

// Paid reports whether lots of the source were bought.
func (f FundingSource) Paid() bool { return f == Purchase || f == Recurring }

// Valid reports whether f is a funding source.
func (f FundingSource) Valid() bool {
	switch f {
	case Purchase, Recurring, Grant, Promotion, Import:
		return true
	}
	return false
}

// ErrInsufficientCredits is a spend the customer's live lots cannot cover.
var ErrInsufficientCredits = errors.New("credits: insufficient credits")

// Lots is one merchant's credit lots over a query handle. Compose it inside
// the caller's transaction so a lot and its transfers commit together.
type Lots struct {
	q        *gen.Queries
	merchant uuid.UUID
	money    *ledger.Ledger
	now      func() time.Time
	// liveAt, when set, is the instant Spend's lots must not have lapsed by,
	// in place of now.
	liveAt *time.Time
	books  *books.Books
}

// New binds a merchant's lots to a query handle and the caller's clock.
func New(q *gen.Queries, merchant uuid.UUID, now func() time.Time) *Lots {
	clock := func() time.Time { return now().UTC().Truncate(time.Microsecond) }
	return &Lots{q: q, merchant: merchant, money: ledger.New(q, merchant).SetClock(clock), now: clock, books: books.New(q, merchant)}
}

// DrawLiveAt makes Spend draw the lots that had not lapsed by t in place of
// now: a running charge's posting through the instant after t. nil restores
// now.
func (l *Lots) DrawLiveAt(t *time.Time) { l.liveAt = t }

// Issue is a new lot.
type Issue struct {
	Customer uuid.UUID
	Unit     string
	Units    int64
	Funding  FundingSource
	// PaidAmount and PaidCurrency are what was paid for the lot; both nil
	// for a gift.
	PaidAmount   *int64
	PaidCurrency *string
	// Source and SourceID are the funding's label and once-only key ("" for
	// none); Invoker is who funded it.
	Source, SourceID, Invoker     string
	Product, InvoiceLine, Payment *uuid.UUID
	Description                   *string
	// StartsAt zero is now; ExpiresAt nil never lapses.
	StartsAt  time.Time
	ExpiresAt *time.Time
}

func (in Issue) validate() error {
	switch {
	case in.Customer == uuid.Nil || in.Units <= 0 || in.Unit == "":
		return fmt.Errorf("credits: a lot needs its customer, unit and positive units")
	case !in.Funding.Valid():
		return fmt.Errorf("credits: unknown funding source %q", in.Funding)
	case (in.PaidAmount == nil) != (in.PaidCurrency == nil):
		return fmt.Errorf("credits: a paid amount needs its currency")
	case in.PaidAmount != nil && *in.PaidAmount <= 0:
		return fmt.Errorf("credits: the paid amount must be positive")
	case in.Funding != Import && in.Funding.Paid() != (in.PaidAmount != nil):
		return fmt.Errorf("credits: a %s lot is paid only when it was bought", in.Funding)
	case in.Source == "" || in.Invoker == "":
		return fmt.Errorf("credits: a lot needs its source and invoker")
	}
	return nil
}

// Issue appends a lot and posts its deposit.
func (l *Lots) Issue(ctx context.Context, in Issue) (gen.BillingCreditGrant, error) {
	if err := in.validate(); err != nil {
		return gen.BillingCreditGrant{}, err
	}
	starts := in.StartsAt
	if starts.IsZero() {
		starts = l.now()
	}
	account, err := l.money.EnsureCustomerBalance(ctx, in.Customer, in.Unit)
	if err != nil {
		return gen.BillingCreditGrant{}, err
	}
	var sourceID *string
	if in.SourceID != "" {
		sourceID = &in.SourceID
	}
	if in.Description != nil && *in.Description == "" {
		in.Description = nil
	}
	lot, err := l.q.InsertCreditGrant(ctx, gen.InsertCreditGrantParams{
		MerchantID: l.merchant, CustomerID: in.Customer, AccountID: account, Unit: in.Unit, Units: in.Units,
		FundingSource: string(in.Funding), PaidAmount: in.PaidAmount, PaidCurrency: in.PaidCurrency,
		Source: in.Source, SourceID: sourceID, Invoker: in.Invoker, ProductID: in.Product, InvoiceLineID: in.InvoiceLine,
		PaymentID: in.Payment, Description: in.Description, StartsAt: starts, ExpiresAt: in.ExpiresAt,
	})
	if err != nil {
		return lot, fmt.Errorf("credits: record the lot: %w", err)
	}
	if err := l.Materialize(ctx, lot.ID); err != nil {
		return lot, err
	}
	return l.Get(ctx, lot.ID)
}

// Get reads a lot.
func (l *Lots) Get(ctx context.Context, id uuid.UUID) (gen.BillingCreditGrant, error) {
	return l.q.GetCreditLot(ctx, gen.GetCreditLotParams{MerchantID: l.merchant, ID: id})
}

// Remaining is a lot's units left on its balance.
func Remaining(lot gen.BillingCreditGrant) int64 { return lot.UnitsCredited - lot.UnitsDebited }

// Materialize posts what a lot records and its ledger lacks: the deposit of
// a lot none of whose units posted, with its price's deferral, or the
// clawback of a revoked lot's units. Idempotent; run it under the customer's
// lock.
func (l *Lots) Materialize(ctx context.Context, id uuid.UUID) error {
	lot, err := l.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("credits: load lot %s: %w", id, err)
	}
	if lot.RevokedAt != nil {
		return l.clawback(ctx, lot)
	}
	if lot.UnitsCredited > 0 {
		return nil
	}
	// The customer's usage so far draws the balance before the deposit.
	if _, err := l.settleUsage(ctx, lot.CustomerID, lot.Unit, nil); err != nil {
		return fmt.Errorf("credits: post usage before the deposit: %w", err)
	}
	// A bought lot's units were sold, a gift's granted.
	from := ledger.UnitsGranted
	if lot.PaidAmount != nil {
		from = ledger.UnitsSold
	}
	coord := ledger.Coord{Operation: ledger.OpDeposit, Source: "grant", SourceID: lot.ID.String()}
	if _, err := l.money.DepositFrom(ctx, from, lot.CustomerID, lot.Unit, lot.Units, coord, lot.ID); err != nil {
		return fmt.Errorf("credits: deposit lot %s: %w", lot.ID, err)
	}
	if err := l.deferPrice(ctx, lot); err != nil {
		return err
	}
	// A lot that lapses is expired by its balance's watch.
	if lot.ExpiresAt != nil {
		if err := ledger.WatchLapsingCredit(ctx, l.q, gen.WatchLapsingCreditParams{MerchantID: l.merchant, CustomerID: lot.CustomerID, Currency: lot.Unit, Now: l.now()}); err != nil {
			return fmt.Errorf("credits: watch the lapsing lot: %w", err)
		}
	}
	return nil
}

// deferPrice defers a paid lot's price from its payment, or an import's declared
// price from opening balances. A bought lot recorded before its payment (a
// charge's change) is deferred by Fund once the payment is.
func (l *Lots) deferPrice(ctx context.Context, lot gen.BillingCreditGrant) error {
	if lot.PaidAmount == nil {
		return nil
	}
	var payment *gen.BillingPayment
	if lot.PaymentID != nil {
		p, err := l.q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: l.merchant, ID: *lot.PaymentID})
		if err != nil {
			return fmt.Errorf("credits: load lot %s's payment: %w", lot.ID, err)
		}
		payment = &p
	}
	err := l.books.IssueCredit(ctx, books.CreditLotOf(lot), payment)
	if errors.Is(err, books.ErrCreditUnfunded) {
		return nil
	}
	return err
}

// Fund defers a bought lot's price from the payment recorded after it: the
// change a charge paid beyond its invoice. The commit refuses a bought lot left
// undeferred.
func (l *Lots) Fund(ctx context.Context, id uuid.UUID, payment gen.BillingPayment) error {
	lot, err := l.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("credits: load lot %s: %w", id, err)
	}
	return l.books.IssueCredit(ctx, books.CreditLotOf(lot), &payment)
}

// move posts the money of a units-book leg that moved a lot's units.
func (l *Lots) move(ctx context.Context, lot uuid.UUID, kind books.CreditMoveKind, leg gen.BillingLedgerTransfer, into *books.Line) error {
	current, err := l.Get(ctx, lot)
	if err != nil {
		return fmt.Errorf("credits: reload lot %s: %w", lot, err)
	}
	return l.books.MoveCredit(ctx, books.CreditLotOf(current), books.CreditMove{
		Kind: kind, Units: leg.Amount, At: leg.EffectiveAt, Into: into,
		Coord: books.Coord{Operation: books.Operation(leg.Operation), Source: leg.Source, SourceID: leg.SourceID},
	})
}

// Revoke records the lot's revocation, effective asOf (zero is now), and
// moves its units left to units_revoked and their money to breakage: frozen
// and reversible, not refunded. What the customer's running charges accrued
// draws the lot first. actor is who revoked it, "" for none.
func (l *Lots) Revoke(ctx context.Context, id uuid.UUID, reason, actor string, asOf time.Time) (gen.BillingCreditGrant, error) {
	lot, err := l.Get(ctx, id)
	if err != nil {
		return lot, fmt.Errorf("credits: load lot %s: %w", id, err)
	}
	if _, err := l.SettleRunningCharges(ctx, lot.CustomerID, lot.Unit, l.now()); err != nil {
		return lot, err
	}
	if _, err := l.event(ctx, id, "revoke", reason, actor, "", asOf); err != nil {
		return lot, err
	}
	if err := l.Materialize(ctx, id); err != nil {
		return lot, err
	}
	return l.Get(ctx, id)
}

// ErrLapsed refuses reinstating a lot past its expiry: it would only expire
// again.
var ErrLapsed = errors.New("credits: the lot has lapsed")

// Reinstate reverses a lot's revocation: the units it froze return to the
// balance from units_revoked (credit_reinstate) and their money from breakage
// to deferral. A lapsed lot is refused (ErrLapsed). actor is who reinstated
// it, "" for none; key the caller's idempotency key, "" for none.
func (l *Lots) Reinstate(ctx context.Context, id uuid.UUID, reason, actor, key string) (gen.BillingCreditGrant, error) {
	lot, err := l.Get(ctx, id)
	if err != nil {
		return lot, fmt.Errorf("credits: load lot %s: %w", id, err)
	}
	if lot.ExpiresAt != nil && !lot.ExpiresAt.After(l.now()) {
		return lot, ErrLapsed
	}
	event, err := l.event(ctx, id, "reinstate", reason, actor, key, time.Time{})
	if err != nil {
		return lot, err
	}
	frozen, err := l.q.GetPurchasedCreditRetiredBalance(ctx, gen.GetPurchasedCreditRetiredBalanceParams{MerchantID: l.merchant, CreditGrantID: id})
	if err != nil {
		return lot, fmt.Errorf("credits: read lot %s's revoked units: %w", id, err)
	}
	if frozen.Revoked > 0 {
		revoked, err := l.money.EnsureSystemAccount(ctx, ledger.UnitsRevoked, lot.Unit)
		if err != nil {
			return lot, err
		}
		lotID, customer := lot.ID, lot.CustomerID
		leg, err := l.money.Apply(ctx, ledger.Transfer{
			Debit: revoked, Credit: lot.AccountID, Amount: frozen.Revoked, Currency: lot.Unit, Type: ledger.CreditReinstate,
			Coord:         ledger.Coord{Operation: ledger.OpCreditReinstate, Source: "grant_reinstate", SourceID: event.ID.String()},
			CreditGrantID: &lotID, Customer: &customer,
		})
		if err != nil {
			return lot, fmt.Errorf("credits: reinstate lot %s: %w", id, err)
		}
		if err := l.move(ctx, id, books.CreditReinstated, leg, nil); err != nil {
			return lot, err
		}
	}
	return l.Get(ctx, id)
}

// event appends a revocation or reinstatement, effective at (zero is now),
// under the caller's key ("" for none).
func (l *Lots) event(ctx context.Context, id uuid.UUID, kind, reason, actor, key string, at time.Time) (gen.BillingCreditGrantEvent, error) {
	if at.IsZero() {
		at = l.now()
	}
	params := gen.InsertCreditGrantEventParams{MerchantID: l.merchant, CreditGrantID: id, Event: kind, EffectiveAt: at}
	if reason != "" {
		params.Reason = &reason
	}
	if actor != "" {
		params.Actor = &actor
	}
	if key != "" {
		params.RequestKey = &key
	}
	event, err := l.q.InsertCreditGrantEvent(ctx, params)
	if err != nil {
		return event, fmt.Errorf("credits: %s lot %s: %w", kind, id, err)
	}
	return event, nil
}

// clawback moves a revoked lot's units left DR its balance / CR
// units_revoked, once per revocation at (credit_revoke, grant_revoke, the
// revocation), and their money to breakage.
func (l *Lots) clawback(ctx context.Context, lot gen.BillingCreditGrant) error {
	remaining := Remaining(lot)
	if remaining <= 0 {
		return nil
	}
	revocation, err := l.q.GetLatestCreditGrantEvent(ctx, gen.GetLatestCreditGrantEventParams{MerchantID: l.merchant, CreditGrantID: lot.ID})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("credits: read lot %s's revocation: %w", lot.ID, err)
	}
	// A revocation from before the events names its lot.
	source := lot.ID.String()
	if err == nil && revocation.Event == "revoke" {
		source = revocation.ID.String()
	}
	revoked, err := l.money.EnsureSystemAccount(ctx, ledger.UnitsRevoked, lot.Unit)
	if err != nil {
		return err
	}
	id, customer := lot.ID, lot.CustomerID
	leg, err := l.money.Apply(ctx, ledger.Transfer{
		Debit: lot.AccountID, Credit: revoked, Amount: remaining, Currency: lot.Unit, Type: ledger.CreditRevoke,
		Coord:         ledger.Coord{Operation: ledger.OpCreditRevoke, Source: "grant_revoke", SourceID: source},
		CreditGrantID: &id, Customer: &customer,
	})
	if err != nil {
		return err
	}
	return l.move(ctx, lot.ID, books.CreditRevoked, leg, nil)
}

// SyncReservation writes what the pending refunds of payment reserve of the
// lot it bought, if any: after a refund of it is reserved, released or
// settled, in that transaction.
func (l *Lots) SyncReservation(ctx context.Context, payment uuid.UUID) error {
	return l.q.SyncCreditGrantReservation(ctx, gen.SyncCreditGrantReservationParams{MerchantID: l.merchant, PaymentID: payment})
}

// MissingDeposits are lots none of whose units posted
// (derive.grant_effect.missing); customer nil reads the whole merchant.
// Repair = Materialize.
func (l *Lots) MissingDeposits(ctx context.Context, customer *uuid.UUID, limit int32) ([]gen.BillingCreditGrant, error) {
	return l.q.ListCreditLotsMissingDeposit(ctx, gen.ListCreditLotsMissingDepositParams{MerchantID: l.merchant, CustomerID: customer, RowLimit: limit})
}

// UnretractedRevocations are revoked lots with units still on the balance
// (derive.grant_effect.excess). Repair = Materialize.
func (l *Lots) UnretractedRevocations(ctx context.Context, customer *uuid.UUID, limit int32) ([]gen.BillingCreditGrant, error) {
	return l.q.ListRevokedCreditLotsWithUnits(ctx, gen.ListRevokedCreditLotsWithUnitsParams{MerchantID: l.merchant, CustomerID: customer, RowLimit: limit})
}

// ReservationDrifts are purchased lots whose reservation disagrees with their
// payment's pending refunds (derive.credit_reservation.mismatch). Repair =
// SyncReservation.
func (l *Lots) ReservationDrifts(ctx context.Context, customer *uuid.UUID, limit int32) ([]gen.BillingCreditGrant, error) {
	return l.q.ListCreditReservationDrifts(ctx, gen.ListCreditReservationDriftsParams{MerchantID: l.merchant, CustomerID: customer, RowLimit: limit})
}
