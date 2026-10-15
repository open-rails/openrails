package pspimport

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/stretchr/testify/require"
)

func TestHistoricalRefundOwnership(t *testing.T) {
	t.Parallel()
	customer := uuid.New()
	payment := transactionData{TransactionID: "12682585392", Amount: 4_230_000, Currency: "USD"}
	good := importRefund{record: gen.BillingPspImportRecord{ID: uuid.New(), CustomerID: &customer, State: string(billing.PSPImportLinked)}, data: transactionData{TransactionID: "12682586568", Amount: -1_010_000, Currency: "USD", OccurredAt: time.Now().UTC()}}
	for _, tc := range []struct {
		name     string
		change   func(*importRefund)
		accepted bool
	}{
		{"linked refund", func(*importRefund) {}, true},
		{"previously imported", func(r *importRefund) { r.record.State = string(billing.PSPImportImported) }, true},
		{"unmatched", func(r *importRefund) { r.record.CustomerID = nil }, false},
		{"another customer", func(r *importRefund) { r.record.CustomerID = new(uuid.New()) }, false},
		{"conflicted", func(r *importRefund) { r.record.State = string(billing.PSPImportConflict) }, false},
		{"different currency", func(r *importRefund) { r.data.Currency = "EUR" }, false},
		{"combined action identity", func(r *importRefund) { r.data.TransactionID = payment.TransactionID }, false},
		{"no identity", func(r *importRefund) { r.data.TransactionID = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refund := good
			tc.change(&refund)
			l := lander{refunds: map[string][]importRefund{payment.TransactionID: {refund}}}
			receipts, ids, reason := l.historicalRefunds(customer, payment)
			if tc.accepted {
				require.Empty(t, reason)
				require.Equal(t, []uuid.UUID{refund.record.ID}, ids)
				require.Len(t, receipts, 1)
				require.Equal(t, int64(1_010_000), receipts[0].Amount)
			} else {
				require.NotEmpty(t, reason)
				require.Empty(t, receipts)
				require.Empty(t, ids)
			}
		})
	}
}
