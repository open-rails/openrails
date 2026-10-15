package reconcile

import (
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// NMI refund transactions must identify both the receipt and its original payment.
func makeNMIRefund(t *RemoteTransaction, local map[string]*LocalPayment, now time.Time) (Finding, bool) {
	f := Finding{Provider: ProviderNMI, Type: FindingRefundUnrecorded, SubjectKey: t.TransactionID,
		Severity: SeverityHigh, Status: FindingStatusRequiresReview, RequiresAdmin: true,
		RemoteEvidence: remoteTxnEvidence(t)}
	if strings.TrimSpace(t.OriginalTransactionID) == "" || t.OriginalTransactionID == t.TransactionID {
		f.RecommendedAction = "NMI refund needs a separate provider refund ID and an explicit original payment link; review the provider evidence"
		return f, true
	}
	original := local[t.OriginalTransactionID]
	if original == nil {
		f.RecommendedAction = "NMI refund references no locally-known original payment"
		return f, true
	}
	f.LocalEvidence = map[string]any{"payment_id": original.ID.String(), "transaction_id": original.TransactionID}
	minor, err := t.minorIn(t.Currency)
	if minor < 0 {
		minor = -minor
	}
	amount, convertErr := moneyutil.RailMinorToNative(t.Currency, minor)
	if err != nil || convertErr != nil || minor <= 0 || amount > original.AmountNative ||
		original.Status != "succeeded" || original.RefundedPaymentID != nil ||
		!strings.EqualFold(t.Currency, original.Currency) || t.OccurredAt.IsZero() ||
		t.OccurredAt.Before(original.PurchasedAt) || t.OccurredAt.After(now) {
		f.RecommendedAction = "NMI refund amount, currency, date or original payment facts need review"
		return f, true
	}
	if prior := local[t.TransactionID]; prior != nil {
		if prior.RefundedPaymentID != nil && *prior.RefundedPaymentID == original.ID &&
			prior.CustomerID == original.CustomerID && prior.AmountNative == -amount &&
			strings.EqualFold(prior.Currency, original.Currency) && prior.Status == "succeeded" &&
			prior.ReversalKind == payments.ReversalRefund && prior.PurchasedAt.Equal(t.OccurredAt.UTC().Truncate(time.Microsecond)) {
			return Finding{}, false
		}
		f.RecommendedAction = "NMI refund ID is already recorded with different payment facts"
		return f, true
	}
	if original.UsageOrderID != nil {
		f.RecommendedAction = "refund belongs to a usage order; reconcile its order and ledger"
		return f, true
	}
	f.Status, f.RequiresAdmin = FindingStatusReconcileRequired, false
	f.RecommendedAction = "record the observed NMI refund against its original payment"
	f.Apply = &ApplyAction{RecordRefund: &RecordRefundAction{
		Rail: original.Rail, TransactionID: t.TransactionID, AmountCents: int64(minor),
		Currency: original.Currency, PurchasedAt: t.OccurredAt, RefundedPaymentID: &original.ID,
		SubscriptionID: original.SubscriptionID, CustomerID: original.CustomerID,
		Metadata: map[string]any{"reconcile_refund_backfill": true, "provider": "nmi"},
	}}
	return f, true
}
