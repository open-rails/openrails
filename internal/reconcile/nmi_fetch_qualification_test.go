package reconcile

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNMIFetchRefusesIncompleteRosterPages(t *testing.T) {
	for _, resource := range []string{"customers", "subscriptions"} {
		for _, body := range []string{"", "null", "{}", fmt.Sprintf(`{"%s":null,"has_more":false}`, resource), fmt.Sprintf(`{"%s":[{}],"has_more":false}`, resource), fmt.Sprintf(`{"%s":[]}`, resource), fmt.Sprintf(`{"%s":[],"has_more":true}`, resource), fmt.Sprintf(`{"%s":[],"has_more":true,"next_cursor":""}`, resource)} {
			t.Run(resource+"/"+body, func(t *testing.T) {
				client := newNMITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/"+resource {
						fmt.Fprint(w, body)
						return
					}
					if r.Method == http.MethodGet {
						fmt.Fprintf(w, `{%q:[],"has_more":false,"next_cursor":null}`, strings.TrimPrefix(r.URL.Path, "/"))
						return
					}
					fmt.Fprint(w, `<nm_response/>`)
				}))
				snapshot, err := NewNMIFetcher(client).Fetch(t.Context(), FetchParams{})
				require.Error(t, err)
				require.Nil(t, snapshot, "an incomplete page cannot assert a completed snapshot")
			})
		}
	}
}

func TestNMIFetchEmptyAndAdditiveRosterFields(t *testing.T) {
	client := newNMITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprintf(w, `{%q:[],"has_more":false,"next_cursor":null,"new_optional_field":{"anything":1}}`, strings.TrimPrefix(r.URL.Path, "/"))
			return
		}
		fmt.Fprint(w, `<nm_response/>`)
	}))
	snapshot, err := NewNMIFetcher(client).Fetch(t.Context(), FetchParams{})
	require.NoError(t, err)
	require.Empty(t, snapshot.PaymentMethods)
	require.Empty(t, snapshot.Subscriptions)
	require.False(t, snapshot.Coverage.SubscriptionsExhaustive, "an empty roster never establishes destructive absence")
	require.True(t, snapshot.Coverage.TransactionsPaginatedComplete)
}

func TestNMIFetchRefusesMalformedMoneyFacts(t *testing.T) {
	valid := `<nm_response><transaction><transaction_id>one</transaction_id><currency>USD</currency><action><action_type>sale</action_type><amount>9.99</amount><date>20260101000000</date><success>1</success></action></transaction></nm_response>`
	for _, body := range []string{
		strings.Replace(valid, "<transaction_id>one</transaction_id>", "", 1),
		strings.Replace(valid, "9.99", "nine", 1),
		strings.Replace(valid, "<amount>9.99</amount>", "", 1),
		strings.Replace(valid, "20260101000000", "bad", 1),
		strings.Replace(valid, "<success>1</success>", "<success>unknown</success>", 1),
		strings.Replace(valid, "<currency>USD</currency>", "", 1),
	} {
		t.Run(body, func(t *testing.T) {
			client := newNMITestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fmt.Fprintf(w, `{%q:[],"has_more":false}`, strings.TrimPrefix(r.URL.Path, "/"))
					return
				}
				fmt.Fprint(w, body)
			}))
			snapshot, err := NewNMIFetcher(client).Fetch(t.Context(), FetchParams{})
			require.Error(t, err)
			require.Nil(t, snapshot)
		})
	}
}
