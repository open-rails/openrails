//go:build integration

package money_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
)

// This is the ordinary core proof: real PostgreSQL, real scoped adapter and
// durable invoice runner, loopback custodian/NMI boundaries. The separately
// selected browser/vendor qualification proves actual vault interpolation.
func TestHyperSwitchInvoiceCollectionWorkflow(t *testing.T) {
	for _, mode := range []string{"recurring arming", "CIT then MIT", "archived stamped account", "lost response", "declined", "preflight missing", "preflight revoked after fence", "metadata unavailable after fence", "old marker unavailable", "park before first send", "profile changes before send", "deployment changes before resume", "secret rotates before resume"} {
		t.Run(mode, func(t *testing.T) {
			e := newNMIReceiptEnv(t)
			custodian, vendorAccount := uuid.New(), "vendor_"+uuid.NewString()
			*e.custodian = custodian
			_, err := e.pool.Exec(e.ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,environment,account_id,settings,credential_versions) VALUES($1,$2,$3,'hyperswitch','test',$4,'{"profile_id":"profile_A","public_api_key":"public_A"}','{"api_key":1}')`, custodian, dbtest.TestMerchantID.UUID(), custodian.String(), vendorAccount)
			require.NoError(t, err)
			_, err = e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET custodian='hyperswitch',custodian_id=$2,rail_customer_ref='customer_A',rail_method_ref='method_A',charge_via='pan_proxy',stored_credential_unscheduled_ref='',stored_credential_recurring_ref='' WHERE id=$1`, e.method, custodian)
			require.NoError(t, err)
			secret, err := merchants.CustodianSecretName("hyperswitch", "test", vendorAccount, "api_key")
			require.NoError(t, err)
			_, err = e.merchants.Secrets().Put(e.ctx, dbtest.TestMerchantID, secret, "custody-key")
			require.NoError(t, err)
			t.Cleanup(func() { _ = e.merchants.Secrets().Delete(context.WithoutCancel(e.ctx), dbtest.TestMerchantID, secret) })
			if mode == "archived stamped account" {
				original := e.methodRow(t).PspID
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.psps SET archived=true,custodian_id=NULL WHERE id=$1`, original)
				require.NoError(t, err)
				other := uuid.New()
				_, err = e.pool.Exec(e.ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id) VALUES($1,$2,'nmi','test',$3)`, other, dbtest.TestMerchantID.UUID(), "unrelated-"+other.String())
				require.NoError(t, err)
				t.Cleanup(func() {
					_, _ = e.pool.Exec(context.WithoutCancel(e.ctx), `DELETE FROM billing.psps WHERE id=$1`, other)
				})
			}
			var mu sync.Mutex
			expectedKey := "custody-key"
			resumed := false
			repaired := false
			var forms []map[string]string
			preflights := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				key := expectedKey
				isResume := resumed
				isRepaired := repaired
				mu.Unlock()
				require.Equal(t, "api-key="+key, r.Header.Get("Authorization"))
				require.Equal(t, "profile_A", r.Header.Get("x-profile-id"))
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v2/payment-methods/method_A" {
					if mode == "metadata unavailable after fence" && !isRepaired {
						w.WriteHeader(503)
						return
					}
					require.Equal(t, "false", r.URL.Query().Get("fetch_raw_detail"))
					_, _ = fmt.Fprintf(w, `{"id":"method_A","merchant_id":%q,"customer_id":"customer_A","storage_type":"persistent","payment_method_data":{"card":{"last4_digits":"1111","expiry_month":"12","expiry_year":"2030"}}}`, vendorAccount)
					return
				}
				require.Equal(t, "/v2/proxy", r.URL.Path)
				if r.Method == http.MethodGet {
					mu.Lock()
					preflights++
					count := preflights
					mu.Unlock()
					if mode == "preflight missing" || (mode == "preflight revoked after fence" && count > 1 && !isRepaired) || (strings.HasSuffix(mode, "before resume") && !isResume) {
						w.WriteHeader(404)
						return
					}
					if mode == "park before first send" {
						_, err := e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET park_reason='fixture custody revoked',parked_at=now() WHERE id=$1`, e.method)
						require.NoError(t, err)
					}
					if mode == "profile changes before send" {
						_, err := e.pool.Exec(e.ctx, `UPDATE billing.custodians SET settings=jsonb_set(settings,'{profile_id}','"profile_B"') WHERE id=$1`, custodian)
						require.NoError(t, err)
					}
					_, _ = fmt.Fprintf(w, `{"contract":"openrails-nmi-form-v2","strict":true,"max_response_bytes":65536,"routes":[{"destination_url":%q,"method":"POST","response_profile":"nmi_classic"}]}`, e.plane.Endpoints.NMIDirectPostURL)
					return
				}
				var input struct {
					Token string            `json:"token"`
					Kind  string            `json:"token_type"`
					Form  map[string]string `json:"request_body"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&input))
				require.Equal(t, "method_A", input.Token)
				require.Equal(t, "payment_method_id", input.Kind)
				require.Equal(t, "synthetic-key", input.Form["security_key"])
				require.Equal(t, "{{$card_number}}", input.Form["ccnumber"])
				if mode == "recurring arming" {
					require.Equal(t, "recurring", input.Form["billing_method"])
				} else {
					require.Empty(t, input.Form["billing_method"])
				}
				mu.Lock()
				forms = append(forms, input.Form)
				n := len(forms)
				mu.Unlock()
				transaction := fmt.Sprintf("txn_hs_%d", n)
				if mode == "declined" {
					_, _ = w.Write([]byte(`{"response":{"response":"2","response_code":"200","responsetext":"Declined"},"status_code":200,"response_headers":{}}`))
					return
				}
				e.gateway.orderSale(input.Form["orderid"], transaction)
				if mode != "old marker unavailable" {
					e.gateway.payment(transaction, "", input.Form["amount"], input.Form["currency"])
				}
				if mode == "lost response" || mode == "old marker unavailable" {
					conn, _, err := w.(http.Hijacker).Hijack()
					require.NoError(t, err)
					_ = conn.Close()
					return
				}
				_, _ = fmt.Fprintf(w, `{"response":{"response":"1","response_code":"100","responsetext":"Approved","transactionid":%q},"status_code":200,"response_headers":{}}`, transaction)
			}))
			t.Cleanup(server.Close)
			e.plane.Config.HyperSwitch = &config.HyperSwitchConfig{APIBaseURL: server.URL}
			require.NoError(t, e.svc.SetHyperSwitchDeployment(server.URL))
			if mode == "recurring arming" {
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET stored_credential_recurring_ref='original_recurring' WHERE id=$1`, e.method)
				require.NoError(t, err)
				method := e.methodRow(t)
				require.Empty(t, method.StoredCredentialUnscheduledRef)
				binding := charge.HyperSwitchBinding{AccountID: vendorAccount, ProfileID: "profile_A", APIBaseURL: server.URL}
				for _, field := range []string{"account", "profile", "deployment"} {
					changed := binding
					switch field {
					case "account":
						changed.AccountID = "other"
					case "profile":
						changed.ProfileID = "other"
					case "deployment":
						changed.APIBaseURL = server.URL + "/other"
					}
					_, err := money.PrepareHyperSwitchCharge(e.ctx, e.plane, method, changed)
					require.Error(t, err, field)
				}
				_, err = e.pool.Exec(e.ctx, `UPDATE billing.custodians SET credential_versions='{"api_key":2}' WHERE id=$1`, custodian)
				require.NoError(t, err)
				_, err = money.PrepareHyperSwitchCharge(e.ctx, e.plane, method, binding)
				require.Error(t, err, "a credential below the stored floor cannot arm")
				_, err = e.merchants.Secrets().Put(e.ctx, dbtest.TestMerchantID, secret, "rotated-custody-key")
				require.NoError(t, err)
				mu.Lock()
				expectedKey = "rotated-custody-key"
				mu.Unlock()
				charger, err := money.PrepareHyperSwitchCharge(e.ctx, e.plane, method, binding)
				require.NoError(t, err, "rotation may satisfy the floor without changing frozen binding")
				mu.Lock()
				require.Empty(t, forms)
				require.Zero(t, preflights)
				mu.Unlock()
				result, refusal, err := charger.ChargeRecurringMIT(e.ctx, charge.Request{Instrument: charge.Instrument{PaymentMethodID: method.ID, Rail: "nmi", CustomerRef: method.RailCustomerRef, MethodRef: method.RailMethodRef}, AmountMinor: 5, Currency: "USD", OrderRef: uuid.NewString(), Context: charge.RecurringMIT(method.StoredCredentialRecurringRef)})
				require.NoError(t, err)
				require.Nil(t, refusal)
				require.NotEmpty(t, result.TransactionID)
				require.Empty(t, result.CapturedRef)
				mu.Lock()
				require.Len(t, forms, 1)
				require.Equal(t, "0.05", forms[0]["amount"])
				require.Equal(t, "merchant", forms[0]["initiated_by"])
				require.Equal(t, "used", forms[0]["stored_credential_indicator"])
				require.Equal(t, "original_recurring", forms[0]["initial_transaction_id"])
				mu.Unlock()
				require.Zero(t, e.settledPayments(t), "the transport helper does not settle an invoice or grant membership")
				return
			}
			request := money.InvoiceCollectionRetryRequest{InvoiceID: e.invoice, PaymentMethodID: e.method, IdempotencyKey: "hs-invoice-" + uuid.NewString()}
			result, err := e.svc.PayInvoiceNow(e.ctx, e.runner, e.payer, request)
			require.NoError(t, err)
			e.op = result.Operation.ID
			if strings.HasSuffix(mode, "before resume") {
				require.Equal(t, intents.StatusPending, result.Operation.Status)
				require.Empty(t, intents.EvidenceString(result.Operation, "submitted_at"))
				mu.Lock()
				resumed = true
				mu.Unlock()
				if mode == "secret rotates before resume" {
					rotated, err := e.merchants.Secrets().Put(e.ctx, dbtest.TestMerchantID, secret, "rotated-custody-key")
					require.NoError(t, err)
					_, err = e.pool.Exec(e.ctx, `UPDATE billing.custodians SET credential_versions=jsonb_build_object('api_key',$2::int) WHERE id=$1`, custodian, rotated.Version)
					require.NoError(t, err)
					mu.Lock()
					expectedKey = "rotated-custody-key"
					mu.Unlock()
				} else {
					e.plane.Config.HyperSwitch.APIBaseURL = server.URL + "/another-deployment"
				}
				dueNow(t, e.pool, e.ctx, e.op)
				_, err = e.runner.RunExecuteOnce(e.ctx)
				require.NoError(t, err)
				result.Operation = latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
			}
			switch mode {
			case "preflight missing":
				require.Equal(t, intents.StatusPending, result.Operation.Status)
				require.Empty(t, intents.EvidenceString(result.Operation, "submitted_at"))
			case "preflight revoked after fence", "metadata unavailable after fence":
				require.Equal(t, intents.StatusFailedTerminal, result.Operation.Status)
				require.Nil(t, e.invoiceRow(t).CollectionIntentID)
				require.NotEqual(t, "paid", e.invoiceRow(t).Status)
				require.Empty(t, e.methodRow(t).StoredCredentialUnscheduledRef)
				var settlements int
				require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.invoice_payments WHERE invoice_id=$1 AND status='settled'`, e.invoice).Scan(&settlements))
				require.Zero(t, settlements)
				require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, e.payer.UUID()).Scan(&settlements))
				require.Zero(t, settlements)
				mu.Lock()
				require.Empty(t, forms)
				repaired = true
				mu.Unlock()
				request.IdempotencyKey = "repaired-" + uuid.NewString()
				retry, err := e.svc.PayInvoiceNow(e.ctx, e.runner, e.payer, request)
				require.NoError(t, err)
				require.Equal(t, intents.StatusSucceeded, retry.Operation.Status)
				require.NotEqual(t, result.Operation.ID, retry.Operation.ID)
				require.Equal(t, "paid", e.invoiceRow(t).Status)
				require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM billing.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, e.payer.UUID()).Scan(&settlements))
				require.Equal(t, 1, settlements)
			case "old marker unavailable":
				require.Equal(t, intents.StatusUnknownNeedsVerify, result.Operation.Status)
				require.NotEmpty(t, intents.EvidenceString(result.Operation, "submitted_at"))
				require.Equal(t, intents.StatusUnknownNeedsVerify, e.verify(t))
				resumed, err := e.runner.ExecuteByID(e.ctx, e.op)
				require.NoError(t, err)
				require.Equal(t, intents.StatusUnknownNeedsVerify, resumed.Status)
				require.NotNil(t, e.invoiceRow(t).CollectionIntentID)
			case "park before first send", "profile changes before send", "deployment changes before resume":
				require.Equal(t, intents.StatusFailedTerminal, result.Operation.Status)
				require.Nil(t, e.invoiceRow(t).CollectionIntentID)
				require.NotEqual(t, "paid", e.invoiceRow(t).Status)
			case "declined":
				require.Equal(t, intents.StatusFailedTerminal, result.Operation.Status)
				require.Empty(t, e.methodRow(t).StoredCredentialUnscheduledRef)
			default:
				if mode == "lost response" {
					require.Equal(t, intents.StatusUnknownNeedsVerify, result.Operation.Status)
					_, err = e.pool.Exec(e.ctx, `UPDATE billing.payment_methods SET park_reason='revoked after send',parked_at=now() WHERE id=$1`, e.method)
					require.NoError(t, err)
					require.Equal(t, intents.StatusSucceeded, e.verify(t), "an old submitted marker must reconcile despite later parking")
				} else {
					require.Equal(t, intents.StatusSucceeded, result.Operation.Status)
				}
				e.requireSettledOnce(t)
				require.Equal(t, "txn_hs_1", e.methodRow(t).StoredCredentialUnscheduledRef)
				require.Empty(t, e.methodRow(t).StoredCredentialRecurringRef)
				replay, err := e.svc.PayInvoiceNow(e.ctx, e.runner, e.payer, request)
				require.NoError(t, err)
				require.True(t, replay.Replayed)
				if mode == "CIT then MIT" {
					e.invoice = seedArrearsInvoice(t, e.svc, e.ctx, e.payer, e.method)
					count, err := e.svc.ChargeOutstanding(e.ctx, e.runner, 0)
					require.NoError(t, err)
					require.Equal(t, 1, count)
					require.Equal(t, "paid", e.invoiceRow(t).Status)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			want := 1
			if mode == "preflight missing" || mode == "park before first send" || mode == "profile changes before send" || mode == "deployment changes before resume" {
				want = 0
			}
			if mode == "CIT then MIT" {
				want = 2
			}
			require.Len(t, forms, want)
			if len(forms) > 0 {
				require.Equal(t, "0.05", forms[0]["amount"], "50,000 USD native units reach NMI exactly")
				require.Equal(t, "customer", forms[0]["initiated_by"])
				require.Equal(t, "stored", forms[0]["stored_credential_indicator"])
			}
			if len(forms) == 2 {
				require.Equal(t, "merchant", forms[1]["initiated_by"])
				require.Equal(t, "used", forms[1]["stored_credential_indicator"])
				require.Equal(t, "txn_hs_1", forms[1]["initial_transaction_id"])
			}
		})
	}
}
