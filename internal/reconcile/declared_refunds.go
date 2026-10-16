package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// The caller holds the customer and subscription locks and writes the charge
// in this same transaction. A conflict must roll back the entire receipt group.
func recordDeclaredRefunds(ctx context.Context, d *db.DB, sub *models.Subscription, charge RemoteTransaction, refunds []RemoteTransaction, now time.Time) (map[string]DeclaredOutcome, error) {
	ctx = db.WithPSPID(ctx, sub.PspID)
	svc := payments.NewPaymentService(d, clockwork.NewFakeClockAt(now))
	original, err := svc.GetByPSPTransactionID(ctx, sub.Rail, charge.TransactionID)
	if err != nil {
		return nil, err
	}
	amount, err := charge.nativeIn(charge.Currency)
	if err != nil {
		return nil, err
	}
	if !charge.Success || !chargeAttempt(charge.Type) || amount <= 0 || charge.OccurredAt.IsZero() || charge.OccurredAt.After(now) ||
		original.CustomerID != sub.CustomerID || original.SubscriptionID == nil || *original.SubscriptionID != sub.ID ||
		original.PspID == nil || *original.PspID != sub.PspID || original.RefundedPaymentID != nil || original.Amount != amount ||
		original.Currency != charge.Currency || original.Status != payments.PaymentStatusSucceededValue || !original.PurchasedAt.Equal(charge.OccurredAt.UTC().Truncate(time.Microsecond)) {
		return nil, observed.ErrConflict
	}
	outcomes := map[string]DeclaredOutcome{}
	for _, r := range refunds {
		amount, err := r.nativeIn(r.Currency)
		if err != nil {
			return nil, err
		}
		if amount < 0 {
			amount = -amount
		}
		if !r.Success || r.Type != TransactionTypeRefund || r.OriginalTransactionID != original.TransactionID ||
			r.TransactionID == "" || r.TransactionID == original.TransactionID || r.SubscriptionID != sub.RailSubscriptionID ||
			r.Currency != original.Currency || amount <= 0 || r.OccurredAt.IsZero() || r.OccurredAt.Before(charge.OccurredAt) || r.OccurredAt.After(now) {
			return nil, fmt.Errorf("refund %s has incomplete or conflicting payment evidence", r.TransactionID)
		}
		prior, err := svc.GetByPSPTransactionID(ctx, sub.Rail, r.TransactionID)
		code := DeclaredAlreadyPresent
		if db.IsNotFound(err) {
			prior, err = svc.RecordObservedRefund(ctx, original.ID, r.TransactionID, amount, payments.ReversalRefund, r.OccurredAt, nil)
			code = DeclaredImported
		}
		if err != nil {
			return nil, err
		}
		if prior.RefundedPaymentID == nil || *prior.RefundedPaymentID != original.ID || prior.CustomerID != original.CustomerID ||
			prior.Amount != -amount || prior.Currency != original.Currency || prior.ReversalKind == nil || *prior.ReversalKind != payments.ReversalRefund ||
			prior.Status != payments.PaymentStatusSucceededValue || !prior.PurchasedAt.Equal(r.OccurredAt.UTC().Truncate(time.Microsecond)) {
			return nil, observed.ErrConflict
		}
		// Replaying a declared receipt follows the same access decision as a
		// repeated provider notification. The payment service preserves an
		// explicit admin refund's keep/revoke choice.
		if code == DeclaredAlreadyPresent {
			if _, err := svc.RecordObservedRefund(ctx, original.ID, r.TransactionID, amount, payments.ReversalRefund, r.OccurredAt, nil); err != nil {
				return nil, err
			}
		}
		outcomes[declaredReceiptKey(r.SubscriptionID, r.TransactionID)] = DeclaredOutcome{Code: code}
	}
	return outcomes, nil
}
