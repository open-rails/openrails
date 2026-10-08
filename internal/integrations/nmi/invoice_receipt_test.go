package nmi

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestInvoiceReceiptRequiresExactPositiveProviderHistory(t *testing.T) {
	for _, name := range []string{"qualified", "optional_query_fields_absent", "wrong_vault", "wrong_currency", "another_sale", "refund", "inexact_amount", "missing_time", "malformed_invoice", "unbound_history", "missing_exact_read", "v5_refund"} {
		t.Run(name, func(t *testing.T) {
			invoice, order := uuid.New(), uuid.New()
			description := "invoice " + invoice.String()
			if name == "malformed_invoice" {
				description = "invoice not-a-uuid"
			}
			txn := strings.Replace(saleTxn("txn", order.String(), "1", "100", "20261008120000"), "<currency>", "<order_description>"+description+"</order_description><customer_vault_id>vault</customer_vault_id><currency>", 1)
			switch name {
			case "optional_query_fields_absent":
				txn = strings.ReplaceAll(strings.ReplaceAll(txn, "<customer_vault_id>vault</customer_vault_id>", ""), "<currency>USD</currency>", "")
			case "wrong_vault":
				txn = strings.ReplaceAll(txn, "<customer_vault_id>vault", "<customer_vault_id>other")
			case "wrong_currency":
				txn = strings.ReplaceAll(txn, "<currency>USD", "<currency>EUR")
			case "inexact_amount":
				txn = strings.ReplaceAll(txn, "<amount>5.00", "<amount>5.001")
			case "missing_time":
				txn = strings.ReplaceAll(txn, "20261008120000", "unreadable")
			}
			f := newNMIFake(t, func(c nmiCall) (int, string) {
				switch c.Path {
				case "/query":
					require.Equal(t, "wire-key", c.Form.Get("security_key"))
					body := txn
					if c.Form.Get("order_description") != "" {
						require.Equal(t, description, c.Form.Get("order_description"))
						require.Empty(t, c.Form.Get("start_date"), "all invoice attempts must be considered")
						require.Empty(t, c.Form.Get("page_number"), "omitted page number is the documented first page")
						switch name {
						case "another_sale":
							body += strings.ReplaceAll(txn, "<transaction_id>txn", "<transaction_id>other")
						case "refund":
							body = strings.ReplaceAll(txn, "</transaction>", "<action><action_type>refund</action_type><success>1</success><amount>1.00</amount></action></transaction>")
						case "unbound_history":
							body = strings.ReplaceAll(txn, description, "invoice "+uuid.NewString())
						}
					} else {
						require.True(t, c.Form.Get("transaction_id") == "txn" || c.Form.Get("order_id") == order.String())
					}
					return http.StatusOK, nmResponse(body)
				case "/payments/txn":
					require.Equal(t, http.MethodGet, c.Method)
					require.Equal(t, "wire-key", c.Auth)
					if name == "missing_exact_read" {
						return http.StatusNotFound, `{}`
					}
					payment := v5Transaction{ID: "txn", Object: "transaction", Currency: "USD", CustomerVaultID: "vault", Response: "1", Actions: []v5TxnAction{{Type: "sale", Amount: "5.00", Success: true}}}
					if name == "v5_refund" {
						payment.Actions = append(payment.Actions, v5TxnAction{Type: "refund", Amount: "1.00", Success: true})
					}
					return http.StatusOK, jsonBody(t, payment)
				default:
					t.Errorf("unexpected financial request %s %s", c.Method, c.Path)
					return http.StatusInternalServerError, ""
				}
			})
			client := f.client(t)
			client.ReadOnly, client.SecurityKey = true, "wrong-key"
			receipt, err := client.ReadInvoiceSaleEvidence(t.Context(), "txn")
			if name == "qualified" || name == "optional_query_fields_absent" {
				require.NoError(t, err)
				require.Equal(t, invoice, receipt.InvoiceID)
				require.Equal(t, "vault", receipt.Sale.CustomerVaultID)
				require.EqualValues(t, 500, receipt.Sale.Amount)
			} else {
				require.Error(t, err, fmt.Sprint(receipt))
			}
		})
	}
}
