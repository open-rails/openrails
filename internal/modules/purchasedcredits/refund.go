package purchasedcredits

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// ValidateRefund reserves no value by itself. The caller binds this service to
// the transaction that inserts its pending refund payment and then writes the
// lot's reservation (credits.Lots.SyncReservation), which admission and
// spending hold.
func (s *Service) ValidateRefund(ctx context.Context, paymentID uuid.UUID, additional int64) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		grant, err := q.GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// What running charges accrued is spend the lots have funded: it is
		// posted before the refundable remainder is read.
		lots := credits.New(q, mid.UUID(), s.now)
		if _, err := lots.SettleRunningCharges(ctx, grant.CustomerID, grant.Unit, s.now()); err != nil {
			return err
		}
		balance, held, err := s.lockBalance(ctx, q, mid.UUID(), grant.CustomerID, grant.Unit)
		if err != nil {
			return err
		}
		paid, err := fundingAmount(grant)
		if err != nil {
			return err
		}
		if additional <= 0 {
			return fmt.Errorf("invalid credit refund amount: %w", billing.ErrInvalid)
		}
		returned, err := returnedCash(ctx, q, grant, paid)
		if err != nil {
			return err
		}
		pending, err := q.GetPurchasedCreditPendingRefunds(ctx, gen.GetPurchasedCreditPendingRefundsParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if err != nil {
			return err
		}
		open, err := openCreditCash(paid, returned)
		if err != nil {
			return err
		}
		// Pending refunds settle against the credit line first, as this one does.
		reservedCash := min(pending, open)
		requestedCash := min(additional, open-reservedCash)
		if requestedCash == 0 {
			return nil
		}
		lot, err := lots.Get(ctx, grant.ID)
		if err != nil {
			return err
		}
		if lot.RevokedAt != nil || (lot.ExpiresAt != nil && !lot.ExpiresAt.After(s.now())) {
			return fmt.Errorf("credit grant is no longer refundable: %w", billing.ErrConflict)
		}
		// The lot's reservation is the pending refunds' share of reservedCash.
		requested := proportional(lot.Units, requestedCash, paid, true)
		if requested > credits.Remaining(lot)-lot.UnitsReserved || requested > balance-held {
			return fmt.Errorf("refund requires unused and unreserved purchased credit: %w", billing.ErrConflict)
		}
		return nil
	})
}

// ApplyReversal posts one completed negative payment, or a positive recovery
// explicitly linked to that reversal. Keeping this identity prevents a dispute
// win from restoring credits withdrawn by an unrelated voluntary refund.
//
// A reversal returns the credit line's money first; the rest of an order's
// payment bought no credit. posted reports whether it touched the lot: one
// that did not is evaluated again by the next call.
func (s *Service) ApplyReversal(ctx context.Context, paymentID, reversalID uuid.UUID, reversesID *uuid.UUID) (posted bool, err error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		posted = false
		q := gen.New(tx)
		g, err := q.GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if g.PaidCurrency != nil && *g.PaidCurrency != g.Unit {
			posted, err = s.applyUnitReversal(ctx, q, mid.UUID(), g, reversalID)
			return err
		}
		currency := g.Unit
		if _, _, err := s.lockBalance(ctx, q, mid.UUID(), g.CustomerID, currency); err != nil {
			return err
		}
		reversal, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: reversalID})
		if err != nil {
			return err
		}
		if reversal.CustomerID != g.CustomerID || reversal.Currency != currency || reversal.RefundedPaymentID == nil || *reversal.RefundedPaymentID != paymentID || reversal.Status != "succeeded" || reversal.Amount == 0 {
			return fmt.Errorf("credit reversal contradicts completed payment")
		}
		committed, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: reversalID.String()})
		if err != nil || committed.Legs > 0 {
			return err
		}
		paid, err := fundingAmount(g)
		if err != nil {
			return err
		}
		net, err := q.GetPurchasedCreditRefundState(ctx, gen.GetPurchasedCreditRefundStateParams{MerchantID: mid.UUID(), CreditGrantID: g.ID})
		if err != nil {
			return err
		}
		restoring := reversal.Amount > 0
		// cashDelta is the credit line's part of the reversal.
		var cashDelta int64
		var origin gen.GetPurchasedCreditReversalStateRow
		var recovered int64
		if !restoring {
			if reversesID != nil || reversal.Amount == (-1<<63) {
				return fmt.Errorf("invalid negative credit reversal")
			}
			open, err := openCreditCash(paid, net.Cash)
			if err != nil {
				return err
			}
			cashDelta = min(-reversal.Amount, open)
		} else {
			if reversesID == nil {
				return fmt.Errorf("purchased credit recovery must identify the reversed payment")
			}
			original, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: *reversesID})
			if err != nil {
				return err
			}
			if original.Amount >= 0 || original.CustomerID != g.CustomerID || original.RefundedPaymentID == nil || *original.RefundedPaymentID != paymentID || original.Currency != currency {
				return fmt.Errorf("credit recovery does not reference this purchase's reversal")
			}
			if reversal.Amount > -original.Amount {
				return fmt.Errorf("credit recovery exceeds its recorded reversal")
			}
			if origin, err = q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: reversesID.String()}); err != nil {
				return err
			}
			if recovered, err = q.GetPurchasedCreditRecoveryTotal(ctx, gen.GetPurchasedCreditRecoveryTotalParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: reversesID.String()}); err != nil {
				return err
			}
			if origin.Cash < 0 || recovered < 0 || recovered > origin.Cash {
				return fmt.Errorf("credit recovery exceeds its recorded reversal")
			}
			// A recovery restores what its reversal took from the credit line
			// first: a reversal that took none restores none.
			cashDelta = min(reversal.Amount, origin.Cash-recovered)
		}
		if cashDelta == 0 {
			return nil
		}
		posted = true
		// Finalization must not leave the unrefunded remainder of an expired
		// source available until the next sweep. Retired balances remain bound
		// to the same grant and can fund this refund below.
		lots := credits.New(q, mid.UUID(), s.now)
		if _, err := lots.ExpireLapsed(ctx, g.CustomerID, currency); err != nil {
			return err
		}
		// What running charges accrued is spend the lots have funded.
		if _, err := lots.SettleRunningCharges(ctx, g.CustomerID, currency, s.now()); err != nil {
			return err
		}
		if g, err = lots.Get(ctx, g.ID); err != nil {
			return err
		}
		remaining := credits.Remaining(g)
		face := g.Units
		var faceDelta, withdrawn, retired, expired, revoked, repaid int64
		retiredAccount := ledger.RevokedCredits
		source := "credit"
		cashSource := "cash"
		lossSource := "consumed_credit"
		if !restoring {
			faceDelta = proportional(face, net.Cash+cashDelta, paid, false) - proportional(face, net.Cash, paid, false)
			withdrawn = min(max(remaining, 0), faceDelta)
			retiredBalance, err := q.GetPurchasedCreditRetiredBalance(ctx, gen.GetPurchasedCreditRetiredBalanceParams{MerchantID: mid.UUID(), CreditGrantID: g.ID})
			if err != nil {
				return err
			}
			expired = min(max(retiredBalance.Expired, 0), faceDelta-withdrawn)
			revoked = min(max(retiredBalance.Revoked, 0), faceDelta-withdrawn-expired)
			// Value that repaid owed is owed again when its payment is reversed.
			standing, err := q.GetPurchasedCreditRepaidOwed(ctx, gen.GetPurchasedCreditRepaidOwedParams{MerchantID: mid.UUID(), CreditGrantID: g.ID})
			if err != nil {
				return err
			}
			repaid = min(max(standing, 0), faceDelta-withdrawn-expired-revoked)
		} else {
			before := proportional(origin.Face, recovered, origin.Cash, false)
			after := proportional(origin.Face, recovered+cashDelta, origin.Cash, false)
			faceDelta = after - before
			// Return previously removed value before reversing consumed-value loss.
			// Sequential allocation is monotone at every integer rounding boundary;
			// rounding each source independently could create negative final legs.
			withdrawn = recoveredPart(origin.Withdrawn, 0, after) - recoveredPart(origin.Withdrawn, 0, before)
			expired = recoveredPart(origin.Expired, origin.Withdrawn, after) - recoveredPart(origin.Expired, origin.Withdrawn, before)
			revoked = recoveredPart(origin.Revoked, origin.Withdrawn+origin.Expired, after) - recoveredPart(origin.Revoked, origin.Withdrawn+origin.Expired, before)
			prior := origin.Withdrawn + origin.Expired + origin.Revoked
			repaid = recoveredPart(origin.Repaid, prior, after) - recoveredPart(origin.Repaid, prior, before)
			pastExpiry := g.ExpiresAt != nil && !g.ExpiresAt.After(s.now())
			if g.RevokedAt != nil || pastExpiry {
				retired, withdrawn = withdrawn, 0
				if pastExpiry {
					retiredAccount = ledger.ExpiredCredits
				}
			}
			source = "credit_restore:" + reversesID.String()
			cashSource = "cash_restore:" + reversesID.String()
			lossSource = "consumed_credit_restore:" + reversesID.String()
		}
		l := ledger.New(q, mid.UUID())
		clearing, err := l.EnsureSystemAccount(ctx, ledger.CreditRefundClearing, currency)
		if err != nil {
			return err
		}
		post := func(account ledger.AccountType, amount int64, kind ledger.TransferType, incoming bool, label string) error {
			if amount == 0 {
				return nil
			}
			var id uuid.UUID
			var err error
			var customer *uuid.UUID
			switch account {
			case ledger.CustomerBalance:
				id, err = l.EnsureCustomerBalance(ctx, g.CustomerID, currency)
				customer = &g.CustomerID
			case ledger.ArrearsLiability:
				id, err = l.EnsureCustomerArrears(ctx, g.CustomerID, currency)
				customer = &g.CustomerID
			default:
				id, err = l.EnsureSystemAccount(ctx, account, currency)
			}
			if err != nil {
				return err
			}
			debit, credit := id, clearing
			if !incoming {
				debit, credit = clearing, id
			}
			_, err = l.Apply(ctx, ledger.Transfer{Debit: debit, Credit: credit, Amount: amount, Currency: currency, Type: kind, Coord: ledger.Coord{Operation: "purchased_credit_refund", Source: label, SourceID: reversalID.String()}, CreditGrantID: &g.ID, Customer: customer})
			return err
		}
		balanceType, cashType := ledger.CreditRefund, ledger.CreditRefundCash
		if restoring {
			balanceType, cashType = ledger.CreditRefundRestore, ledger.CreditRefundCashRestore
		}
		if err := post(ledger.CustomerBalance, withdrawn, balanceType, !restoring, source); err != nil {
			return err
		}
		if err := post(retiredAccount, retired, ledger.CreditRefundFunding, false, "retired_"+source); err != nil {
			return err
		}
		expiredSource, revokedSource, repaidSource := "expired_credit", "revoked_credit", "repaid_owed"
		if restoring {
			expiredSource += "_restore:" + reversesID.String()
			revokedSource += "_restore:" + reversesID.String()
			repaidSource += "_restore:" + reversesID.String()
		}
		if err := post(ledger.ExpiredCredits, expired, ledger.CreditRefundFunding, !restoring, expiredSource); err != nil {
			return err
		}
		if err := post(ledger.RevokedCredits, revoked, ledger.CreditRefundFunding, !restoring, revokedSource); err != nil {
			return err
		}
		if err := post(ledger.ArrearsLiability, repaid, ledger.CreditRefundFunding, !restoring, repaidSource); err != nil {
			return err
		}
		if err := post(ledger.CreditRefundLoss, faceDelta-withdrawn-retired-expired-revoked-repaid, ledger.CreditRefundFunding, !restoring, lossSource); err != nil {
			return err
		}
		if err := post(ledger.RailClearing, cashDelta, cashType, restoring, cashSource); err != nil {
			return err
		}
		if faceDelta > cashDelta {
			return post(ledger.PromotionalFunding, faceDelta-cashDelta, ledger.CreditRefundFunding, restoring, "promotion")
		}
		return post(ledger.PlatformRevenue, cashDelta-faceDelta, ledger.CreditRefundFunding, !restoring, "revenue")
	})
	return posted && err == nil, err
}

// openCreditCash is what of the credit line's payment no reversal has returned.
func openCreditCash(paid, reversed int64) (int64, error) {
	if reversed < 0 || reversed > paid {
		return 0, fmt.Errorf("credit refunds exceed original payment")
	}
	return paid - reversed, nil
}

func fundingAmount(lot gen.BillingCreditGrant) (int64, error) {
	if lot.PaidAmount == nil || *lot.PaidAmount <= 0 || lot.PaidCurrency == nil {
		return 0, fmt.Errorf("purchased credit lacks its payment funding")
	}
	return *lot.PaidAmount, nil
}

// returnedCash is what of a purchased lot's paid amount settled reversals
// returned: its cash legs for credit in its payment's currency. A pack of a
// merchant's unit has no cash leg: the credit line takes its payment's net
// reversals first, up to what it paid.
func returnedCash(ctx context.Context, q *gen.Queries, lot gen.BillingCreditGrant, paid int64) (int64, error) {
	if *lot.PaidCurrency == lot.Unit {
		net, err := q.GetPurchasedCreditRefundState(ctx, gen.GetPurchasedCreditRefundStateParams{MerchantID: lot.MerchantID, CreditGrantID: lot.ID})
		return net.Cash, err
	}
	settled, err := q.GetPurchasedCreditSettledReversals(ctx, gen.GetPurchasedCreditSettledReversalsParams{MerchantID: lot.MerchantID, PaymentID: *lot.PaymentID})
	return min(max(settled, 0), paid), err
}

// applyUnitReversal brings a pack of a merchant's unit to what its payment's
// settled reversals take of it: their share of what the pack paid
// (returnedCash) of its units, rounded down, so partial reversals sum to the
// whole. What they take is drawn as a refund always draws, from scratch: the
// lot's units on the balance first, then what expired or was revoked of it,
// and the rest is consumed; each part moves by the difference from what
// earlier reversals took, so a recovery returns units in the reverse order.
// The legs route through credit_refund_clearing and the units go back to
// processor_clearing, which issued them: the unit has no cash. It reports
// whether it posted; a reversal that posted is not applied again.
func (s *Service) applyUnitReversal(ctx context.Context, q *gen.Queries, mid uuid.UUID, g gen.BillingCreditGrant, reversalID uuid.UUID) (bool, error) {
	unit := g.Unit
	if g.PaymentID == nil {
		return false, fmt.Errorf("purchased credit lacks its payment")
	}
	if _, _, err := s.lockBalance(ctx, q, mid, g.CustomerID, unit); err != nil {
		return false, err
	}
	reversal, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid, ID: reversalID})
	if err != nil {
		return false, err
	}
	if reversal.CustomerID != g.CustomerID || reversal.Currency != *g.PaidCurrency || reversal.RefundedPaymentID == nil || *reversal.RefundedPaymentID != *g.PaymentID || reversal.Status != "succeeded" || reversal.Amount == 0 {
		return false, fmt.Errorf("credit reversal contradicts completed payment")
	}
	committed, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid, CreditGrantID: g.ID, ReversalID: reversalID.String()})
	if err != nil || committed.Legs > 0 {
		return false, err
	}
	paid, err := fundingAmount(g)
	if err != nil {
		return false, err
	}
	returned, err := returnedCash(ctx, q, g, paid)
	if err != nil {
		return false, err
	}
	taken, err := q.GetPurchasedCreditUnitRefundState(ctx, gen.GetPurchasedCreditUnitRefundStateParams{MerchantID: mid, CreditGrantID: g.ID})
	if err != nil {
		return false, err
	}
	target := proportional(g.Units, returned, paid, false)
	delta := target - (taken.Withdrawn + taken.Expired + taken.Revoked + taken.Consumed)
	if delta == 0 {
		return false, nil
	}
	lots := credits.New(q, mid, s.now)
	if _, err := lots.ExpireLapsed(ctx, g.CustomerID, unit); err != nil {
		return false, err
	}
	if _, err := lots.SettleRunningCharges(ctx, g.CustomerID, unit, s.now()); err != nil {
		return false, err
	}
	if g, err = lots.Get(ctx, g.ID); err != nil {
		return false, err
	}
	retired, err := q.GetPurchasedCreditRetiredBalance(ctx, gen.GetPurchasedCreditRetiredBalanceParams{MerchantID: mid, CreditGrantID: g.ID})
	if err != nil {
		return false, err
	}
	// What the reversals take, as if none had taken anything yet.
	withdrawn := min(target, max(credits.Remaining(g), 0)+taken.Withdrawn)
	expired := min(target-withdrawn, max(retired.Expired, 0)+taken.Expired)
	revoked := min(target-withdrawn-expired, max(retired.Revoked, 0)+taken.Revoked)
	consumed := target - withdrawn - expired - revoked

	l := ledger.New(q, mid)
	clearing, err := l.EnsureSystemAccount(ctx, ledger.CreditRefundClearing, unit)
	if err != nil {
		return false, err
	}
	// leg moves amount of the lot's units from one account to the other.
	leg := func(from, to uuid.UUID, amount int64, kind ledger.TransferType, label string, customer *uuid.UUID) error {
		if amount <= 0 {
			return nil
		}
		_, err := l.Apply(ctx, ledger.Transfer{Debit: from, Credit: to, Amount: amount, Currency: unit, Type: kind,
			Coord: ledger.Coord{Operation: "purchased_credit_refund", Source: label, SourceID: reversalID.String()}, CreditGrantID: &g.ID, Customer: customer})
		return err
	}
	account := func(t ledger.AccountType) (uuid.UUID, error) {
		if t == ledger.CustomerBalance {
			return l.EnsureCustomerBalance(ctx, g.CustomerID, unit)
		}
		return l.EnsureSystemAccount(ctx, t, unit)
	}
	// Each part moves into the clearing account by what it takes more than
	// before, and back by what it takes less. Units a lot no longer holds go
	// back where its end put the rest.
	backTo, backLabel, backType := ledger.CustomerBalance, "credit_restore", ledger.CreditRefundRestore
	switch {
	case g.ExpiresAt != nil && !g.ExpiresAt.After(s.now()):
		backTo, backLabel, backType = ledger.ExpiredCredits, "retired_credit_restore", ledger.CreditRefundFunding
	case g.RevokedAt != nil:
		backTo, backLabel, backType = ledger.RevokedCredits, "retired_credit_restore", ledger.CreditRefundFunding
	}
	for _, part := range []struct {
		from, back           ledger.AccountType
		change               int64
		take, restore        ledger.TransferType
		takeLabel, backLabel string
	}{
		{ledger.CustomerBalance, backTo, withdrawn - taken.Withdrawn, ledger.CreditRefund, backType, "credit", backLabel},
		{ledger.ExpiredCredits, ledger.ExpiredCredits, expired - taken.Expired, ledger.CreditRefundFunding, ledger.CreditRefundFunding, "expired_credit", "expired_credit_restore"},
		{ledger.RevokedCredits, ledger.RevokedCredits, revoked - taken.Revoked, ledger.CreditRefundFunding, ledger.CreditRefundFunding, "revoked_credit", "revoked_credit_restore"},
		{ledger.CreditRefundLoss, ledger.CreditRefundLoss, consumed - taken.Consumed, ledger.CreditRefundFunding, ledger.CreditRefundFunding, "consumed_credit", "consumed_credit_restore"},
	} {
		if part.change == 0 {
			continue
		}
		from, back := part.from, part.back
		id, err := account(from)
		if part.change < 0 {
			id, err = account(back)
		}
		if err != nil {
			return false, err
		}
		var customer *uuid.UUID
		if id == g.AccountID {
			customer = &g.CustomerID
		}
		if part.change > 0 {
			err = leg(id, clearing, part.change, part.take, part.takeLabel, customer)
		} else {
			err = leg(clearing, id, -part.change, part.restore, part.backLabel, customer)
		}
		if err != nil {
			return false, err
		}
	}
	// The units go back to the account that issued them, or come out again.
	issued, err := l.EnsureSystemAccount(ctx, ledger.RailClearing, unit)
	if err != nil {
		return false, err
	}
	if delta > 0 {
		err = leg(clearing, issued, delta, ledger.CreditRefundFunding, "units_returned", nil)
	} else {
		err = leg(issued, clearing, -delta, ledger.CreditRefundFunding, "units_returned_restore", nil)
	}
	return err == nil, err
}

// proportional uses integer arithmetic without int64 multiplication overflow.
func proportional(face, cash, paid int64, roundUp bool) int64 {
	if face <= 0 || cash <= 0 || paid <= 0 {
		return 0
	}
	product := new(big.Int).Mul(big.NewInt(face), big.NewInt(cash))
	if roundUp {
		product.Add(product, big.NewInt(paid-1))
	}
	return product.Quo(product, big.NewInt(paid)).Int64()
}

func recoveredPart(amount, preceding, total int64) int64 { return min(amount, max(total-preceding, 0)) }
