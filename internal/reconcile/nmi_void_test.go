package reconcile

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
)

func TestNMIVoidCannotUsePaymentStatusAsReversalProof(t *testing.T) {
	for _, condition := range []string{"canceled", "complete"} {
		t.Run(condition, func(t *testing.T) {
			void := ""
			if condition == "complete" {
				void = `<action><action_type>void</action_type><amount>9.99</amount><date>20261008000000</date><success>1</success></action>`
			}
			client := newNMITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprintf(w, `{%q:[],"has_more":false}`, strings.TrimPrefix(r.URL.Path, "/"))
					return
				}
				fmt.Fprintf(w, `<nm_response><transaction><transaction_id>voided-sale</transaction_id><condition>%s</condition><currency>USD</currency><action><action_type>sale</action_type><amount>9.99</amount><date>20261007000000</date><success>1</success></action>%s</transaction></nm_response>`, condition, void)
			}))
			snapshot, err := NewNMIFetcher(client).Fetch(t.Context(), FetchParams{})
			require.NoError(t, err)
			require.Len(t, snapshot.Transactions, 1, "a void must not disappear from completed financial observation")
			observation := snapshot.Transactions[0]
			require.Equal(t, TransactionType("void"), observation.Type)
			require.Equal(t, "voided-sale", observation.TransactionID)
			require.Contains(t, string(observation.Raw), condition)
			for _, localState := range []string{"unrecorded", "payment", "invoice", "partially_refunded", "refunded", "refunded_wrong_amount"} {
				var payments []LocalPayment
				if localState != "unrecorded" {
					payment := LocalPayment{ID: uuid.New(), TransactionID: "voided-sale", AmountCents: 999, Currency: "USD", Status: "completed"}
					if localState == "invoice" {
						invoice := uuid.New()
						payment.InvoiceID, payment.Status = &invoice, "settled"
					}
					if localState == "partially_refunded" {
						payment.Status = "partially_refunded"
					}
					if strings.HasPrefix(localState, "refunded") {
						payment.Status = "refunded"
					}
					if localState == "refunded_wrong_amount" {
						payment.AmountCents++
					}
					payments = append(payments, payment)
				}
				findings := diffProvider(ProviderNMI, snapshot, &LocalState{}, payments, time.Now(), diffOptions{})
				// PGLocalWriter's same-ID partial refund can produce status
				// "refunded" while the original amount remains unchanged.
				// None of these status values proves a full reversal allocation.
				require.Len(t, findings, 1, localState)
				require.Equal(t, FindingReversalUnlinked, findings[0].Type)
				require.Equal(t, FindingStatusRequiresReview, findings[0].Status)
				require.Nil(t, findings[0].Apply, "a void observation is not authority to invent a refund or payment")
			}
		})
	}
}

func TestNMIVoidedAuthorizationOrZeroSaleIsNotMoney(t *testing.T) {
	for _, kind := range []string{"auth", "validate", "sale"} {
		transaction := nmi.QueryTransaction{TransactionID: "zero", Condition: "canceled", Currency: "USD", Actions: []nmi.QueryAction{{ActionType: kind, Amount: "0.00", Success: "1", Date: "20261007000000"}}}
		for _, observation := range normalizeNMITransaction(transaction) {
			require.NotEqual(t, TransactionType("void"), observation.Type)
		}
	}
}
