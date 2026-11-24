package books

// A customer's usage receivable, shared across every source of owed: usage
// postings, running charges and failed usage accrue it (AccrueUsage); credit
// repays it at face (RepayFromCredit); a card, a remittance or an observed
// provider payment collects it (CollectUsage); a waiver writes it off
// (WaiveUsage); an uncollectible usage order carries an allowance against it
// (SettleAllowance); a chargeback of a collection reopens it (ReopenUsage)
// and its won dispute collects it again (ResettleUsage). Owed that accrued
// before the books has no receivable: what collects it is revenue, and what
// waives or reserves it posts nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db/gen"
)

// The operations of the receivable's movements.
const (
	// OpUsageCollection applies a usage order payment's cash to the
	// receivable, and OpUsageCollectionRevenue what it paid beyond it to
	// usage revenue, both at (_, payment, id).
	OpUsageCollection        Operation = "usage_collection"
	OpUsageCollectionRevenue Operation = "usage_collection_revenue"
	// OpUsageWaiver writes a voided usage order's due off at (_, order, id).
	OpUsageWaiver Operation = "usage_waiver"
	// OpAllowance moves an uncollectible usage order's allowance at
	// (allowance, order:<id>, n).
	OpAllowance Operation = "allowance"
	// OpDisputeWonRevenue is a usage order payment's won dispute settling
	// owed from before the books again, as usage revenue; the owed it
	// collects from the receivable is at OpDisputeWonReopened, the rest at
	// OpDisputeWon.
	OpDisputeWonRevenue Operation = "dispute_won_revenue"
	// OpDisputeWonLoss gives back what a usage order payment's chargeback
	// lost to chargebacks.
	OpDisputeWonLoss Operation = "dispute_won_loss"
)

// Receivable is what the general ledger holds receivable from a customer in
// a currency, its account locked against concurrent caps.
func (b *Books) Receivable(ctx context.Context, customer uuid.UUID, currency string) (int64, error) {
	r, err := b.q.LockUsageReceivable(ctx, gen.LockUsageReceivableParams{MerchantID: b.merchant, CustomerID: customer, Currency: currency})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("books: read the receivable: %w", err)
	}
	return r, nil
}

// UsageCollection is a usage order payment's cash that settles owed.
type UsageCollection struct {
	Payment, Customer uuid.UUID
	Order             *uuid.UUID
	Currency          string
	// Amount is the owed it settles, no more than the payment received.
	Amount int64
	At     time.Time
}

// CollectUsage applies a usage order payment's cash, which Receive booked
// to unapplied cash, to what it settles, never beyond what the payment left
// unapplied: as much as the customer's receivable holds, usage_collected DR
// unapplied_cash / CR usage_receivable; the rest settles owed from before the
// books and is usage revenue. Both at the payment's own coordinate (source
// payment), which the reversal poster counts as applied.
func (b *Books) CollectUsage(ctx context.Context, c UsageCollection) error {
	if c.Amount <= 0 || CheckCurrency(c.Currency) != nil {
		return nil
	}
	cash, err := b.q.GetPaymentCashApplication(ctx, gen.GetPaymentCashApplicationParams{MerchantID: b.merchant, PaymentID: c.Payment.String()})
	if err != nil {
		return err
	}
	if c.Amount = min(c.Amount, cash.Received-cash.Applied); c.Amount <= 0 {
		return nil
	}
	receivable, err := b.Receivable(ctx, c.Customer, c.Currency)
	if err != nil {
		return err
	}
	collected := min(max(receivable, 0), c.Amount)
	for _, leg := range []struct {
		credit Ref
		op     Operation
		amount int64
	}{
		{Ref{Type: UsageReceivable, Customer: &c.Customer}, OpUsageCollection, collected},
		{Ref{Type: UsageRevenue}, OpUsageCollectionRevenue, c.Amount - collected},
	} {
		if leg.amount <= 0 {
			continue
		}
		if _, _, err := b.Post(ctx, Posting{
			Debit: Ref{Type: UnappliedCash}, Credit: leg.credit, Amount: leg.amount, Currency: c.Currency, Type: UsageCollected,
			Coord: Coord{Operation: leg.op, Source: "payment", SourceID: c.Payment.String()}, EffectiveAt: c.At, Customer: &c.Customer, Order: c.Order,
		}); err != nil {
			return err
		}
	}
	return nil
}

// WaiveUsage writes off a voided usage order's due: its allowance first
// (usage_waived DR allowance_doubtful), the rest to revenue_adjustments, CR
// usage_receivable, all within what the receivable holds.
func (b *Books) WaiveUsage(ctx context.Context, order gen.BillingOrder, due int64, at time.Time) error {
	if due <= 0 || CheckCurrency(order.Currency) != nil {
		return nil
	}
	receivable, err := b.Receivable(ctx, order.CustomerID, order.Currency)
	if err != nil {
		return err
	}
	allowance, err := b.q.GetUsageAllowance(ctx, gen.GetUsageAllowanceParams{MerchantID: b.merchant, Source: allowanceSource(order.ID)})
	if err != nil {
		return err
	}
	receivable = max(receivable, 0)
	writeOff := min(max(allowance.Allowance, 0), due, receivable)
	if err := b.moveAllowance(ctx, order, Ref{Type: AllowanceDoubtful}, receivableOf(order), writeOff, UsageWaived, allowance.Legs, at); err != nil {
		return err
	}
	if waived := min(due-writeOff, receivable-writeOff); waived > 0 {
		_, _, err = b.Post(ctx, Posting{
			Debit: Ref{Type: RevenueAdjustments}, Credit: receivableOf(order), Amount: waived, Currency: order.Currency, Type: UsageWaived,
			Coord: Coord{Operation: OpUsageWaiver, Source: "order", SourceID: order.ID.String()}, EffectiveAt: at, Customer: &order.CustomerID, Order: &order.ID,
		})
	}
	return err
}

// SettleAllowance brings a usage order's allowance for doubtful accounts to
// what its state calls for, after any change to what it has due or whether
// it is open: an uncollectible order reserves its due, within what the
// receivable holds beyond the customer's other allowances (bad_debt DR
// bad_debt / CR allowance_doubtful), and keeps no more than it still has
// due; any other order none (bad_debt_recovery back). A voided order's
// allowance is its waiver's to write off.
func (b *Books) SettleAllowance(ctx context.Context, orderID uuid.UUID, at time.Time) error {
	order, err := b.q.GetOrder(ctx, gen.GetOrderParams{MerchantID: b.merchant, ID: orderID})
	if err != nil {
		return err
	}
	if order.Origin != "usage" || order.Status == "void" || CheckCurrency(order.Currency) != nil {
		return nil
	}
	current, err := b.q.GetUsageAllowance(ctx, gen.GetUsageAllowanceParams{MerchantID: b.merchant, Source: allowanceSource(order.ID)})
	if err != nil {
		return err
	}
	due := int64(0)
	if order.AmountPaid != nil {
		due = max(order.Total-*order.AmountPaid, 0)
	}
	target := int64(0)
	if order.Status == "uncollectible" {
		target = min(current.Allowance, due)
		if current.Allowance <= 0 {
			receivable, err := b.Receivable(ctx, order.CustomerID, order.Currency)
			if err != nil {
				return err
			}
			others, err := b.q.GetCustomerUsageAllowance(ctx, gen.GetCustomerUsageAllowanceParams{MerchantID: b.merchant, CustomerID: order.CustomerID, Currency: order.Currency})
			if err != nil {
				return err
			}
			target = min(due, max(receivable-others, 0))
		}
	}
	switch delta := target - current.Allowance; {
	case delta > 0:
		return b.moveAllowance(ctx, order, Ref{Type: BadDebt}, Ref{Type: AllowanceDoubtful}, delta, BadDebtWrittenOff, current.Legs, at)
	case delta < 0:
		return b.moveAllowance(ctx, order, Ref{Type: AllowanceDoubtful}, Ref{Type: BadDebt}, -delta, BadDebtRecovered, current.Legs, at)
	}
	return nil
}

// moveAllowance posts one leg of an order's allowance, the order's legs-th.
func (b *Books) moveAllowance(ctx context.Context, order gen.BillingOrder, debit, credit Ref, amount int64, typ TransferType, legs int64, at time.Time) error {
	if amount <= 0 {
		return nil
	}
	_, _, err := b.Post(ctx, Posting{
		Debit: debit, Credit: credit, Amount: amount, Currency: order.Currency, Type: typ,
		Coord:       Coord{Operation: OpAllowance, Source: allowanceSource(order.ID), SourceID: strconv.FormatInt(legs+1, 10)},
		EffectiveAt: at, Customer: &order.CustomerID, Order: &order.ID,
	})
	return err
}

func allowanceSource(order uuid.UUID) string { return "order:" + order.String() }

func receivableOf(order gen.BillingOrder) Ref {
	customer := order.CustomerID
	return Ref{Type: UsageReceivable, Customer: &customer}
}

// UsageReversal is a chargeback of a usage order payment, or its won
// dispute's recovery, and what the units book made of it.
type UsageReversal struct {
	// Reversal is the reversal's payment row; Original the payment it
	// reverses.
	Reversal, Original gen.BillingPayment
	// Owed is the owed it reopened (a chargeback) or settled again (a
	// recovery).
	Owed int64
	// Unapplied is what of the payment's cash unapplied cash still held
	// before a chargeback.
	Unapplied int64
	// Loss is what the payment's chargebacks lost to chargebacks that no
	// recovery has given back yet, before a recovery.
	Loss int64
}

// ReopenUsage posts a chargeback of a usage order payment, all at
// (chargeback, payment, its id), and returns its loss: the owed it reopened
// is receivable again, usage_reopened DR usage_receivable / CR the payment's
// cash account; the rest of the cash leaves what the payment still holds in
// unapplied cash (cash_returned), and beyond that is a loss to chargebacks
// (chargeback_recognized): cash a credit lot already took, such as the
// change of a card's rounding.
func (b *Books) ReopenUsage(ctx context.Context, r UsageReversal) (int64, error) {
	amount := -r.Reversal.Amount
	if amount <= 0 || CheckCurrency(r.Reversal.Currency) != nil {
		return 0, nil
	}
	customer := r.Reversal.CustomerID
	reopened := min(r.Owed, amount)
	returned := min(amount-reopened, max(r.Unapplied, 0))
	loss := amount - reopened - returned
	cash := CashAccount(r.Original)
	return loss, b.reversalLegs(ctx, r.Reversal, []reversalLeg{
		{Ref{Type: UsageReceivable, Customer: &customer}, cash, reopened, UsageReopened, OpChargeback},
		{Ref{Type: UnappliedCash}, cash, returned, CashReturned, OpChargeback},
		{Ref{Type: Chargebacks}, cash, loss, ChargebackRecognized, OpChargeback},
	})
}

// ResettleUsage posts a won dispute's recovery of a usage order payment and
// returns what it gave back of the chargebacks' loss and what it brought
// beyond: the owed it settled again leaves the receivable as far as it holds
// it (dispute_won DR the cash account / CR usage_receivable, at
// dispute_won_reopened), owed from before the books is usage revenue (at
// dispute_won_revenue), the chargebacks' loss comes back from chargebacks (at
// dispute_won_loss), and the rest is unapplied cash (at dispute_won), the
// customer's, for the caller to credit.
func (b *Books) ResettleUsage(ctx context.Context, r UsageReversal) (reversed, excess int64, err error) {
	amount := r.Reversal.Amount
	if amount <= 0 || CheckCurrency(r.Reversal.Currency) != nil {
		return 0, 0, nil
	}
	customer := r.Reversal.CustomerID
	receivable, err := b.Receivable(ctx, customer, r.Reversal.Currency)
	if err != nil {
		return 0, 0, err
	}
	owed := min(r.Owed, amount)
	settled := min(owed, max(receivable, 0))
	reversed = min(amount-owed, max(r.Loss, 0))
	excess = amount - owed - reversed
	cash := CashAccount(r.Original)
	return reversed, excess, b.reversalLegs(ctx, r.Reversal, []reversalLeg{
		{cash, Ref{Type: UsageReceivable, Customer: &customer}, settled, DisputeWon, OpDisputeWonReopened},
		{cash, Ref{Type: UsageRevenue}, owed - settled, DisputeWon, OpDisputeWonRevenue},
		{cash, Ref{Type: Chargebacks}, reversed, DisputeWon, OpDisputeWonLoss},
		{cash, Ref{Type: UnappliedCash}, excess, DisputeWon, OpDisputeWon},
	})
}

// CustomerCreditFindingType is the finding a won dispute's credit to its
// customer raises: the merchant may refund it instead.
const CustomerCreditFindingType = "books.dispute.customer_credit"

// RaiseCustomerCredit records that a won dispute of a usage order payment
// brought back more than the customer still owed, credited to them as a credit lot.
func (b *Books) RaiseCustomerCredit(ctx context.Context, recovery gen.BillingPayment, lot uuid.UUID, amount int64) error {
	raw, err := json.Marshal(map[string]any{"payment_id": recovery.ID, "refunded_payment_id": recovery.RefundedPaymentID, "credit_grant_id": lot,
		"amount": amount, "currency": recovery.Currency})
	if err != nil {
		return err
	}
	action := "A won dispute returned more of a usage order payment than the customer still owed. The rest is credit on their balance; refund it to the customer if they should have the money back instead."
	if _, err := b.q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: b.merchant, FindingType: CustomerCreditFindingType, SubjectKey: "payment:" + recovery.ID.String(),
		Severity: "low", Status: "requires_review", RecommendedAction: &action, Evidence: raw,
	}); err != nil {
		return fmt.Errorf("books: raise %s: %w", CustomerCreditFindingType, err)
	}
	return nil
}

type reversalLeg struct {
	debit, credit Ref
	amount        int64
	typ           TransferType
	op            Operation
}

func (b *Books) reversalLegs(ctx context.Context, reversal gen.BillingPayment, legs []reversalLeg) error {
	for _, leg := range legs {
		if leg.amount <= 0 {
			continue
		}
		if _, _, err := b.Post(ctx, Posting{
			Debit: leg.debit, Credit: leg.credit, Amount: leg.amount, Currency: reversal.Currency, Type: leg.typ,
			Coord:       Coord{Operation: leg.op, Source: "payment", SourceID: reversal.ID.String()},
			EffectiveAt: reversal.PurchasedAt, Customer: &reversal.CustomerID, Order: reversal.OrderID,
		}); err != nil {
			return err
		}
	}
	return nil
}
