package purchasedcredits

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money/ledger"
)

// ValidateRefund reserves no value by itself. The caller must bind this service
// to the transaction that inserts its pending refund payment; that durable row
// then participates in both admission and spending's existing held total.
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
		balance, held, err := s.lockBalance(ctx, q, mid.UUID(), grant.CustomerID, stringValue(grant.Currency))
		if err != nil {
			return err
		}
		paid, err := fundingAmount(grant)
		if err != nil {
			return err
		}
		if additional <= 0 || additional > paid {
			return fmt.Errorf("invalid credit refund amount: %w", billing.ErrInvalid)
		}
		terminated, err := q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: mid.UUID(), GrantID: grant.ID})
		if err != nil {
			return err
		}
		if terminated || (grant.EndsAt != nil && !grant.EndsAt.After(s.now())) {
			return fmt.Errorf("credit grant is no longer refundable: %w", billing.ErrConflict)
		}
		remaining, err := q.GetCreditLotRemaining(ctx, gen.GetCreditLotRemainingParams{MerchantID: mid.UUID(), GrantID: grant.ID})
		if err != nil {
			return err
		}
		pending, err := q.GetPurchasedCreditPendingRefunds(ctx, gen.GetPurchasedCreditPendingRefundsParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if err != nil {
			return err
		}
		if pending > paid-additional {
			return fmt.Errorf("pending refunds exceed purchased credit funding: %w", billing.ErrConflict)
		}
		reserved := proportional(intValue(grant.Amount), pending, paid, true)
		requested := proportional(intValue(grant.Amount), additional, paid, true)
		if requested > remaining-reserved || requested > balance-held {
			return fmt.Errorf("refund requires unused and unreserved purchased credit: %w", billing.ErrConflict)
		}
		return nil
	})
}

// ApplyReversal posts one completed negative payment, or a positive recovery
// explicitly linked to that reversal. Keeping this identity prevents a dispute
// win from restoring credits withdrawn by an unrelated voluntary refund.
func (s *Service) ApplyReversal(ctx context.Context, paymentID, reversalID uuid.UUID, reversesID *uuid.UUID) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		g, err := q.GetPurchasedCreditGrant(ctx, gen.GetPurchasedCreditGrantParams{MerchantID: mid.UUID(), PaymentID: paymentID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		currency := stringValue(g.Currency)
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
		committed, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), GrantID: g.ID, ReversalID: reversalID.String()})
		if err != nil || committed.Legs > 0 {
			return err
		}
		paid, err := fundingAmount(g)
		if err != nil {
			return err
		}
		net, err := q.GetPurchasedCreditRefundState(ctx, gen.GetPurchasedCreditRefundStateParams{MerchantID: mid.UUID(), GrantID: g.ID})
		if err != nil {
			return err
		}
		// Finalization must not leave the unrefunded remainder of an expired
		// source available until the next sweep. Retired balances remain bound
		// to the same grant and can fund this refund below.
		grantLedger := grants.New(q, mid.UUID())
		grantLedger.SetClock(s.now)
		if _, err := grantLedger.ExpireLapsed(ctx, g.CustomerID, currency); err != nil {
			return err
		}
		remaining, err := q.GetCreditLotRemaining(ctx, gen.GetCreditLotRemainingParams{MerchantID: mid.UUID(), GrantID: g.ID})
		if err != nil {
			return err
		}
		face := intValue(g.Amount)
		restoring := reversal.Amount > 0
		cashDelta := reversal.Amount
		var faceDelta, withdrawn, retired, expired, revoked, repaid int64
		retiredAccount := ledger.RevokedCredits
		source := "credit"
		cashSource := "cash"
		lossSource := "consumed_credit"
		if !restoring {
			if reversesID != nil || cashDelta == (-1<<63) {
				return fmt.Errorf("invalid negative credit reversal")
			}
			cashDelta = -cashDelta
			if cashDelta > paid-net.Cash {
				return fmt.Errorf("credit refunds exceed original payment")
			}
			faceDelta = proportional(face, net.Cash+cashDelta, paid, false) - proportional(face, net.Cash, paid, false)
			withdrawn = min(max(remaining, 0), faceDelta)
			retiredBalance, err := q.GetPurchasedCreditRetiredBalance(ctx, gen.GetPurchasedCreditRetiredBalanceParams{MerchantID: mid.UUID(), GrantID: g.ID})
			if err != nil {
				return err
			}
			expired = min(max(retiredBalance.Expired, 0), faceDelta-withdrawn)
			revoked = min(max(retiredBalance.Revoked, 0), faceDelta-withdrawn-expired)
			// Value that repaid owed is owed again when its payment is reversed.
			standing, err := q.GetPurchasedCreditRepaidOwed(ctx, gen.GetPurchasedCreditRepaidOwedParams{MerchantID: mid.UUID(), GrantID: g.ID})
			if err != nil {
				return err
			}
			repaid = min(max(standing, 0), faceDelta-withdrawn-expired-revoked)
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
			origin, err := q.GetPurchasedCreditReversalState(ctx, gen.GetPurchasedCreditReversalStateParams{MerchantID: mid.UUID(), GrantID: g.ID, ReversalID: reversesID.String()})
			if err != nil {
				return err
			}
			recovered, err := q.GetPurchasedCreditRecoveryTotal(ctx, gen.GetPurchasedCreditRecoveryTotalParams{MerchantID: mid.UUID(), GrantID: g.ID, ReversalID: reversesID.String()})
			if err != nil {
				return err
			}
			if origin.Cash <= 0 || cashDelta > origin.Cash-recovered {
				return fmt.Errorf("credit recovery exceeds its recorded reversal")
			}
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
			terminated, err := q.IsGrantTerminated(ctx, gen.IsGrantTerminatedParams{MerchantID: mid.UUID(), GrantID: g.ID})
			if err != nil {
				return err
			}
			pastExpiry := g.EndsAt != nil && !g.EndsAt.After(s.now())
			if terminated || pastExpiry {
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
			_, err = l.Apply(ctx, ledger.Transfer{Debit: debit, Credit: credit, Amount: amount, Currency: currency, Type: kind, Coord: ledger.Coord{Operation: "purchased_credit_refund", Source: label, SourceID: reversalID.String()}, GrantID: &g.ID, Customer: customer})
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
}

func fundingAmount(g gen.BillingGrant) (int64, error) {
	var spec grants.Spec
	if err := json.Unmarshal(g.SpecSnapshot, &spec); err != nil {
		return 0, err
	}
	if spec.Deposit == nil || spec.Deposit.PaidAmount == nil || *spec.Deposit.PaidAmount <= 0 {
		return 0, fmt.Errorf("purchased credit lacks immutable payment funding")
	}
	return *spec.Deposit.PaidAmount, nil
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
