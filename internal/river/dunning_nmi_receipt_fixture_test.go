//go:build integration

package riverjobs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
)

// withRebillReceipts adds the Query API and exact v5 read to classic scripted
// rebill fixtures. It records only approved sales, keeps the classic response
// unchanged, and uses the fixture's provider plan terms (recurring writes carry
// no amount or currency). Verification reads never consume a scripted write.
func withRebillReceipts(next http.Handler, amount, currency string) http.Handler {
	type sale struct{ transaction, order, vault string }
	var mu sync.Mutex
	orders, transactions := map[string]sale{}, map[string]sale{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("report_type") == "transaction" {
			order := r.Form.Get("order_id")
			mu.Lock()
			receipt, found := orders[order]
			mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			if !found {
				fmt.Fprint(w, "<nm_response></nm_response>")
				return
			}
			fmt.Fprintf(w, `<nm_response><transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><action><action_type>sale</action_type><success>1</success></action></transaction></nm_response>`, receipt.transaction, receipt.order)
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/payments/") {
			transaction := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			mu.Lock()
			receipt, found := transactions[transaction]
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if !found {
				w.WriteHeader(http.StatusNotFound)
				fmt.Fprint(w, `{"type":"notFound","error_code":"E_NOT_FOUND"}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "transaction", "id": receipt.transaction, "response": "1", "response_code": "100",
				"amount": amount, "currency": currency, "customer_vault_id": receipt.vault,
				"actions": []map[string]any{{"type": "sale", "amount": amount, "success": true}},
			})
			return
		}
		if r.Form.Get("type") != "sale" {
			next.ServeHTTP(w, r)
			return
		}
		response := httptest.NewRecorder()
		next.ServeHTTP(response, r)
		values, _ := url.ParseQuery(response.Body.String())
		if response.Code == http.StatusOK && values.Get("response") == "1" && values.Get("transactionid") != "" {
			receipt := sale{values.Get("transactionid"), r.Form.Get("orderid"), r.Form.Get("customer_vault_id")}
			mu.Lock()
			orders[receipt.order], transactions[receipt.transaction] = receipt, receipt
			mu.Unlock()
		}
		for key, values := range response.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(response.Code)
		_, _ = w.Write(response.Body.Bytes())
	})
}
