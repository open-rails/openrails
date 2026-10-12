package payment

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/solana/settlement"
)

// SettleSolanaTransfer is the only path that records a transfer on a Solana
// Pay purchase reference, for the poller and the client confirm alike. Under
// the reference's row lock it classifies the transfer, credits the purchase
// when the transfer settles it, and records the signature exactly once:
// credited, review (money to refund or decide on), duplicate (the transfer
// already settled another reference) or ignored. A replay returns the first
// receipt. The only errors returned are ones worth retrying; anything that
// would fail the same way again is recorded for review instead, so one bad
// transfer never blocks the reference.
func (s *OrderAttemptService) SettleSolanaTransfer(ctx context.Context, reference string, t solanamodule.ObservedTransfer) (*solanamodule.Receipt, error) {
	if s.db == nil {
		return nil, errors.New("purchase settlement database unavailable")
	}
	var out *solanamodule.Receipt
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		ledger := solanamodule.NewPayLedger(d)
		ref, err := ledger.Lock(ctx, reference)
		if err != nil {
			return err
		}
		if ref.Kind != string(solanamodule.ReferencePurchase) {
			return fmt.Errorf("solana reference %s is not a purchase", reference)
		}
		if seen, err := ledger.Receipt(ctx, reference, t.Signature); err != nil || seen != nil {
			out = seen
			return err
		}
		session, err := NewOrderAttemptRepo(d).GetByID(ctx, ref.OrderAttemptID)
		if err != nil {
			return err
		}
		now := s.now()
		receipt := solanamodule.Receipt{
			Reference: reference, Signature: t.Signature, SessionID: session.ID,
			Recipient: getStringField(session.RailState, "recipient"), TokenMint: getStringField(session.RailState, "token_mint"),
			ExpectedAmount: getUint64Field(session.RailState, "token_amount"),
			ReceivedAmount: t.Amount, Payer: t.Payer, LandedAt: t.LandedAt,
		}
		open := session.Status == models.OrderAttemptStatusCreated || session.Status == models.OrderAttemptStatusRequiresAction || session.Status == models.OrderAttemptStatusExpired
		if open && session.OrderID != nil {
			// A transfer pays the order only on its current attempt while it
			// takes payment; any other is money to review.
			if open, err = solanaOrderLive(ctx, d, session); err != nil {
				return err
			}
		}
		receipt.Disposition, receipt.ReviewReason = solanamodule.Decide(ref, open, receipt.ExpectedAmount, t, now)
		if receipt.Disposition == solanamodule.Credited {
			err = s.creditSolanaTransfer(ctx, d, session, &receipt, t, now)
		} else {
			err = ledger.Record(ctx, receipt, now)
		}
		switch {
		case errors.Is(err, solanamodule.ErrSignatureClaimed):
			receipt.Disposition, receipt.ReviewReason, receipt.PaymentID = solanamodule.Duplicate, solanamodule.ReasonClaimedElsewhere, nil
			err = ledger.Record(ctx, receipt, now)
		case err != nil && !transientSettleError(err):
			receipt.Disposition, receipt.ReviewReason, receipt.PaymentID = solanamodule.Review, solanamodule.ReasonSettleFailed, nil
			if err = ledger.Record(ctx, receipt, now); errors.Is(err, solanamodule.ErrSignatureClaimed) {
				receipt.Disposition, receipt.ReviewReason = solanamodule.Duplicate, solanamodule.ReasonClaimedElsewhere
				err = ledger.Record(ctx, receipt, now)
			}
		}
		if err != nil {
			return err
		}
		if err := ledger.RaiseReview(ctx, receipt, session.CustomerID.String(), now); err != nil {
			return err
		}
		out = &receipt
		return nil
	})
	return out, err
}

// creditSolanaTransfer credits the purchase from its accepted terms, confirms
// the reference and records the credit, all inside a savepoint: a transfer
// already recorded elsewhere, or a purchase that refuses, leaves nothing.
func (s *OrderAttemptService) creditSolanaTransfer(ctx context.Context, d *db.DB, session *models.OrderAttempt, receipt *solanamodule.Receipt, t solanamodule.ObservedTransfer, now time.Time) error {
	return d.RunInTx(ctx, func(ctx context.Context, sp pgx.Tx) error {
		inner := s.db.NewWithPgxTx(sp)
		// One landed transfer settles at most one purchase, across merchants
		// and PSPs, and a purchase at most one transfer.
		if err := settlement.ClaimOrder(ctx, inner, session.ID, t.Signature); errors.Is(err, settlement.ErrClaimed) {
			return solanamodule.ErrSignatureClaimed
		} else if err != nil {
			return err
		}
		paymentID, err := s.creditSolanaOrder(ctx, inner, session, receipt.Reference, t)
		// Under the reference lock, the transfer's payment or purchase
		// transaction id already taken means it settled something else.
		var pgErr *pgconn.PgError
		if errors.Is(err, ErrPaymentTransactionTaken) || errors.As(err, &pgErr) && pgErr.Code == "23505" && transactionIDConstraints[pgErr.ConstraintName] {
			return solanamodule.ErrSignatureClaimed
		}
		if err != nil {
			return err
		}
		ledger := solanamodule.NewPayLedger(inner)
		if confirmed, err := ledger.Confirm(ctx, receipt.Reference, t.Signature, now); err != nil || !confirmed {
			return errors.Join(err, fmt.Errorf("solana reference %s left pending state under lock", receipt.Reference))
		}
		receipt.PaymentID = &paymentID
		return ledger.Record(ctx, *receipt, now)
	})
}

// transactionIDConstraints are the unique indexes a transfer's transaction id
// collides with when it already settled another payment or purchase.
var transactionIDConstraints = map[string]bool{
	"payments_channel_transaction_id_key":      true,
	"payments_psp_id_transaction_id_key":       true,
	"order_attempts_psp_id_transaction_id_key": true,
	"order_attempts_transaction_id_key":        true,
}

// transientSettleError is a failure another attempt can get past: the
// connection, the database or the caller gave out, or a concurrent writer won
// a lock or a race (a retry then sees its result). Every other error repeats.
func transientSettleError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err) || pgconn.SafeToRetry(err) {
		return true
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505", pgErr.Code == "55P03", pgErr.Code == "55006":
			return true
		case len(pgErr.Code) == 5:
			switch pgErr.Code[:2] {
			case "08", "40", "53", "57", "58":
				return true
			}
		}
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// ResolveSolanaPayReview closes a review receipt once an operator refunded
// or otherwise settled its money.
func (s *OrderAttemptService) ResolveSolanaPayReview(ctx context.Context, signature, resolution string) error {
	resolved, err := solanamodule.NewPayLedger(s.db).Resolve(ctx, strings.TrimSpace(signature), resolution, s.now())
	if err != nil {
		return err
	}
	if !resolved {
		return fmt.Errorf("%w: no open Solana Pay review for signature %s", ErrOrderAttemptNotFound, signature)
	}
	return nil
}
