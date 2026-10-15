package pspimport

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

func TestOrphanedHistoryRequiresSuccessfulStandalonePayment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind                 string
		success, invoice, accepted bool
		known, refunded            bool
		rail                       string
		ref                        string
	}{
		{name: "Stripe", rail: "stripe", kind: "sale", success: true},
		{name: "CCBill", rail: "ccbill", kind: "sale", success: true},
		{name: "refunded sale", kind: "sale", success: true, refunded: true},
		{name: "sale", kind: "sale", success: true, accepted: true},
		{name: "deleted subscription", kind: "sale", success: true, accepted: true, ref: "deleted"},
		{name: "subscription linked elsewhere or blocked", kind: "sale", success: true, known: true, ref: "existing"},
		{name: "failed sale", kind: "sale"},
		{name: "decline", kind: "decline"},
		{name: "refund", kind: "refund", success: true},
		{name: "chargeback", kind: "chargeback", success: true},
		{name: "invoice", kind: "sale", success: true, invoice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, customer := uuid.New(), uuid.New()
			data := transactionData{TransactionID: "12345678", Type: tc.kind, Success: tc.success, Amount: 1_000_000, Currency: "USD", OccurredAt: time.Now().UTC()}
			if tc.invoice {
				data.StripeInvoice = &billing.DeclaredStripeInvoice{ID: "in_123"}
			}
			raw, err := json.Marshal(data)
			require.NoError(t, err)
			rail := tc.rail
			if rail == "" {
				rail = "nmi"
			}
			l := lander{scope: merchants.PSPScope{ID: uuid.New(), Rail: rail}, asOf: time.Now().UTC(), refunded: map[string]bool{data.TransactionID: tc.refunded}}
			d, err := l.declare([]customerGroup{{customer: customer, records: []gen.BillingPspImportRecord{{ID: id, Kind: string(billing.PSPImportTransaction), State: string(billing.PSPImportLinked), SubscriptionRef: &tc.ref, Data: raw}}}}, map[string]bool{tc.ref: tc.known})
			require.NoError(t, err)
			require.Empty(t, d.book.Subscriptions)
			require.Empty(t, d.book.Purchases)
			if tc.accepted {
				require.Len(t, d.book.HistoricalPayments, 1)
				require.Equal(t, data.TransactionID, d.book.HistoricalPayments[0].TransactionID)
			} else {
				require.Empty(t, d.book.HistoricalPayments)
				require.Equal(t, billing.PSPImportBlocked, d.outcomes[id].state)
				require.NotEmpty(t, d.outcomes[id].reason)
			}
		})
	}
}
