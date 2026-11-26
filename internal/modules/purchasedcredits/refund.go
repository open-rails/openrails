package purchasedcredits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/money/credits"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// A voluntary refund of a bought lot's payment takes the lot's units off the
// balance when it is requested (WithdrawRefund, credit_refund at the refund's
// id), and gives them back (credit_refund_restore) when the provider refund
// fails (ReleaseRefund). Their money stays deferred until the refund settles
// (units_refunding), when ApplyReversal books it.

// ValidateRefund checks, without writing, that a refund of additional of the
// payment's cash finds the units it withdraws of the lot the payment bought
// unspent: live on the lot and not owed to pending usage.
func (s *Service) ValidateRefund(ctx context.Context, paymentID uuid.UUID, additional int64) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	if additional <= 0 {
		return fmt.Errorf("invalid credit refund amount: %w", billing.ErrInvalid)
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
		pending, err := q.GetPurchasedCreditPendingRefunds(ctx, gen.GetPurchasedCreditPendingRefundsParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if err != nil {
			return err
		}
		_, err = s.refundUnits(ctx, q, mid.UUID(), grant, pending, additional)
		return err
	})
}

// WithdrawRefund takes off the balance, in the caller's transaction, the
// units a pending refund (reversal, of cash) withdraws from the lot the
// payment bought, after the customer's usage and running charges posted: the
// lot funds what came before. The refund's row is already written.
func (s *Service) WithdrawRefund(ctx context.Context, paymentID, reversal uuid.UUID, cash int64) error {
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
		lots := credits.New(q, mid.UUID(), s.now)
		if _, err := lots.SettleRunningCharges(ctx, grant.CustomerID, grant.Unit, s.now()); err != nil {
			return err
		}
		pending, err := q.GetPurchasedCreditPendingRefunds(ctx, gen.GetPurchasedCreditPendingRefundsParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if err != nil {
			return err
		}
		units, err := s.refundUnits(ctx, q, mid.UUID(), grant, max(pending-cash, 0), cash)
		if err != nil || units == 0 {
			return err
		}
		lot, err := lots.Get(ctx, grant.ID)
		if err != nil {
			return err
		}
		if err := postUnits(ctx, ledger.New(q, mid.UUID()), lot, reversal, ledger.CustomerUnits, ledger.UnitsRefunded, units, ledger.CreditRefund, "credit"); err != nil {
			return err
		}
		return lots.SyncRefunding(ctx, paymentID)
	})
}

// ReleaseRefund gives back, in the caller's transaction, the units a refund
// that failed withdrew. Idempotent.
func (s *Service) ReleaseRefund(ctx context.Context, paymentID, reversal uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		lot, err := q.GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		state, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), CreditGrantID: lot.ID, ReversalID: reversal.String()})
		if err != nil {
			return err
		}
		if state.Withdrawn > 0 {
			if err := postUnits(ctx, ledger.New(q, mid.UUID()), lot, reversal, ledger.UnitsRefunded, ledger.CustomerUnits, state.Withdrawn, ledger.CreditRefundRestore, "credit_release"); err != nil {
				return err
			}
		}
		return credits.New(q, mid.UUID(), s.now).SyncRefunding(ctx, paymentID)
	})
}

// supersedePending gives back to the lot up to short units that the payment's
// pending refunds withdrew, newest first, for reversal, which settled before
// them.
func (s *Service) supersedePending(ctx context.Context, q *gen.Queries, lot gen.BillingCreditGrant, reversal uuid.UUID, short int64) error {
	pending, err := q.ListPendingCreditRefunds(ctx, gen.ListPendingCreditRefundsParams{MerchantID: lot.MerchantID, CreditGrantID: lot.ID})
	if err != nil {
		return err
	}
	l := ledger.New(q, lot.MerchantID)
	for _, r := range pending {
		if short == 0 {
			break
		}
		back := min(short, r.Withdrawn)
		if back <= 0 || r.ReversalID == reversal {
			continue
		}
		if err := postUnits(ctx, l, lot, r.ReversalID, ledger.UnitsRefunded, ledger.CustomerUnits, back, ledger.CreditRefundRestore, "credit_superseded:"+reversal.String()); err != nil {
			return err
		}
		short -= back
	}
	return credits.New(q, lot.MerchantID, s.now).SyncRefunding(ctx, *lot.PaymentID)
}

// refundUnits is what a refund of cash withdraws from the lot, under the
// customer's lock: its share of the lot's face, cumulative with the refunds
// before it (pending, the pending refunds' cash ahead of it), rounded down.
// A revoked or lapsed lot is not refundable, and the units must be on the lot
// and not owed to pending usage.
func (s *Service) refundUnits(ctx context.Context, q *gen.Queries, mid uuid.UUID, grant gen.BillingCreditGrant, pending, cash int64) (int64, error) {
	balance, unposted, err := s.lockBalance(ctx, q, mid, grant.CustomerID, grant.Unit)
	if err != nil {
		return 0, err
	}
	paid, err := fundingAmount(grant)
	if err != nil {
		return 0, err
	}
	net, err := q.GetPurchasedCreditRefundState(ctx, gen.GetPurchasedCreditRefundStateParams{MerchantID: mid, CreditGrantID: grant.ID})
	if err != nil {
		return 0, err
	}
	open, err := openCreditCash(paid, net.Cash)
	if err != nil {
		return 0, err
	}
	// Refunds settle against the credit line first, in the order they came.
	before := min(pending, open)
	requested := min(cash, open-before)
	if requested == 0 {
		return 0, nil
	}
	lot, err := credits.New(q, mid, s.now).Get(ctx, grant.ID)
	if err != nil {
		return 0, err
	}
	if lot.RevokedAt != nil || (lot.ExpiresAt != nil && !lot.ExpiresAt.After(s.now())) {
		return 0, fmt.Errorf("credit grant is no longer refundable: %w", billing.ErrConflict)
	}
	units := proportional(lot.Units, net.Cash+before+requested, paid, false) - proportional(lot.Units, net.Cash+before, paid, false)
	if units > credits.Remaining(lot) || units > balance-unposted {
		return 0, fmt.Errorf("refund requires unused purchased credit: %w", billing.ErrConflict)
	}
	return units, nil
}

// ApplyReversal decides, once, a settled reversal's share of the lot its
// payment bought, and posts it: a negative payment returns the credit line's
// money first (the rest of an invoice's payment bought no credit); a recovery
// restores what its chargeback took. The share is recorded, zero included,
// so a later call never revisits it; decided reports whether this call
// decided it. The units always move; the money only once the general ledger
// booked the lot.
func (s *Service) ApplyReversal(ctx context.Context, paymentID, reversalID uuid.UUID, reversesID *uuid.UUID) (decided bool, err error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		decided = false
		q := gen.New(tx)
		g, err := q.GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// The lot's units are in its unit; its money, and every payment of
		// it, in its paid currency (a merchant's own unit is bought with
		// money).
		currency := g.Unit
		paidCurrency, err := fundingCurrency(g)
		if err != nil {
			return err
		}
		path := reversalPathOf(books.CreditLotOf(g))
		if _, _, err := s.lockBalance(ctx, q, mid.UUID(), g.CustomerID, currency); err != nil {
			return err
		}
		reversal, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: reversalID})
		if err != nil {
			return err
		}
		if reversal.CustomerID != g.CustomerID || reversal.Currency != paidCurrency || reversal.RefundedPaymentID == nil || *reversal.RefundedPaymentID != paymentID || reversal.Status != "succeeded" || reversal.Amount == 0 {
			return fmt.Errorf("credit reversal contradicts completed payment")
		}
		// What a refund withdrew when it was requested is already off the
		// balance.
		committed, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: reversalID.String()})
		if err != nil {
			return err
		}
		// A reversal's share is decided once, when it first settles: a later
		// recovery never gives an earlier reversal more of the lot.
		if done, err := q.CreditReversalDecided(ctx, gen.CreditReversalDecidedParams{MerchantID: mid.UUID(), ReversalID: reversalID, CreditGrantID: g.ID}); err != nil || done {
			return err
		}
		// decide records the share: the reversal's own, or what a recovery
		// restored of origin's.
		decide := func(origin uuid.UUID, cash, units int64) error {
			decided = true
			return q.RecordCreditReversalShare(ctx, gen.RecordCreditReversalShareParams{MerchantID: mid.UUID(), ReversalID: reversalID, CreditGrantID: g.ID, OriginID: origin, Cash: cash, Units: units})
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
		// move posts cashDelta of the lot's money, and the units it is worth,
		// out of the lot or, for a recovery of reversesID (which took share
		// and had recovered back), into it; it returns the units.
		move := func(reversesID *uuid.UUID, cashDelta int64, origin gen.GetPurchasedCreditReversalStateRow, share gen.GetCreditReversalShareRow, recovered int64) (int64, error) {
			var err error
			// Finalization must not leave the unrefunded remainder of an expired
			// source available until the next sweep. Retired balances remain bound
			// to the same grant and can fund this refund below.
			lots := credits.New(q, mid.UUID(), s.now)
			if _, err := lots.ExpireLapsed(ctx, g.CustomerID, currency); err != nil {
				return 0, err
			}
			// What running charges accrued is spend the lots have funded.
			if _, err := lots.SettleRunningCharges(ctx, g.CustomerID, currency, s.now()); err != nil {
				return 0, err
			}
			if g, err = lots.Get(ctx, g.ID); err != nil {
				return 0, err
			}
			remaining := credits.Remaining(g)
			face := g.Units
			var faceDelta, withdrawn, retired, expired, revoked, repaid int64
			// more is what this call withdraws beyond what the request did,
			// excess what the request withdrew beyond the reversal's share.
			var more, excess int64
			retiredAccount := ledger.UnitsRevoked
			source := "credit"
			lossSource := "consumed_credit"
			if !restoring {
				faceDelta = proportional(face, net.Cash+cashDelta, paid, false) - proportional(face, net.Cash, paid, false)
				kept := min(committed.Withdrawn, faceDelta)
				excess = committed.Withdrawn - kept
				// A settled reversal comes before pending refunds: what they
				// withdrew goes back to the lot for it, and each takes what is
				// left of its share when it settles.
				if short := faceDelta - kept - max(remaining, 0); short > 0 {
					if err := s.supersedePending(ctx, q, g, reversalID, short); err != nil {
						return 0, err
					}
					if g, err = lots.Get(ctx, g.ID); err != nil {
						return 0, err
					}
					remaining = credits.Remaining(g)
				}
				more = min(max(remaining, 0), faceDelta-kept)
				withdrawn = kept + more
				retiredBalance, err := q.GetPurchasedCreditRetiredBalance(ctx, gen.GetPurchasedCreditRetiredBalanceParams{MerchantID: mid.UUID(), CreditGrantID: g.ID})
				if err != nil {
					return 0, err
				}
				expired = min(max(retiredBalance.Expired, 0), faceDelta-withdrawn)
				revoked = min(max(retiredBalance.Revoked, 0), faceDelta-withdrawn-expired)
				// Value that repaid owed is owed again when its payment is reversed.
				standing, err := q.GetPurchasedCreditRepaidOwed(ctx, gen.GetPurchasedCreditRepaidOwedParams{MerchantID: mid.UUID(), CreditGrantID: g.ID})
				if err != nil {
					return 0, err
				}
				repaid = min(max(standing, 0), faceDelta-withdrawn-expired-revoked)
			} else {
				before := proportional(share.Units, recovered, share.Cash, false)
				after := proportional(share.Units, recovered+cashDelta, share.Cash, false)
				faceDelta = after - before
				// Return previously removed value before reversing consumed-value loss.
				// Sequential allocation is monotone at every integer rounding boundary;
				// rounding each source independently could create negative final legs.
				withdrawn = recoveredPart(origin.Withdrawn, 0, after) - recoveredPart(origin.Withdrawn, 0, before)
				expired = recoveredPart(origin.Expired, origin.Withdrawn, after) - recoveredPart(origin.Expired, origin.Withdrawn, before)
				revoked = recoveredPart(origin.Revoked, origin.Withdrawn+origin.Expired, after) - recoveredPart(origin.Revoked, origin.Withdrawn+origin.Expired, before)
				prior := origin.Withdrawn + origin.Expired + origin.Revoked
				repaid = recoveredPart(origin.Repaid, prior, after) - recoveredPart(origin.Repaid, prior, before)
				// Owed is repaid again only while it is outstanding: what was
				// repaid since returns to the balance, as units withdrawn.
				outstanding, err := ledger.New(q, mid.UUID()).OutstandingOwed(ctx, g.CustomerID, currency)
				if err != nil {
					return 0, err
				}
				if repaid > outstanding {
					withdrawn += repaid - outstanding
					repaid = outstanding
				}
				pastExpiry := g.ExpiresAt != nil && !g.ExpiresAt.After(s.now())
				if g.RevokedAt != nil || pastExpiry {
					retired, withdrawn = withdrawn, 0
					if pastExpiry {
						retiredAccount = ledger.UnitsExpired
					}
				}
				more = withdrawn
				source = "credit_restore:" + reversesID.String()
				lossSource = "consumed_credit_restore:" + reversesID.String()
			}
			l := ledger.New(q, mid.UUID())
			units := unitsMoved{withdrawn: more, retired: retired, expired: expired, revoked: revoked, repaid: repaid,
				consumed: faceDelta - withdrawn - retired - expired - revoked - repaid, retiredAccount: retiredAccount}
			if committed.Withdrawn > 0 {
				// Its request posted at "credit"; what it settles beyond or short
				// of that posts apart.
				source = "credit_settled"
				if err := postUnits(ctx, l, g, reversalID, ledger.UnitsRefunded, ledger.CustomerUnits, excess, ledger.CreditRefundRestore, "credit_excess"); err != nil {
					return 0, err
				}
			}
			// The reversal is settled: its units are no longer refunding.
			if err := lots.SyncRefunding(ctx, paymentID); err != nil {
				return 0, err
			}
			if g, err = lots.Get(ctx, g.ID); err != nil {
				return 0, err
			}
			leg := func(from, to ledger.AccountType, amount int64, kind ledger.TransferType, label string) error {
				return postUnits(ctx, l, g, reversalID, from, to, amount, kind, label)
			}
			gl := books.New(q, mid.UUID())
			// Dated when the reversal settled, as its other legs are.
			at := books.SettledAt(reversal)
			// The cash returns to, and is recovered into, where the lot's
			// payment brought it.
			bought, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: paymentID})
			if err != nil {
				return 0, err
			}
			account := books.CashAccount(bought)
			if !restoring {
				// Every unit the reversal takes goes to units_refunded, from where
				// it was; its money is the general ledger's.
				if err := leg(ledger.CustomerUnits, ledger.UnitsRefunded, units.withdrawn, ledger.CreditRefund, source); err != nil {
					return 0, err
				}
				for _, from := range []struct {
					account ledger.AccountType
					amount  int64
					label   string
				}{{ledger.UnitsExpired, units.expired, "expired_credit"}, {ledger.UnitsRevoked, units.revoked, "revoked_credit"},
					{ledger.CustomerOwed, units.repaid, "repaid_owed"}, {ledger.UnitsRedeemed, units.consumed, lossSource}} {
					if err := leg(from.account, ledger.UnitsRefunded, from.amount, ledger.CreditRefundFunding, from.label); err != nil {
						return 0, err
					}
				}
				if path == reversalUnbooked {
					return faceDelta, unbookedLot(ctx, q, g, paymentID, reversalID)
				}
				if g, err = lots.Get(ctx, g.ID); err != nil {
					return 0, err
				}
				zone, err := books.AccountingZone(ctx, s.db.NewWithPgxTx(tx))
				if err != nil {
					return 0, err
				}
				return faceDelta, gl.ReverseCredit(ctx, books.CreditLotOf(g), books.CreditReversal{
					Reversal: reversalID, Chargeback: reversal.ReversalKind != nil && *reversal.ReversalKind == reversalChargeback,
					Cash: cashDelta, CashAccount: account, Repaid: units.repaid, Consumed: units.consumed, Lapsed: units.expired + units.revoked,
					At: at, Now: s.now(), Zone: zone,
				})
			}
			restore := ":" + reversesID.String()
			if err := leg(ledger.UnitsRefunded, ledger.CustomerUnits, units.withdrawn, ledger.CreditRefundRestore, source); err != nil {
				return 0, err
			}
			for _, to := range []struct {
				account ledger.AccountType
				amount  int64
				label   string
			}{{units.retiredAccount, units.retired, "retired_" + source}, {ledger.UnitsExpired, units.expired, "expired_credit_restore" + restore},
				{ledger.UnitsRevoked, units.revoked, "revoked_credit_restore" + restore}, {ledger.CustomerOwed, units.repaid, "repaid_owed_restore" + restore},
				{ledger.UnitsRedeemed, units.consumed, lossSource}} {
				if err := leg(ledger.UnitsRefunded, to.account, to.amount, ledger.CreditRefundFunding, to.label); err != nil {
					return 0, err
				}
			}
			if path == reversalUnbooked {
				return faceDelta, unbookedLot(ctx, q, g, paymentID, reversalID)
			}
			original, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: *reversesID})
			if err != nil {
				return 0, err
			}
			if g, err = lots.Get(ctx, g.ID); err != nil {
				return 0, err
			}
			lot := books.CreditLotOf(g)
			zone, err := books.AccountingZone(ctx, s.db.NewWithPgxTx(tx))
			if err != nil {
				return 0, err
			}
			if err := gl.RecoverCredit(ctx, lot, books.CreditRecovery{
				Recovery: reversalID, Origin: *reversesID, Chargeback: original.ReversalKind != nil && *original.ReversalKind == reversalChargeback,
				Cash: cashDelta, CashAccount: account, Repaid: units.repaid, Retired: units.retired, At: at, Now: s.now(), Zone: zone,
			}); err != nil {
				return 0, err
			}
			if units.retired == 0 {
				return faceDelta, nil
			}
			// Units restored to a revoked or lapsed lot are retired at once, their
			// money with them.
			if g, err = lots.Get(ctx, g.ID); err != nil {
				return 0, err
			}
			kind := books.CreditRevoked
			if units.retiredAccount == ledger.UnitsExpired {
				kind = books.CreditExpired
			}
			return faceDelta, gl.MoveCredit(ctx, books.CreditLotOf(g), books.CreditMove{Kind: kind, At: at,
				Coord: books.Coord{Operation: books.OpCreditRefund, Source: "money_retired_restore" + restore, SourceID: reversalID.String()}})
		}
		if !restoring {
			if reversesID != nil || reversal.Amount == (-1<<63) {
				return fmt.Errorf("invalid negative credit reversal")
			}
			open, err := openCreditCash(paid, net.Cash)
			if err != nil {
				return err
			}
			// The credit line's part of the reversal.
			cash := min(-reversal.Amount, open)
			units := int64(0)
			switch {
			case cash > 0:
				if units, err = move(nil, cash, gen.GetPurchasedCreditReversalStateRow{}, gen.GetCreditReversalShareRow{}, 0); err != nil {
					return err
				}
			case committed.Withdrawn > 0:
				// Reversals that settled first took the whole line: what the
				// request withdrew goes back.
				if err := postUnits(ctx, ledger.New(q, mid.UUID()), g, reversalID, ledger.UnitsRefunded, ledger.CustomerUnits, committed.Withdrawn, ledger.CreditRefundRestore, "credit_excess"); err != nil {
					return err
				}
				if err := credits.New(q, mid.UUID(), s.now).SyncRefunding(ctx, paymentID); err != nil {
					return err
				}
			}
			return decide(reversalID, cash, units)
		}
		// A recovery restores what its chargeback took of the lot; one naming
		// none, what chargebacks took, the oldest first. Only a chargeback's:
		// a dispute win never restores what a voluntary refund withdrew.
		var origins []uuid.UUID
		if reversesID == nil {
			if origins, err = chargebacksByOccurrence(ctx, q, mid.UUID(), paymentID); err != nil {
				return err
			}
		} else {
			original, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: *reversesID})
			if err != nil {
				return err
			}
			if original.Amount >= 0 || original.CustomerID != g.CustomerID || original.RefundedPaymentID == nil || *original.RefundedPaymentID != paymentID || original.Currency != paidCurrency {
				return fmt.Errorf("credit recovery does not reference this purchase's reversal")
			}
			if reversal.Amount > -original.Amount {
				return fmt.Errorf("credit recovery exceeds its recorded reversal")
			}
			origins = []uuid.UUID{*reversesID}
		}
		left, restored := reversal.Amount, false
		for _, id := range origins {
			origin, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: id.String()})
			if err != nil {
				return err
			}
			share, err := q.GetCreditReversalShare(ctx, gen.GetCreditReversalShareParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: id})
			if err != nil {
				return err
			}
			recovered, err := q.GetCreditRecoveredShare(ctx, gen.GetCreditRecoveredShareParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: id})
			if err != nil {
				return err
			}
			if recovered.Cash > share.Cash {
				return fmt.Errorf("credit recovery exceeds its recorded reversal")
			}
			// A recovery restores what its reversal took from the credit line
			// first: a reversal that took none restores none.
			c := min(left, share.Cash-recovered.Cash)
			if c == 0 {
				continue
			}
			u, err := move(&id, c, origin, share, recovered.Cash)
			if err != nil {
				return err
			}
			if err := decide(id, c, u); err != nil {
				return err
			}
			left, restored = left-c, true
		}
		if !restored {
			return decide(reversalID, 0, 0)
		}
		return nil
	})
	return decided && err == nil, err
}

// reversalPath is how a reversal of a paid lot posts.
type reversalPath int

const (
	// reversalInBooks: the units to units_refunded, the cash in the general
	// ledger.
	reversalInBooks reversalPath = iota
	// reversalUnbooked: a lot the general ledger never booked (its payment
	// brought no cash to apply) moves its units alone; the units book holds
	// no money, and the reversal's cash is booked against the payment.
	reversalUnbooked
)

func reversalPathOf(l books.CreditLot) reversalPath {
	if l.Booked() {
		return reversalInBooks
	}
	return reversalUnbooked
}

// UnbookedCreditFindingType is the finding a reversal of a paid lot raises
// when the general ledger never booked the lot.
const UnbookedCreditFindingType = "books.credit.unbooked"

// unbookedLot records, once per lot, that a reversal moved a lot's units
// while the general ledger never booked the lot (its payment's cash did not
// apply: books.credit.unapplied).
func unbookedLot(ctx context.Context, q *gen.Queries, g gen.BillingCreditGrant, paymentID, reversalID uuid.UUID) error {
	raw, err := json.Marshal(map[string]any{"credit_grant_id": g.ID, "payment_id": paymentID, "reversal_id": reversalID, "unit": g.Unit, "paid_currency": g.PaidCurrency})
	if err != nil {
		return err
	}
	action := "The general ledger never booked this credit lot: its reversals withdraw its units, and their money is booked against the payment. Book the lot's price (see books.credit.unapplied) to settle its deferred revenue."
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: g.MerchantID, FindingType: UnbookedCreditFindingType, SubjectKey: "credit_grant:" + g.ID.String(),
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: raw,
	})
	if err != nil {
		return fmt.Errorf("raise %s: %w", UnbookedCreditFindingType, err)
	}
	return nil
}

// reversalChargeback is payments.ReversalChargeback (payments imports this
// package).
const reversalChargeback = "chargeback"

// unitsMoved is how a reversal's face splits across where the lot's units
// were.
type unitsMoved struct {
	withdrawn, retired, expired, revoked, repaid, consumed int64
	retiredAccount                                         ledger.AccountType
}

// postUnits posts one units-book leg of a reversal, naming the lot and, on a
// customer's account, the customer.
func postUnits(ctx context.Context, l *ledger.Ledger, g gen.BillingCreditGrant, reversalID uuid.UUID, from, to ledger.AccountType, amount int64, kind ledger.TransferType, label string) error {
	if amount == 0 {
		return nil
	}
	var customer *uuid.UUID
	account := func(t ledger.AccountType) (uuid.UUID, error) {
		switch t {
		case ledger.CustomerUnits:
			customer = &g.CustomerID
			return l.EnsureCustomerBalance(ctx, g.CustomerID, g.Unit)
		case ledger.CustomerOwed:
			customer = &g.CustomerID
			return l.EnsureCustomerArrears(ctx, g.CustomerID, g.Unit)
		}
		return l.EnsureSystemAccount(ctx, t, g.Unit)
	}
	debit, err := account(from)
	if err != nil {
		return err
	}
	credit, err := account(to)
	if err != nil {
		return err
	}
	_, err = l.Apply(ctx, ledger.Transfer{Debit: debit, Credit: credit, Amount: amount, Currency: g.Unit, Type: kind,
		Coord: ledger.Coord{Operation: books.OpCreditRefund, Source: label, SourceID: reversalID.String()}, CreditGrantID: &g.ID, Customer: customer})
	return err
}

// openCreditCash is what of the credit line's payment no reversal has returned.
func openCreditCash(paid, reversed int64) (int64, error) {
	if reversed < 0 || reversed > paid {
		return 0, fmt.Errorf("credit refunds exceed original payment")
	}
	return paid - reversed, nil
}

func fundingAmount(lot gen.BillingCreditGrant) (int64, error) {
	if _, err := fundingCurrency(lot); err != nil {
		return 0, err
	}
	return *lot.PaidAmount, nil
}

// fundingCurrency is the currency a bought lot was paid in: its unit, or the
// money a merchant's own unit was bought with.
func fundingCurrency(lot gen.BillingCreditGrant) (string, error) {
	if lot.PaidAmount == nil || *lot.PaidAmount <= 0 || lot.PaidCurrency == nil || *lot.PaidCurrency == "" {
		return "", fmt.Errorf("purchased credit lacks its payment funding")
	}
	return *lot.PaidCurrency, nil
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

// chargebacksByOccurrence is the payment's settled chargebacks, oldest first
// by when they occurred.
func chargebacksByOccurrence(ctx context.Context, q *gen.Queries, merchant, payment uuid.UUID) ([]uuid.UUID, error) {
	rows, err := q.ListRefundsForPayment(ctx, gen.ListRefundsForPaymentParams{MerchantID: merchant, RefundedPaymentID: &payment})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b gen.BillingPayment) int {
		if c := a.PurchasedAt.Compare(b.PurchasedAt); c != 0 {
			return c
		}
		return slices.Compare(a.ID[:], b.ID[:])
	})
	var out []uuid.UUID
	for _, r := range rows {
		if r.Status == "succeeded" && r.Amount < 0 && r.ReversalKind != nil && *r.ReversalKind == reversalChargeback {
			out = append(out, r.ID)
		}
	}
	return out, nil
}
