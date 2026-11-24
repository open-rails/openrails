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
		net, err := q.GetPurchasedCreditRefundState(ctx, gen.GetPurchasedCreditRefundStateParams{MerchantID: mid.UUID(), CreditGrantID: grant.ID})
		if err != nil {
			return err
		}
		pending, err := q.GetPurchasedCreditPendingRefunds(ctx, gen.GetPurchasedCreditPendingRefundsParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if err != nil {
			return err
		}
		open, err := openCreditCash(paid, net.Cash)
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

// ApplyReversal decides, once, a settled reversal's share of the lot its
// payment bought, and posts it: a negative payment returns the credit line's
// money first (the rest of an order's payment bought no credit); a recovery
// restores what its chargeback took. The share is recorded, zero included,
// so a later call never revisits it. posted reports whether it moved any.
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
		// The lot's units are in its unit; its money, and every payment of
		// it, in its paid currency (a merchant's own unit is bought with
		// money).
		currency := g.Unit
		paidCurrency, err := fundingCurrency(g)
		if err != nil {
			return err
		}
		if reversalPathOf(books.CreditLotOf(g)) == reversalRefused {
			// It waits, unapplied, until the lot is booked.
			return refuseUnbookedUnitPack(ctx, q, g, paymentID, reversalID)
		}
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
		committed, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: reversalID.String()})
		if err != nil || committed.Legs > 0 {
			return err
		}
		// A reversal's share is decided once, when it first settles: a later
		// recovery never gives an earlier reversal more of the lot.
		if decided, err := q.CreditReversalDecided(ctx, gen.CreditReversalDecidedParams{MerchantID: mid.UUID(), ReversalID: reversalID, CreditGrantID: g.ID}); err != nil || decided {
			return err
		}
		decide := func(cash, units int64) error {
			return q.RecordCreditReversalShare(ctx, gen.RecordCreditReversalShareParams{MerchantID: mid.UUID(), ReversalID: reversalID, CreditGrantID: g.ID, Cash: cash, Units: units})
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
		// out of the lot or, for a recovery of reversesID, back into it; it
		// returns the units.
		move := func(reversesID *uuid.UUID, cashDelta int64, origin gen.GetPurchasedCreditReversalStateRow, recovered int64) (int64, error) {
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
			retiredAccount := ledger.UnitsRevoked
			source := "credit"
			cashSource := "cash"
			lossSource := "consumed_credit"
			if !restoring {
				faceDelta = proportional(face, net.Cash+cashDelta, paid, false) - proportional(face, net.Cash, paid, false)
				withdrawn = min(max(remaining, 0), faceDelta)
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
				source = "credit_restore:" + reversesID.String()
				cashSource = "cash_restore:" + reversesID.String()
				lossSource = "consumed_credit_restore:" + reversesID.String()
			}
			l := ledger.New(q, mid.UUID())
			units := unitsMoved{withdrawn: withdrawn, retired: retired, expired: expired, revoked: revoked, repaid: repaid,
				consumed: faceDelta - withdrawn - retired - expired - revoked - repaid, retiredAccount: retiredAccount}
			if reversalPathOf(books.CreditLotOf(g)) == reversalLegacy {
				// A lot from before the credit books keeps its reversals' cash in
				// the units book until the books are rebuilt.
				return faceDelta, postLegacyReversal(ctx, l, g, reversalID, reversesID, restoring, cashDelta, faceDelta, units, source, cashSource, lossSource)
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
				if g, err = lots.Get(ctx, g.ID); err != nil {
					return 0, err
				}
				return faceDelta, gl.ReverseCredit(ctx, books.CreditLotOf(g), books.CreditReversal{
					Reversal: reversalID, Chargeback: reversal.ReversalKind != nil && *reversal.ReversalKind == reversalChargeback,
					Cash: cashDelta, CashAccount: account, Repaid: units.repaid, At: at,
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
			original, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: *reversesID})
			if err != nil {
				return 0, err
			}
			if g, err = lots.Get(ctx, g.ID); err != nil {
				return 0, err
			}
			lot := books.CreditLotOf(g)
			if err := gl.RecoverCredit(ctx, lot, books.CreditRecovery{
				Recovery: reversalID, Origin: *reversesID, Chargeback: original.ReversalKind != nil && *original.ReversalKind == reversalChargeback,
				Cash: cashDelta, CashAccount: account, Repaid: units.repaid, Retired: units.retired, At: at,
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
			if cash == 0 {
				return decide(0, 0)
			}
			posted = true
			units, err := move(nil, cash, gen.GetPurchasedCreditReversalStateRow{}, 0)
			if err != nil {
				return err
			}
			return decide(cash, units)
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
		left := reversal.Amount
		var cash, units int64
		for _, id := range origins {
			origin, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: id.String()})
			if err != nil {
				return err
			}
			recovered, err := q.GetPurchasedCreditRecoveryTotal(ctx, gen.GetPurchasedCreditRecoveryTotalParams{MerchantID: mid.UUID(), CreditGrantID: g.ID, ReversalID: id.String()})
			if err != nil {
				return err
			}
			if origin.Cash < 0 || recovered < 0 || recovered > origin.Cash {
				return fmt.Errorf("credit recovery exceeds its recorded reversal")
			}
			// A recovery restores what its reversal took from the credit line
			// first: a reversal that took none restores none.
			c := min(left, origin.Cash-recovered)
			if c == 0 {
				continue
			}
			posted = true
			u, err := move(&id, c, origin, recovered)
			if err != nil {
				return err
			}
			left, cash, units = left-c, cash+c, units+u
		}
		return decide(cash, units)
	})
	return posted && err == nil, err
}

// reversalPath is how a reversal of a paid lot posts.
type reversalPath int

const (
	// reversalInBooks: the units to units_refunded, the cash in the general
	// ledger.
	reversalInBooks reversalPath = iota
	// reversalLegacy: a lot from before the credit books keeps its cash legs
	// in the units book.
	reversalLegacy
	// reversalRefused: a pack of a merchant's unit the general ledger never
	// booked has nowhere to keep its cash; the units book holds no money.
	reversalRefused
)

func reversalPathOf(l books.CreditLot) reversalPath {
	switch {
	case l.Booked():
		return reversalInBooks
	case l.PaidCurrency != l.Unit:
		return reversalRefused
	}
	return reversalLegacy
}

// UnbookedUnitPackFindingType is the finding a reversal of a pack of a
// merchant's unit raises when the general ledger never booked the pack.
const UnbookedUnitPackFindingType = "books.credit.unbooked_unit_pack"

// refuseUnbookedUnitPack records, rather than applies, a reversal of a pack
// of a merchant's unit the general ledger never booked (its payment's cash
// did not apply: books.credit.unapplied).
func refuseUnbookedUnitPack(ctx context.Context, q *gen.Queries, g gen.BillingCreditGrant, paymentID, reversalID uuid.UUID) error {
	raw, err := json.Marshal(map[string]any{"credit_grant_id": g.ID, "payment_id": paymentID, "reversal_id": reversalID, "unit": g.Unit, "paid_currency": g.PaidCurrency})
	if err != nil {
		return err
	}
	action := "A reversal of this pack of a merchant's unit waits: the general ledger never booked the pack's price, and its unit holds no money to return. Book the lot's price (see books.credit.unapplied), then the reversal applies."
	_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID: g.MerchantID, FindingType: UnbookedUnitPackFindingType, SubjectKey: "credit_grant:" + g.ID.String(),
		Severity: "high", Status: "requires_review", RecommendedAction: &action, Evidence: raw,
	})
	if err != nil {
		return fmt.Errorf("raise %s: %w", UnbookedUnitPackFindingType, err)
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

// postLegacyReversal posts a reversal of a lot from before the credit books
// as the units book did then: every leg through units_refund_clearing, the
// cash beside them and the difference between face and cash to promotion or
// revenue.
func postLegacyReversal(ctx context.Context, l *ledger.Ledger, g gen.BillingCreditGrant, reversalID uuid.UUID, reversesID *uuid.UUID, restoring bool, cashDelta, faceDelta int64, units unitsMoved, source, cashSource, lossSource string) error {
	if g.PaidCurrency == nil || *g.PaidCurrency != g.Unit {
		return fmt.Errorf("credit lot %s: its cash has no leg in its unit %s", g.ID, g.Unit)
	}
	post := func(account ledger.AccountType, amount int64, kind ledger.TransferType, incoming bool, label string) error {
		from, to := account, ledger.UnitsRefundClearing
		if !incoming {
			from, to = to, from
		}
		return postUnits(ctx, l, g, reversalID, from, to, amount, kind, label)
	}
	balanceType, cashType := ledger.CreditRefund, ledger.CreditRefundCash
	if restoring {
		balanceType, cashType = ledger.CreditRefundRestore, ledger.CreditRefundCashRestore
	}
	if err := post(ledger.CustomerUnits, units.withdrawn, balanceType, !restoring, source); err != nil {
		return err
	}
	if err := post(units.retiredAccount, units.retired, ledger.CreditRefundFunding, false, "retired_"+source); err != nil {
		return err
	}
	expiredSource, revokedSource, repaidSource := "expired_credit", "revoked_credit", "repaid_owed"
	if restoring {
		expiredSource += "_restore:" + reversesID.String()
		revokedSource += "_restore:" + reversesID.String()
		repaidSource += "_restore:" + reversesID.String()
	}
	if err := post(ledger.UnitsExpired, units.expired, ledger.CreditRefundFunding, !restoring, expiredSource); err != nil {
		return err
	}
	if err := post(ledger.UnitsRevoked, units.revoked, ledger.CreditRefundFunding, !restoring, revokedSource); err != nil {
		return err
	}
	if err := post(ledger.CustomerOwed, units.repaid, ledger.CreditRefundFunding, !restoring, repaidSource); err != nil {
		return err
	}
	if err := post(ledger.UnitsRefundLoss, units.consumed, ledger.CreditRefundFunding, !restoring, lossSource); err != nil {
		return err
	}
	if err := post(ledger.UnitsSold, cashDelta, cashType, restoring, cashSource); err != nil {
		return err
	}
	if faceDelta > cashDelta {
		return post(ledger.UnitsGranted, faceDelta-cashDelta, ledger.CreditRefundFunding, restoring, "promotion")
	}
	return post(ledger.UnitsRedeemed, cashDelta-faceDelta, ledger.CreditRefundFunding, !restoring, "revenue")
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
