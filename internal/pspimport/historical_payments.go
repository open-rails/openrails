package pspimport

import (
	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
)

type importRefund struct {
	record gen.BillingPspImportRecord
	data   transactionData
}

// Refund evidence comes from the whole import, not only this customer's
// linked records. An unresolved refund must never let its charge land alone.
func (l *lander) historicalRefunds(customer uuid.UUID, payment transactionData) ([]billing.DeclaredHistoricalRefund, []uuid.UUID, string) {
	rows := l.refunds[payment.TransactionID]
	var refunds []billing.DeclaredHistoricalRefund
	var ids []uuid.UUID
	for _, r := range rows {
		if r.record.CustomerID == nil || *r.record.CustomerID != customer ||
			r.record.State != string(billing.PSPImportLinked) && r.record.State != string(billing.PSPImportImported) {
			return nil, nil, "this payment has a recorded refund that is not linked to the same customer"
		}
		if r.data.TransactionID == "" || r.data.TransactionID == payment.TransactionID {
			return nil, nil, "this payment has a recorded refund without a separate provider refund ID"
		}
		if r.data.Currency != payment.Currency {
			return nil, nil, "this payment has a recorded refund in a different currency"
		}
		// NMI reports refunds as negative amounts; the import contract uses positive amounts.
		amount := r.data.Amount
		if amount < 0 {
			amount = -amount
		}
		refunds = append(refunds, billing.DeclaredHistoricalRefund{TransactionID: r.data.TransactionID, Amount: amount, RefundedAt: r.data.OccurredAt})
		ids = append(ids, r.record.ID)
	}
	return refunds, ids, ""
}
