package reconcile

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"
)

func TestNMICurrencyInvoiceReceiptMatchesBulkObservation(t *testing.T) {
	for _, tc := range []struct {
		currency, amount string
		minor            int64
	}{{"USD", "500.00", 50_000}, {"JPY", "500.00", 500}, {"EUR", "1.23", 123}} {
		t.Run(tc.currency, func(t *testing.T) {
			client := newNMITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprintf(w, `{%q:[],"has_more":false}`, strings.TrimPrefix(r.URL.Path, "/"))
					return
				}
				fmt.Fprintf(w, `<nm_response><transaction><transaction_id>paid-invoice</transaction_id><currency>%s</currency><action><action_type>sale</action_type><amount>%s</amount><date>20261008000000</date><success>1</success></action></transaction></nm_response>`, tc.currency, tc.amount)
			}))
			snapshot, err := NewNMIFetcher(client).Fetch(t.Context(), FetchParams{})
			require.NoError(t, err)
			require.Len(t, snapshot.Transactions, 1)
			require.Equal(t, tc.minor, snapshot.Transactions[0].AmountCents, "bulk and exact receipt readers use the same rail units")
			invoice := uuid.New()
			known := LocalPayment{ID: uuid.New(), CustomerID: uuid.New(), InvoiceID: &invoice, TransactionID: "paid-invoice", AmountCents: tc.minor, Currency: tc.currency, Status: "settled"}
			require.Empty(t, diffProvider(ProviderNMI, snapshot, &LocalState{}, []LocalPayment{known}, time.Now(), diffOptions{}), "a valid receipt must not create a conflict that holds freshness forever")
		})
	}
}

func TestNMICurrencyScheduleComparesAtBoundPriceScale(t *testing.T) {
	for _, currency := range []string{"JPY", "USD"} {
		for _, planFallback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/plan=%t", currency, planFallback), func(t *testing.T) {
				priceID := uuid.New()
				native := int64(5_000_000) // 500 JPY
				if currency == "USD" {
					native = 500_000_000 // 500 USD
				}
				local := &LocalSubscription{ID: uuid.New(), PriceID: &priceID, Status: "active", CollectionPolicy: models.CollectionPolicyNMISchedule, RailSubscriptionID: "schedule"}
				idx := &localIndex{prices: []LocalPrice{{ID: priceID, Currency: currency, Amount: native}}}
				wire := nmi.V5Subscription{ID: "schedule", Amount: "500.00", NextBillingDate: time.Now().AddDate(0, 1, 0).Format("2006-01-02")}
				if planFallback {
					wire.Amount, wire.Plan = "", &nmi.V5Plan{PlanAmount: "500.00"}
				}
				client := newNMITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/subscriptions" {
						_ = json.NewEncoder(w).Encode(map[string]any{"subscriptions": []nmi.V5Subscription{wire}, "has_more": false})
						return
					}
					if r.Method == http.MethodGet {
						fmt.Fprint(w, `{"customers":[],"has_more":false}`)
						return
					}
					fmt.Fprint(w, `<nm_response/>`)
				}))
				snapshot, err := NewNMIFetcher(client).Fetch(t.Context(), FetchParams{})
				require.NoError(t, err)
				require.Len(t, snapshot.Subscriptions, 1)
				remote := &snapshot.Subscriptions[0]
				require.Nil(t, compareScheduleTerms(ProviderNMI, idx, local, remote))
				idx.prices[0].Amount += 10_000 // one rail minor unit in either currency
				require.NotNil(t, compareScheduleTerms(ProviderNMI, idx, local, remote), "real changed price must still be held")
			})
		}
	}
}

func TestNMICurrencyQualificationRefusesFractionalYenOrUnknownScale(t *testing.T) {
	for _, tc := range []struct{ currency, amount string }{{"JPY", "500.01"}, {"XYZ", "500.00"}, {"USD", "1.001"}} {
		transaction := nmi.QueryTransaction{TransactionID: "money", Currency: tc.currency, Actions: []nmi.QueryAction{{ActionType: "sale", Amount: tc.amount, Success: "1", Date: "20261008000000"}}}
		require.Error(t, qualifyNMITransaction(transaction), "%s %s cannot become applied coverage", tc.amount, tc.currency)
	}
}
