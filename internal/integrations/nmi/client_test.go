package nmi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

type nmiCall struct {
	Method, Path, Auth, Body string
	Form, Query              url.Values
}

// nmiFake is the provider boundary: classic direct post at /transact, the
// Query API at /query and v5 JSON everywhere else.
type nmiFake struct {
	*httptest.Server
	mu    sync.Mutex
	calls []nmiCall
}

func newNMIFake(t *testing.T, respond func(nmiCall) (int, string)) *nmiFake {
	t.Helper()
	f := &nmiFake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c := nmiCall{Method: r.Method, Path: r.URL.Path, Auth: r.Header.Get("Authorization"), Body: string(raw), Query: r.URL.Query()}
		if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
			c.Form, _ = url.ParseQuery(c.Body)
		}
		f.mu.Lock()
		f.calls = append(f.calls, c)
		f.mu.Unlock()
		status, body := respond(c)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(f.Close)
	return f
}

func reply(body string) func(nmiCall) (int, string) {
	return func(nmiCall) (int, string) { return http.StatusOK, body }
}

func (f *nmiFake) Calls() []nmiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]nmiCall(nil), f.calls...)
}

func (f *nmiFake) client(t *testing.T) *NMIClient { return nmiClient(t, f.URL) }

func nmiClient(t *testing.T, base string) *NMIClient {
	t.Helper()
	c, err := NewAccountClient(uuid.New(), uuid.New(), "nmi", &config.NMIProviderSettings{SecurityKey: "wire-key"}, true)
	require.NoError(t, err)
	c.DirectPostURL, c.QueryURL, c.V5BaseURL = base+"/transact", base+"/query", base
	c.LoopbackFixture = true
	return c
}

func citOneTime() *StoredCredential {
	return &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: IndicatorStored}
}

func citRecurring() *StoredCredential {
	return &StoredCredential{InitiatedBy: InitiatedByCustomer, Indicator: IndicatorStored, Recurring: true}
}

func mitRecurring() *StoredCredential {
	return &StoredCredential{InitiatedBy: InitiatedByMerchant, Indicator: IndicatorUsed, InitialTransactionID: "initial-txn", Recurring: true}
}

const approvedDirect = "response=1&response_code=100&responsetext=SUCCESS&transactionid=txn-1&subscription_id=sub-1&authcode=A1"

func TestDirectPostWireShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		call   func(t *testing.T, c *NMIClient)
		want   map[string]string
		absent []string
	}{
		{"unscheduled CIT sale", func(t *testing.T, c *NMIClient) {
			got, err := c.RunSale(t.Context(), SaleParams{CustomerVaultID: "v1", BillingID: " b1 ", Amount: 1999, Currency: "USD", OrderID: "o1", StoredCredential: citOneTime()})
			require.NoError(t, err)
			require.Equal(t, SaleResponse{TransactionID: "txn-1", Authcode: "A1", ResponseText: "SUCCESS"}, *got)
		}, map[string]string{"type": "sale", "security_key": "wire-key", "customer_vault_id": "v1", "billing_id": "b1", "amount": "19.99", "currency": "USD",
			"orderid": "o1", "order_description": "One-time purchase", "initiated_by": "customer", "stored_credential_indicator": "stored"},
			[]string{"initial_transaction_id", "billing_method", "recurring"}},
		{"unscheduled MIT sale in yen", func(t *testing.T, c *NMIClient) {
			_, err := c.RunSale(t.Context(), SaleParams{CustomerVaultID: "v1", Amount: 500, Currency: "JPY", OrderDescription: "d", StoredCredential: &StoredCredential{InitiatedBy: InitiatedByMerchant, Indicator: IndicatorUsed, InitialTransactionID: "anchor"}})
			require.NoError(t, err)
		}, map[string]string{"amount": "500.00", "currency": "JPY", "order_description": "d", "initiated_by": "merchant", "stored_credential_indicator": "used", "initial_transaction_id": "anchor"},
			[]string{"billing_method", "billing_id", "orderid"}},
		{"paid enrollment", func(t *testing.T, c *NMIClient) {
			got, err := c.AddRecurringSubscription(t.Context(), RecurringPaymentData{PlanID: "p1", CustomerVaultID: "v1", BillingID: "b1", Currency: "JPY", Amount: 4,
				OrderID: " o1 ", PONumber: "o1", StartDate: "20261021", CustomerID: "cust", StoredCredential: citRecurring()})
			require.NoError(t, err)
			require.Equal(t, "sub-1", got.SubscriptionID)
			require.Equal(t, "txn-1", got.TransactionID)
		}, map[string]string{"recurring": "add_subscription", "type": "sale", "amount": "4.00", "currency": "JPY", "plan_id": "p1", "customer_vault_id": "v1", "billing_id": "b1",
			"orderid": "o1", "ponumber": "o1", "start_date": "20261021", "billing_method": "recurring", "initiated_by": "customer", "stored_credential_indicator": "stored"},
			[]string{"customerid", "payment_token", "initial_transaction_id"}},
		{"schedule-only enrollment", func(t *testing.T, c *NMIClient) {
			_, err := c.AddRecurringSubscription(t.Context(), RecurringPaymentData{ScheduleOnly: true, PlanID: "p1", CustomerVaultID: "v1", Currency: "USD", StartDate: "20261021"})
			require.NoError(t, err)
		}, map[string]string{"recurring": "add_subscription", "plan_id": "p1", "start_date": "20261021"},
			[]string{"type", "amount", "billing_method", "initiated_by", "stored_credential_indicator", "initial_transaction_id"}},
		{"token enrollment", func(t *testing.T, c *NMIClient) {
			_, err := c.AddRecurringSubscription(t.Context(), RecurringPaymentData{PlanID: "p1", PaymentToken: "tok", CustomerID: "cust", BillingID: "b1", Currency: "USD", Amount: 100, StoredCredential: citRecurring()})
			require.NoError(t, err)
		}, map[string]string{"payment_token": "tok", "customerid": "cust", "amount": "1.00"}, []string{"customer_vault_id", "billing_id"}},
		{"recurring MIT rebill", func(t *testing.T, c *NMIClient) {
			got, err := c.AttemptManualRebill(t.Context(), ManualRebillParams{VaultID: "v1", BillingID: "b1", SubscriptionID: "s1", OrderID: "ord", PONumber: "ord", StoredCredential: mitRecurring()})
			require.NoError(t, err)
			require.Equal(t, ManualRebillResponse{Success: true, TransactionID: "txn-1"}, *got)
		}, map[string]string{"type": "sale", "recurring": "rebill_subscription", "subscription_id": "s1", "customer_vault_id": "v1", "billing_id": "b1", "orderid": "ord", "ponumber": "ord",
			"billing_method": "recurring", "initiated_by": "merchant", "stored_credential_indicator": "used", "initial_transaction_id": "initial-txn"},
			[]string{"amount"}},
		{"edit plan sends only mutable fields", func(t *testing.T, c *NMIClient) {
			require.NoError(t, c.EditRecurringPlan(t.Context(), "plan-1", "New", 1999, "USD"))
		}, map[string]string{"recurring": "edit_plan", "current_plan_id": "plan-1", "plan_amount": "19.99", "plan_name": "New"},
			[]string{"plan_id", "day_frequency", "plan_payments"}},
		{"recurring agreement verification moves no funds", func(t *testing.T, c *NMIClient) {
			id, err := c.EstablishRecurringAgreement(t.Context(), " v1 ", "b1", "ord")
			require.NoError(t, err)
			require.Equal(t, "txn-1", id)
		}, map[string]string{"type": "validate", "customer_vault_id": "v1", "billing_id": "b1", "orderid": "ord", "billing_method": "recurring", "initiated_by": "customer", "stored_credential_indicator": "stored"},
			[]string{"amount", "initial_transaction_id"}},
		{"subscription payment source", func(t *testing.T, c *NMIClient) {
			require.NoError(t, c.UpdateSubscriptionPaymentSource(t.Context(), "s1", "v2"))
		}, map[string]string{"recurring": "update_subscription", "subscription_id": "s1", "customer_vault_id": "v2"}, []string{"plan_amount"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNMIFake(t, reply(approvedDirect))
			tc.call(t, f.client(t))
			calls := f.Calls()
			require.Len(t, calls, 1)
			require.Equal(t, http.MethodPost, calls[0].Method)
			require.Equal(t, "/transact", calls[0].Path)
			for k, v := range tc.want {
				require.Equal(t, v, calls[0].Form.Get(k), k)
			}
			for _, k := range tc.absent {
				require.NotContains(t, calls[0].Form, k)
			}
		})
	}
}

func TestV5WireShapes(t *testing.T) {
	f := newNMIFake(t, func(c nmiCall) (int, string) {
		switch c.Method + " " + c.Path {
		case "GET /customers/vault-9":
			return 200, `{"object":"customer","id":"vault-9","billing":[{"id":"B0","priority":2},{"id":"B77","priority":1}]}`
		case "POST /customers":
			return 200, `{"object":"customer","id":"v9","billing":[{"id":"B1","priority":1,"payment_details":{"card_number":"4xxxxxxxxxxx1111","card_type":"visa"}}]}`
		case "POST /subscriptions":
			return 200, `{"object":"subscription","id":"sub-9"}`
		}
		return 200, `{"object":"transaction","id":"r1","response":"1","response_code":"100"}`
	})
	c := f.client(t)
	anchor := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, method, path, body string
		call                     func() error
	}{
		{"partial refund", "POST", "/payments/t1/refund", `{"amount":5.00}`, func() error {
			got, err := c.Refund(t.Context(), RefundParams{TransactionID: " t1 ", Amount: 500, Currency: "USD"})
			if err == nil {
				require.Equal(t, "r1", got.TransactionID)
			}
			return err
		}},
		{"yen refund keeps its scale", "POST", "/payments/t1/refund", `{"amount":100.00}`, func() error {
			_, err := c.Refund(t.Context(), RefundParams{TransactionID: "t1", Amount: 100, Currency: "JPY"})
			return err
		}},
		{"full refund omits amount", "POST", "/payments/t1/refund", `{}`, func() error {
			_, err := c.Refund(t.Context(), RefundParams{TransactionID: "t1", Currency: "USD"})
			return err
		}},
		{"void", "POST", "/payments/t1/void", `{}`, func() error { return c.Void(t.Context(), "t1") }},
		{"create plan", "POST", "/plans", `{"id":"p1","plan_name":"Premium","plan_amount":9.99,"plan_payments":0,"day_frequency":30}`, func() error {
			return c.AddRecurringPlan(t.Context(), "p1", "Premium", 999, "USD", 30, 0)
		}},
		{"create vault", "POST", "/customers", `{"billing":{"currency":"USD","payment_details":{"payment_token":"tok"},"first_name":"Ada"}}`, func() error {
			got, err := c.CreateCustomerVault(t.Context(), CreateCustomerVaultData{PaymentToken: " tok ", FirstName: "Ada"})
			if err == nil {
				require.Equal(t, CreateCustomerVaultResponse{CustomerVaultID: "v9", BillingID: "B1", Card: V5BillingCardData{CardNumber: "4xxxxxxxxxxx1111", CardType: "visa"}}, *got)
			}
			return err
		}},
		{"update vault resolves the priority-1 billing id", "PATCH", "/customers/vault-9", `{"billing":[{"id":"B77","currency":"USD","payment_details":{"payment_token":"tok"}}]}`, func() error {
			return c.UpdateCustomerVault(t.Context(), UpdateCustomerVaultData{CustomerVaultID: "vault-9", CreateCustomerVaultData: CreateCustomerVaultData{PaymentToken: "tok"}})
		}},
		{"update vault targets a known billing id", "PATCH", "/customers/vault-9", `{"billing":[{"id":"B88","currency":"USD"}]}`, func() error {
			return c.UpdateCustomerVault(t.Context(), UpdateCustomerVaultData{CustomerVaultID: "vault-9", BillingID: "B88"})
		}},
		{"delete one billing entry", "DELETE", "/customers/vault-9/billing/bill-2", ``, func() error { return c.DeleteCustomerBillingEntry(t.Context(), "vault-9", "bill-2") }},
		{"delete vault", "DELETE", "/customers/vault-9", ``, func() error {
			return c.DeleteCustomerVault(t.Context(), DeleteCustomerVaultData{CustomerVaultID: "vault-9"})
		}},
		{"delete subscription", "DELETE", "/subscriptions/s1", ``, func() error { return c.DeleteRecurringSubscription(t.Context(), "s1") }},
		{"paused cutover enrollment", "POST", "/subscriptions", `{"plan_id":"p1","customer_vault":{"id":"v1","billing_id":"b1"},"paused_subscription":true,"start_date":"20261021000000"}`, func() error {
			_, err := c.CreatePausedSubscription(t.Context(), "p1", "v1", "b1", anchor)
			return err
		}},
		{"cutover activation", "PUT", "/subscriptions/s1", `{"paused_subscription":false,"start_date":"20261021000000"}`, func() error {
			return c.ActivateSubscription(t.Context(), "s1", anchor)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(f.Calls())
			require.NoError(t, tc.call())
			calls := f.Calls()
			last := calls[len(calls)-1]
			require.Equal(t, tc.method, last.Method)
			require.Equal(t, tc.path, last.Path)
			require.Equal(t, "wire-key", last.Auth, "the bare key is the whole Authorization value")
			if tc.body == "" {
				require.Empty(t, last.Body)
			} else {
				require.JSONEq(t, tc.body, last.Body)
			}
			if tc.name == "update vault resolves the priority-1 billing id" {
				require.Equal(t, "GET /customers/vault-9", calls[before].Method+" "+calls[before].Path)
			} else {
				require.Len(t, calls, before+1)
			}
		})
	}
	require.Contains(t, f.Calls()[0].Body, `"amount":5.00`, "exact two-decimal JSON number on the wire")
}

func TestRequestsRefusedBeforeTheGateway(t *testing.T) {
	f := newNMIFake(t, reply(`{"object":"transaction","id":"t","response":"1","response_code":"100"}`))
	c := f.client(t)
	ctx := t.Context()
	sale := func(mut func(*SaleParams)) error {
		p := SaleParams{CustomerVaultID: "v1", Amount: 100, Currency: "USD", StoredCredential: citOneTime()}
		mut(&p)
		_, err := c.RunSale(ctx, p)
		return err
	}
	enroll := func(d RecurringPaymentData) error { _, err := c.AddRecurringSubscription(ctx, d); return err }
	unaccounted := *c
	unaccounted.SecurityKey = "rotated-under-it"
	for name, err := range map[string]error{
		"sale without credential": sale(func(p *SaleParams) { p.StoredCredential = nil }),
		"sale with invalid credential": sale(func(p *SaleParams) {
			p.StoredCredential = &StoredCredential{InitiatedBy: InitiatedByMerchant, Indicator: IndicatorStored}
		}),
		"sale order id over 50": sale(func(p *SaleParams) { p.OrderID = strings.Repeat("o", 51) }),
		"sale zero amount":      sale(func(p *SaleParams) { p.Amount = 0 }),
		"sale without currency": sale(func(p *SaleParams) { p.Currency = " " }),
		"sale unknown currency": sale(func(p *SaleParams) { p.Currency = "XYZ" }),
		"sale without vault":    sale(func(p *SaleParams) { p.CustomerVaultID = "" }),
		"negative refund": func() error {
			_, err := c.Refund(ctx, RefundParams{TransactionID: "t", Amount: -1, Currency: "USD"})
			return err
		}(),
		"refund without currency":        func() error { _, err := c.Refund(ctx, RefundParams{TransactionID: "t", Amount: 1}); return err }(),
		"schedule-only with amount":      enroll(RecurringPaymentData{ScheduleOnly: true, PlanID: "p", CustomerVaultID: "v", Amount: 1}),
		"schedule-only with credential":  enroll(RecurringPaymentData{ScheduleOnly: true, PlanID: "p", CustomerVaultID: "v", StoredCredential: citRecurring()}),
		"paid enrollment w/o credential": enroll(RecurringPaymentData{PlanID: "p", CustomerVaultID: "v", Currency: "USD", Amount: 1}),
		"enrollment without source":      enroll(RecurringPaymentData{PlanID: "p", Currency: "USD", StoredCredential: citRecurring()}),
		"reference-less MIT rebill": func() error {
			_, err := c.AttemptManualRebill(ctx, ManualRebillParams{VaultID: "v", BillingID: "b", SubscriptionID: "s", StoredCredential: &StoredCredential{InitiatedBy: InitiatedByMerchant, Indicator: IndicatorUsed, Recurring: true}})
			return err
		}(),
		"plan without frequency": c.AddRecurringPlan(ctx, "p", "n", 100, "USD", 0, 0),
		"plan without name":      c.AddRecurringPlan(ctx, "p", "", 100, "USD", 30, 0),
		"plan edit without id":   c.EditRecurringPlan(ctx, "", "n", 100, "USD"),
		"agreement order id over 50": func() error {
			_, err := c.EstablishRecurringAgreement(ctx, "v", "", strings.Repeat("o", 51))
			return err
		}(),
		"native sale with rotated key": unaccounted.PrepareRecurringSale(ctx, "v", "b"),
		"native sale inexact billing":  c.PrepareRecurringSale(ctx, "v", " b"),
		"paused enrollment w/o anchor": func() error { _, err := c.CreatePausedSubscription(ctx, "p", "v", "b", time.Time{}); return err }(),
	} {
		require.Error(t, err, name)
		require.False(t, IsTransportAmbiguous(err), name)
	}
	require.Empty(t, f.Calls(), "refused requests never reach NMI")

	c.ReadOnly = true
	for name, err := range map[string]error{
		"direct post": func() error {
			_, err := c.RunSale(ctx, SaleParams{CustomerVaultID: "v", Amount: 1, Currency: "USD", StoredCredential: citOneTime()})
			return err
		}(),
		"v5 delete":   c.DeleteRecurringSubscription(ctx, "s"),
		"v5 refund":   func() error { _, err := c.Refund(ctx, RefundParams{TransactionID: "t", Currency: "USD"}); return err }(),
		"native sale": c.PrepareRecurringSale(ctx, "v", "b"),
	} {
		require.ErrorIs(t, err, ErrProviderReadOnly, name)
		require.False(t, IsTransportAmbiguous(err), name)
	}
	require.Empty(t, f.Calls())
	_, _, err := c.GetPayment(ctx, "t")
	require.NoError(t, err, "reads stay available under readonly")
}

// A contradictory or incomplete classic reply after one POST is uncertainty,
// never authority to record a decline or to send the operation again.
func TestDirectResponseOutcomeClassification(t *testing.T) {
	const sentinel = "RAW_PROVIDER_SENTINEL"
	cases := []struct {
		name, body string
		declined   bool
	}{
		{"qualified refusal", "response=2&response_code=202&responsetext=" + sentinel, true},
		{"communication error", "response=3&response_code=420&responsetext=" + sentinel, false},
		{"unqualified error", "response=3&response_code=400&response_message=" + sentinel, false},
		{"contradictory discriminators", "response=2&response=1&response_code=202&response_code=100", false},
		{"repeated equal discriminator", "response=2&response=2&response_code=202", false},
		{"contradictory approval", "response=1&response_code=202&transactionid=one", false},
		{"contradictory refusal", "response=2&response_code=100", false},
		{"missing refusal code", "response=2&responsetext=" + sentinel, false},
		{"invalid refusal code", "response=2&response_code=invalid", false},
		{"invalid encoding", "response=2&response_code=202&responsetext=%Q" + sentinel, false},
		{"duplicate payment identity", "response=1&response_code=100&transactionid=one&transactionid=two", false},
		{"duplicate schedule identity", "response=1&response_code=100&subscription_id=one&subscription_id=two", false},
		{"not a form", "<html>bad gateway</html>", false},
	}
	for _, caller := range []string{"sale", "initial", "rebill"} {
		for _, tc := range cases {
			t.Run(caller+"/"+tc.name, func(t *testing.T) {
				f := newNMIFake(t, reply(tc.body))
				c := f.client(t)
				var err error
				var diagnostic string
				declined := false
				switch caller {
				case "sale":
					_, err = c.RunSale(t.Context(), SaleParams{CustomerVaultID: "v", Amount: 999, Currency: "USD", OrderID: "op", StoredCredential: citOneTime()})
				case "initial":
					_, err = c.AddRecurringSubscription(t.Context(), RecurringPaymentData{CustomerVaultID: "v", PlanID: "p", Amount: 999, Currency: "USD", StoredCredential: citRecurring()})
				case "rebill":
					var res *ManualRebillResponse
					res, err = c.AttemptManualRebill(t.Context(), ManualRebillParams{VaultID: "v", BillingID: "b", SubscriptionID: "s", StoredCredential: mitRecurring()})
					declined, diagnostic = res.Declined, res.ErrorMessage
					require.False(t, res.Success)
				}
				if err != nil {
					diagnostic += err.Error()
					var refusal *CustomerVaultError
					if errors.As(err, &refusal) {
						require.True(t, tc.declined, "unknown response exposed typed decline proof")
						declined = !RequiresVerification(err)
						require.Equal(t, tc.body, refusal.RawResponse, "a qualified refusal retains its raw proof internally")
					}
				}
				require.Equal(t, tc.declined, declined)
				if !tc.declined {
					require.True(t, RequiresVerification(err), "got %v", err)
				}
				require.NotContains(t, diagnostic, sentinel, "opaque provider text never reaches diagnostics")
				require.Len(t, f.Calls(), 1)
			})
		}
	}

	f := newNMIFake(t, reply("response=1&response_code=100"))
	res, err := f.client(t).AttemptManualRebill(t.Context(), ManualRebillParams{VaultID: "v", BillingID: "b", SubscriptionID: "s", StoredCredential: mitRecurring()})
	require.True(t, IsTransportAmbiguous(err), "an approval without a transaction id is unproven")
	require.False(t, res.Success)

	f = newNMIFake(t, reply("response=3&response_code=300&responsetext=Duplicate transaction REFID:1"))
	_, err = f.client(t).RunSale(t.Context(), SaleParams{CustomerVaultID: "v", Amount: 1, Currency: "USD", StoredCredential: citOneTime()})
	require.ErrorIs(t, err, ErrDuplicateTransaction)
	require.True(t, IsTransportAmbiguous(err), "a duplicate refusal still requires verification")
	f = newNMIFake(t, reply("response=3&response_code=300&responsetext=Invalid amount"))
	_, err = f.client(t).RunSale(t.Context(), SaleParams{CustomerVaultID: "v", Amount: 1, Currency: "USD", StoredCredential: citOneTime()})
	require.NotErrorIs(t, err, ErrDuplicateTransaction)
	require.True(t, IsTransportAmbiguous(err))
}

func TestTransportOutcomeClassification(t *testing.T) {
	sale := func(c *NMIClient) error {
		_, err := c.RunSale(t.Context(), SaleParams{CustomerVaultID: "v", Amount: 1, Currency: "USD", StoredCredential: citOneTime()})
		return err
	}
	vault := func(c *NMIClient) error {
		_, err := c.CreateCustomerVault(t.Context(), CreateCustomerVaultData{PaymentToken: "tok"})
		return err
	}
	read := func(c *NMIClient) error { _, _, err := c.GetPayment(t.Context(), "t"); return err }
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		call      func(*NMIClient) error
		ambiguous bool
	}{
		{"direct 5xx", 502, "", sale, true},
		{"direct 4xx after send", 400, "response=3&response_code=300", sale, true},
		{"v5 mutation 5xx", 502, "", vault, true},
		{"v5 mutation undecodable 2xx", 200, "not json", vault, true},
		{"v5 mutation 4xx envelope is a clean rejection", 400, `{"message":"bad"}`, vault, false},
		{"v5 read 5xx", 502, "", read, false},
		{"v5 read undecodable", 200, "not json", read, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNMIFake(t, func(nmiCall) (int, string) { return tc.status, tc.body })
			err := tc.call(f.client(t))
			require.Error(t, err)
			require.Equal(t, tc.ambiguous, IsTransportAmbiguous(err), "%v", err)
			require.Len(t, f.Calls(), 1)
		})
	}

	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c := nmiClient(t, dead.URL)
	require.True(t, IsTransportAmbiguous(sale(c)), "a dead endpoint may have received the request")
	require.True(t, IsTransportAmbiguous(vault(c)))
	require.False(t, IsTransportAmbiguous(read(c)))
}

func TestV5RefundOutcomes(t *testing.T) {
	const sentinel = "RAW_PROVIDER_SENTINEL"
	refund := func(status int, body string) error {
		f := newNMIFake(t, func(nmiCall) (int, string) { return status, body })
		_, err := f.client(t).Refund(t.Context(), RefundParams{TransactionID: "t", Amount: 999, Currency: "USD"})
		require.Len(t, f.Calls(), 1)
		return err
	}
	err := refund(200, `{"id":"r","response":"2","response_code":"201","response_text":"`+sentinel+`"}`)
	var decline *CustomerVaultError
	require.ErrorAs(t, err, &decline)
	require.Equal(t, "do_not_honor", decline.LocalizationID)
	require.False(t, RequiresVerification(err))

	err = refund(200, `{"id":"r","response":"3","response_code":"420","response_text":"`+sentinel+`"}`)
	require.False(t, errors.As(err, &decline), "uncertainty never carries typed decline proof")
	require.True(t, RequiresVerification(err))
	require.NotContains(t, err.Error(), sentinel)

	for _, status := range []int{400, 404, 502} {
		err := refund(status, `{"type":"`+sentinel+`","message":"`+sentinel+`"}`)
		require.Error(t, err)
		require.NotContains(t, err.Error(), sentinel, "provider envelopes are never diagnostics")
		require.Equal(t, status == 404, errors.Is(err, ErrV5NotFound), status)
		require.Equal(t, status >= 500, RequiresVerification(err), status)
	}
}

func TestAddCustomerBillingEntryNamesTheAddedCard(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"billing object", `{"object":"billing","id":"B2"}`, "B2"},
		{"whole customer", `{"object":"customer","id":"v1","billing":[{"id":"B1"},{"id":"B2"}]}`, "B2"},
		{"unnamed", `{"object":"customer","id":"v1","billing":[{"id":"B1"}]}`, ""},
		{"two new entries", `{"object":"customer","id":"v1","billing":[{"id":"B1"},{"id":"B2"},{"id":"B3"}]}`, ""},
	} {
		f := newNMIFake(t, reply(tc.body))
		got, err := f.client(t).AddCustomerBillingEntry(t.Context(), "v1", CreateCustomerVaultData{PaymentToken: "tok"}, []string{"B1"})
		require.Equal(t, tc.want, got, tc.name)
		require.Equal(t, tc.want == "", IsTransportAmbiguous(err), tc.name)
		require.Equal(t, "/customers/v1/billing", f.Calls()[0].Path)
	}
}

// or#866: a cancelled caller context aborts the in-flight call; a mutation
// aborted mid-flight stays an unknown outcome.
func TestStalledGatewayHonorsCallerContext(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	client := nmiClient(t, srv.URL)
	for _, tc := range []struct {
		name     string
		mutating bool
		call     func(context.Context) error
	}{
		{"v5 read", false, func(ctx context.Context) error { _, _, err := client.GetSubscription(ctx, "s"); return err }},
		{"v5 roster page", false, func(ctx context.Context) error { _, err := client.ListCustomersPage(ctx, "", 10, ""); return err }},
		{"query search", false, func(ctx context.Context) error {
			_, err := client.SearchTransactions(ctx, QueryFilter{OrderID: "o"})
			return err
		}},
		{"direct-post sale", true, func(ctx context.Context) error {
			_, err := client.RunSale(ctx, SaleParams{CustomerVaultID: "v", Amount: 1, Currency: "USD", StoredCredential: citOneTime()})
			return err
		}},
		{"direct-post enrollment", true, func(ctx context.Context) error {
			_, err := client.AddRecurringSubscription(ctx, RecurringPaymentData{PlanID: "p", CustomerVaultID: "v", Currency: "USD", StoredCredential: citRecurring()})
			return err
		}},
		{"v5 vault create", true, func(ctx context.Context) error {
			_, err := client.CreateCustomerVault(ctx, CreateCustomerVaultData{PaymentToken: "tok"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(50*time.Millisecond, cancel)
			done := make(chan error, 1)
			go func() { done <- tc.call(ctx) }()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, tc.mutating, IsTransportAmbiguous(err), fmt.Sprint(err))
			case <-time.After(5 * time.Second):
				t.Fatal("the caller's cancel did not abort the in-flight request")
			}
		})
	}
}
