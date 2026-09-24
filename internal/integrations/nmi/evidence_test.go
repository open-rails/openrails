package nmi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

func nmResponse(parts ...string) string {
	return "<nm_response>" + strings.Join(parts, "") + "</nm_response>"
}

func saleTxn(id, order, success, code, date string) string {
	return fmt.Sprintf(`<transaction><transaction_id>%s</transaction_id><order_id>%s</order_id><currency>USD</currency><action><action_type>sale</action_type><amount>5.00</amount><success>%s</success><date>%s</date><response_code>%s</response_code><response_text>text-%s</response_text></action></transaction>`, id, order, success, date, code, code)
}

func jsonBody(t *testing.T, v any) string {
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	return string(raw)
}

func TestProbeCredentialsIsOneBoundedRead(t *testing.T) {
	for body, ok := range map[string]bool{
		`<?xml version="1.0"?><nm_response></nm_response>`:                                 true,
		`<nm_response><error_response>Invalid security key</error_response></nm_response>`: false,
		`<html></html>`: false,
		`not xml`:       false,
	} {
		f := newNMIFake(t, reply(body))
		err := f.client(t).ProbeCredentials(t.Context())
		require.Equal(t, ok, err == nil, body)
		if strings.Contains(body, "error_response") {
			require.ErrorIs(t, err, ErrCredentialsRejected)
		}
		c := f.Calls()[0]
		require.Equal(t, "/query", c.Path)
		require.Equal(t, "transaction", c.Form.Get("report_type"))
		require.Equal(t, "1", c.Form.Get("result_limit"))
		require.Equal(t, "wire-key", c.Form.Get("security_key"))
	}
}

// Only a simulator approves the non-issued test card: approved = simulated,
// declined = live, request-level error or transport failure = indeterminate.
func TestProbeTestModeAndSandboxArming(t *testing.T) {
	probe := func(t *testing.T, response string) *nmiFake {
		return newNMIFake(t, func(c nmiCall) (int, string) {
			return 200, `{"object":"transaction","id":"99001","response":"` + response + `","response_text":"PROBE","response_code":"100"}`
		})
	}
	for response, want := range map[string]TestModeProbeResult{"1": ProbeSimulated, "2": ProbeLive, "3": ProbeIndeterminate} {
		f := probe(t, response)
		c := f.client(t)
		orders := map[string]bool{}
		for range 3 {
			got, err := c.ProbeTestMode(t.Context())
			require.Equal(t, want, got)
			require.Equal(t, want == ProbeIndeterminate, err != nil)
		}
		for _, call := range f.Calls() {
			if call.Path != "/payments/auth" {
				require.Equal(t, "/payments/99001/void", call.Path, "only an approved probe is voided")
				require.Equal(t, "1", response)
				continue
			}
			var req struct {
				Amount         json.Number `json:"amount"`
				PaymentDetails struct {
					CardNumber string `json:"card_number"`
				} `json:"payment_details"`
				OrderDetails struct {
					ID string `json:"id"`
				} `json:"order_details"`
			}
			require.NoError(t, json.Unmarshal([]byte(call.Body), &req))
			cents, err := moneyutil.ParseDecimalToCents(req.Amount.String())
			require.NoError(t, err)
			require.True(t, cents >= 101 && cents <= 199, "probe amount %s stays in [1.01,1.99]", req.Amount)
			require.Equal(t, moneyutil.FormatCentsDecimal(cents), req.Amount.String())
			require.Equal(t, probeTestCard, req.PaymentDetails.CardNumber)
			require.True(t, strings.HasPrefix(req.OrderDetails.ID, probeOrderIDPrefix))
			orders[req.OrderDetails.ID] = true
		}
		require.Len(t, orders, 3, "each probe carries a unique order id so duplicate detection never trips")
	}

	dead := probe(t, "1")
	c := dead.client(t)
	dead.Close()
	got, err := c.ProbeTestMode(t.Context())
	require.Error(t, err)
	require.Equal(t, ProbeIndeterminate, got)

	for response, want := range map[string]error{"1": nil, "2": ErrLiveCredentialsUnderTestMode, "3": nil} {
		f := probe(t, response)
		c := f.client(t)
		err := CheckTestModeArm(t.Context(), c)
		switch {
		case response == "1":
			require.NoError(t, err)
		case want != nil:
			require.ErrorIs(t, err, want)
		default:
			require.ErrorContains(t, err, "refusing to arm")
		}
		f.Close()
		require.ErrorContains(t, CheckTestModeArm(t.Context(), c), "refusing to arm", "a previous verdict never arms without a fresh probe")
	}
}

func TestSaleProbesClassifyByOrderAndPeriod(t *testing.T) {
	since := time.Date(2026, 6, 9, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, body string
		since      time.Time
		check      func(t *testing.T, r SaleProbeResult)
	}{
		{"success inside the period", nmResponse(saleTxn("txn1", "order", "1", "100", "20260610120000")), since, func(t *testing.T, r SaleProbeResult) {
			require.True(t, r.SuccessFound)
			require.Equal(t, "txn1", r.SuccessTransactionID)
			require.Equal(t, time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC), r.SuccessAt)
			require.Equal(t, []string{"5.00", "USD"}, []string{r.SuccessAmount, r.SuccessCurrency})
		}},
		{"latest decline carries the evidence", nmResponse(saleTxn("d1", "order", "0", "202", "20260608120000"), saleTxn("d2", "order", "0", "252", "20260610120000"), saleTxn("d0", "order", "0", "201", "20260607120000")), time.Time{}, func(t *testing.T, r SaleProbeResult) {
			require.False(t, r.SuccessFound)
			require.True(t, r.DeclineFound)
			require.Equal(t, "d2", r.DeclineTransactionID)
			require.Equal(t, 252, r.DeclineResponseCode)
			require.Equal(t, "text-252", r.DeclineReason)
			require.Len(t, r.Sales, 3)
		}},
		{"any success beats declines", nmResponse(saleTxn("d1", "order", "0", "202", "20260610120000"), saleTxn("s1", "order", "1", "100", "20260609120000")), time.Time{}, func(t *testing.T, r SaleProbeResult) {
			require.True(t, r.SuccessFound)
			require.Equal(t, "s1", r.SuccessTransactionID)
		}},
		{"signup sale before the period is filtered client side", nmResponse(saleTxn("signup", "order", "1", "100", "20260101120000")), since, func(t *testing.T, r SaleProbeResult) {
			require.False(t, r.SuccessFound || r.DeclineFound)
			require.Empty(t, r.Sales)
		}},
		{"undated action still counts", nmResponse(saleTxn("s1", "order", "1", "100", "garbled")), since, func(t *testing.T, r SaleProbeResult) {
			require.True(t, r.SuccessFound)
			require.True(t, r.SuccessAt.IsZero())
		}},
		{"another order is ignored", nmResponse(saleTxn("other", "different", "1", "100", "20260610120000")), time.Time{}, func(t *testing.T, r SaleProbeResult) {
			require.False(t, r.SuccessFound)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNMIFake(t, reply(tc.body))
			r, err := f.client(t).ProbeSalesByOrderID(t.Context(), "order", tc.since)
			require.NoError(t, err)
			tc.check(t, r)
			form := f.Calls()[0].Form
			require.Equal(t, "order", form.Get("order_id"))
			if tc.since.IsZero() {
				require.NotContains(t, form, "start_date")
			} else {
				require.Equal(t, "20260609000000", form.Get("start_date"))
			}
		})
	}

	f := newNMIFake(t, reply(nmResponse(`<error_response>Invalid security key</error_response>`)))
	_, err := f.client(t).ProbeSalesByOrderID(t.Context(), "order", time.Time{})
	require.ErrorContains(t, err, "Invalid security key")

	f = newNMIFake(t, reply(nmResponse(saleTxn("old", "order", "1", "100", "20250101120000"))))
	id, found, err := f.client(t).FindSuccessfulSaleByOrderID(t.Context(), "order")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "old", id, "the manual-rebill verifier has no date bound")

	f = newNMIFake(t, reply(nmResponse(saleTxn("renewal", "legacy-order", "1", "100", "20260610120000"))))
	r, err := f.client(t).ProbeSalesBySubscriptionID(t.Context(), "sub-1", time.Time{})
	require.NoError(t, err)
	require.True(t, r.SuccessFound, "an imported schedule's own order reference still matches")
	require.Equal(t, "sub-1", f.Calls()[0].Form.Get("subscription_id"))
	require.NotContains(t, f.Calls()[0].Form, "order_id")
}

func TestRecurringLivenessTreatsTombstoneAndAbsenceAsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		found  bool
		next   time.Time
		err    bool
	}{
		{"live", 200, `{"id":"s","delayed_condition":"active","next_billing_date":"2026-07-01"}`, true, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), false},
		{"iso timestamp", 200, `{"id":"s","next_billing_date":"2026-07-01T00:00:00.000Z"}`, true, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), false},
		{"unparseable date", 200, `{"id":"s","next_billing_date":"soon"}`, true, time.Time{}, false},
		{"deletion tombstone", 200, `{"id":"s","delayed_condition":"inactive","next_billing_date":"2026-07-29"}`, false, time.Time{}, false},
		{"never existed", 404, `{}`, false, time.Time{}, false},
		{"gateway error", 500, `{}`, false, time.Time{}, true},
	} {
		f := newNMIFake(t, func(nmiCall) (int, string) { return tc.status, tc.body })
		got, err := f.client(t).GetRecurringLiveness(t.Context(), "s")
		require.Equal(t, tc.err, err != nil, tc.name)
		require.Equal(t, RecurringLiveness{Found: tc.found, NextChargeDate: tc.next}, got, tc.name)
		require.Equal(t, "/subscriptions/s", f.Calls()[0].Path)
	}
}

func TestRecurringPlanReadsParseExactMoneyTerms(t *testing.T) {
	plans := map[string]string{
		"/plans/usd":     `{"id":"usd","plan_name":"Premium","plan_amount":"9.99","plan_payments":"12","day_frequency":"30"}`,
		"/plans/month":   `{"id":"month","plan_name":"Monthly","plan_amount":"5.00","plan_payments":"","day_frequency":"0","month_frequency":"1"}`,
		"/plans/yen":     `{"id":"yen","plan_name":"JPY","plan_amount":"4.00","day_frequency":"30"}`,
		"/plans/inexact": `{"id":"inexact","plan_name":"X","plan_amount":"9.999"}`,
		"/plans/badpay":  `{"id":"badpay","plan_amount":"1.00","plan_payments":"-1"}`,
	}
	f := newNMIFake(t, func(c nmiCall) (int, string) {
		if body, ok := plans[c.Path]; ok {
			return 200, body
		}
		return 404, `{}`
	})
	c := f.client(t)
	twelve := 12
	for _, tc := range []struct {
		id, currency string
		want         RecurringPlanDetail
		err          bool
	}{
		{"usd", "USD", RecurringPlanDetail{Found: true, ID: "usd", Name: "Premium", AmountCents: 999, DayFrequency: 30, Payments: &twelve}, false},
		{"month", "USD", RecurringPlanDetail{Found: true, ID: "month", Name: "Monthly", AmountCents: 500}, false},
		{"yen", "JPY", RecurringPlanDetail{Found: true, ID: "yen", Name: "JPY", AmountCents: 4, DayFrequency: 30}, false},
		{"inexact", "USD", RecurringPlanDetail{Found: true, Name: "X"}, true},
		{"badpay", "USD", RecurringPlanDetail{}, true},
		{"missing", "USD", RecurringPlanDetail{}, false},
	} {
		got, err := c.GetRecurringPlanDetailByID(t.Context(), tc.id, tc.currency)
		require.Equal(t, tc.err, err != nil, tc.id)
		require.Equal(t, tc.want, got, tc.id)
	}
	found, name, cents, err := c.GetRecurringPlanByID(t.Context(), "usd", "USD")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "Premium", name)
	require.EqualValues(t, 999, cents)
}

func TestListRecurringPlansFollowsCursorsAndRefusesLoops(t *testing.T) {
	pages := map[string]string{
		"":  `{"plans":[{"id":"a"}],"next_cursor":"7","has_more":true}`,
		"7": `{"plans":[],"next_cursor":8,"has_more":true}`,
		"8": `{"plans":[{"id":"b"}],"next_cursor":null,"has_more":false}`,
	}
	f := newNMIFake(t, func(c nmiCall) (int, string) { return 200, pages[c.Query.Get("cursor")] })
	plans, err := f.client(t).ListRecurringPlans(t.Context())
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, []string{plans[0].ID, plans[1].ID})
	require.Len(t, f.Calls(), 3, "an empty page mid-stream does not end the walk")

	loop := newNMIFake(t, reply(`{"plans":[{"id":"a"}],"next_cursor":"7","has_more":true}`))
	_, err = loop.client(t).ListRecurringPlans(t.Context())
	require.ErrorContains(t, err, "repeated cursor")
}

func TestEnrollmentEvidenceRequiresOneCorrelatedSchedule(t *testing.T) {
	const record = `<subscription id="123"><subscription_id>123</subscription_id><plan><plan_id>plan</plan_id></plan><orderid>op</orderid><ponumber>op</ponumber><next_charge_date>2026-10-21</next_charge_date></subscription>`
	for _, tc := range []struct {
		name, record string
		valid        bool
	}{
		{"exact schedule", record, true},
		{"absent", "", false},
		{"ambiguous", record + record, false},
		{"wrong identity", strings.ReplaceAll(record, "123", "456"), false},
		{"contradictory identity", strings.Replace(record, `id="123"`, `id="456"`, 1), false},
		{"wrong plan", strings.ReplaceAll(record, "<plan_id>plan</plan_id>", "<plan_id>other</plan_id>"), false},
		{"missing order", strings.Replace(record, "<orderid>op</orderid>", "", 1), false},
		{"missing next charge", strings.Replace(record, "<next_charge_date>2026-10-21</next_charge_date>", "", 1), false},
		{"malformed", "<subscription>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNMIFake(t, func(c nmiCall) (int, string) {
				if c.Method == http.MethodGet {
					return 200, `{"id":"123","customer_vault_id":"vault","delayed_condition":"active","plan":{"id":"plan"}}`
				}
				require.Equal(t, "recurring", c.Form.Get("report_type"))
				require.Equal(t, "123", c.Form.Get("subscription_id"))
				return 200, "<nm_response>" + tc.record + "</nm_response>"
			})
			facts, found, err := f.client(t).ReadEnrollmentEvidence(t.Context(), "123")
			require.Len(t, f.Calls(), 2)
			if !tc.valid {
				require.ErrorIs(t, err, ErrReceiptMismatch)
				require.False(t, found)
				return
			}
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, []string{"op", "op", "2026-10-21", "vault"}, []string{facts.OrderReference, facts.PONumber, facts.NextChargeDate, facts.Subscription.CustomerVaultID})
		})
	}
}

func TestRecurringSaleEvidenceBindsAccountOrderTransactionAndSoleBilling(t *testing.T) {
	for _, tc := range []struct {
		name, candidate, vault, billing, returnedID, currency, amount string
		duplicate, absent, invalid                                    bool
	}{
		{name: "qualified", candidate: "txn", vault: "v1", billing: "b1", returnedID: "txn", currency: "usd", amount: "5.00"},
		{name: "recovered without a response", vault: "v1", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.00"},
		{name: "wrong candidate", candidate: "other", vault: "v1", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.00", invalid: true},
		{name: "wrong vault", candidate: "txn", vault: "other", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.00", invalid: true},
		{name: "changed billing", candidate: "txn", vault: "v1", billing: "b2", returnedID: "txn", currency: "USD", amount: "5.00", invalid: true},
		{name: "wrong exact transaction", candidate: "txn", vault: "v1", billing: "b1", returnedID: "other", currency: "USD", amount: "5.00", invalid: true},
		{name: "missing currency", candidate: "txn", vault: "v1", billing: "b1", returnedID: "txn", amount: "5.00", invalid: true},
		{name: "inexact amount", candidate: "txn", vault: "v1", billing: "b1", returnedID: "txn", currency: "USD", amount: "5.001", invalid: true},
		{name: "duplicate successful order", candidate: "txn", vault: "v1", billing: "b1", duplicate: true, invalid: true},
		{name: "delayed visibility", candidate: "txn", absent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNMIFake(t, func(c nmiCall) (int, string) {
				switch c.Path {
				case "/query":
					require.Equal(t, "wire-key", c.Form.Get("security_key"))
					require.Equal(t, "accepted-order", c.Form.Get("order_id"))
					body := ""
					if !tc.absent {
						body += saleTxn("txn", "accepted-order", "1", "100", "")
					}
					if tc.duplicate {
						body += saleTxn("other", "accepted-order", "1", "100", "")
					}
					return 200, nmResponse(body)
				case "/payments/txn":
					require.Equal(t, "wire-key", c.Auth)
					return 200, jsonBody(t, v5Transaction{Object: "transaction", ID: tc.returnedID, CustomerVaultID: tc.vault, Response: "1", Currency: tc.currency, Amount: tc.amount, Actions: []v5TxnAction{{Type: "sale", Amount: tc.amount, Success: true}}})
				case "/customers/v1":
					require.Equal(t, "wire-key", c.Auth)
					return 200, fmt.Sprintf(`{"object":"customer","id":"v1","billing":[{"id":%q}]}`, tc.billing)
				}
				t.Errorf("unexpected provider request %s %s", c.Method, c.Path)
				return 500, ""
			})
			c := f.client(t)
			// Recovery works under readonly and stays pinned to the account key.
			c.SecurityKey, c.ReadOnly = "wrong-key", true
			facts, found, err := c.ReadRecurringSaleEvidence(t.Context(), "accepted-order", tc.candidate, "v1", "b1")
			switch {
			case tc.invalid:
				require.Error(t, err)
				require.False(t, found)
			case tc.absent:
				require.NoError(t, err)
				require.False(t, found)
				require.Len(t, f.Calls(), 1)
			default:
				require.NoError(t, err)
				require.True(t, found)
				require.Len(t, f.Calls(), 3)
				require.Equal(t, SaleEvidence{VaultBillingID: "b1", TransactionID: "txn", OrderReference: "accepted-order", CustomerVaultID: "v1", Amount: 500, Currency: "USD", Approved: true}, facts)
			}
		})
	}

	f := newNMIFake(t, reply(""))
	unbound, err := newClient("nmi", &config.NMIProviderSettings{SecurityKey: "k"}, true)
	require.NoError(t, err)
	unbound.QueryURL, unbound.V5BaseURL = f.URL+"/query", f.URL
	_, _, err = unbound.ReadRecurringSaleEvidence(t.Context(), "order", "txn", "v1", "b1")
	require.Error(t, err, "an unbound client has no account to read")
	_, _, err = f.client(t).ReadRecurringSaleEvidence(t.Context(), "order", "txn", "v1", "")
	require.Error(t, err)
	require.Empty(t, f.Calls())
}

func TestReceiptConfirmationRequiresExactCurrencyAmountAndVault(t *testing.T) {
	serve := func(t *testing.T, txns map[string]v5Transaction) *NMIClient {
		f := newNMIFake(t, func(c nmiCall) (int, string) {
			txn, ok := txns[strings.TrimPrefix(c.Path, "/payments/")]
			if !ok {
				return 404, `{}`
			}
			return 200, jsonBody(t, txn)
		})
		return f.client(t)
	}
	for _, tc := range []struct {
		name, currency, original, refunded, vault, response string
		minor                                               moneyutil.Cents
		ok                                                  bool
	}{
		{"dollars", "USD", "1.00", "1.00", "vault", "1", 100, true},
		{"yen", "JPY", "100.00", "100.00", "vault", "1", 100, true},
		{"full refund binds the original amount", "USD", "2.50", "-2.50", "vault", "1", 0, true},
		{"partial mismatch", "USD", "1.00", "1.00", "vault", "1", 50, false},
		{"fractional yen", "JPY", "100.01", "100.01", "vault", "1", 100, false},
		{"unparseable", "USD", "1.00", "unknown", "vault", "1", 100, false},
		{"missing currency", "", "1.00", "1.00", "vault", "1", 100, false},
		{"other vault", "USD", "1.00", "1.00", "other", "1", 100, false},
		{"not approved", "USD", "1.00", "1.00", "vault", "2", 100, false},
	} {
		c := serve(t, map[string]v5Transaction{
			"original": {ID: "original", Amount: tc.original, Currency: tc.currency, Response: "1", CustomerVaultID: "vault"},
			"refund":   {ID: "refund", Currency: tc.currency, Response: tc.response, CustomerVaultID: tc.vault, Actions: []v5TxnAction{{Type: "refund", Amount: tc.refunded, Success: true}}},
		})
		err := c.ConfirmRefund(t.Context(), "original", "refund", tc.minor, tc.currency)
		require.Equal(t, tc.ok, err == nil, "%s: %v", tc.name, err)
		if !tc.ok {
			require.ErrorIs(t, err, ErrReceiptMismatch, tc.name)
		}
	}

	sale := v5Transaction{ID: "t", Response: "1", CustomerVaultID: "v", Currency: "USD", Actions: []v5TxnAction{{Type: "sale", Amount: "5.00", Success: true}}}
	c := serve(t, map[string]v5Transaction{"t": sale})
	require.NoError(t, c.ConfirmApprovedSale(t.Context(), "t", "v", 500, "usd"))
	require.NoError(t, c.ConfirmApprovedUnvaultedSale(t.Context(), "t", 500, "USD"))
	for name, err := range map[string]error{
		"other vault":    c.ConfirmApprovedSale(t.Context(), "t", "w", 500, "USD"),
		"no vault":       c.ConfirmApprovedSale(t.Context(), "t", "", 500, "USD"),
		"other amount":   c.ConfirmApprovedSale(t.Context(), "t", "v", 499, "USD"),
		"other currency": c.ConfirmApprovedSale(t.Context(), "t", "v", 500, "EUR"),
		"absent":         c.ConfirmApprovedSale(t.Context(), "missing", "v", 500, "USD"),
	} {
		require.ErrorIs(t, err, ErrReceiptMismatch, name)
	}
	sale.Response = "2"
	require.ErrorIs(t, serve(t, map[string]v5Transaction{"t": sale}).ConfirmApprovedUnvaultedSale(t.Context(), "t", 500, "USD"), ErrReceiptMismatch)
}

func TestReadOrderAttemptsOnlyReportsDefiniteSoleDecline(t *testing.T) {
	declined := saleTxn("d1", "order", "0", "202", "")
	for _, tc := range []struct {
		name, body string
		want       OrderAttempts
		mismatch   bool
	}{
		{"nothing recorded", nmResponse(), OrderAttempts{}, false},
		{"definite decline", nmResponse(declined), OrderAttempts{Transactions: 1, Declined: true, DeclineCode: 202, DeclineTransactionID: "d1"}, false},
		{"uncertain code", nmResponse(saleTxn("d1", "order", "0", "420", "")), OrderAttempts{Transactions: 1}, false},
		{"missing code", nmResponse(saleTxn("d1", "order", "0", "", "")), OrderAttempts{Transactions: 1}, false},
		{"approved", nmResponse(saleTxn("s1", "order", "1", "100", "")), OrderAttempts{Transactions: 1}, false},
		{"two attempts", nmResponse(declined, saleTxn("d2", "order", "0", "202", "")), OrderAttempts{Transactions: 2}, false},
		{"another order", nmResponse(saleTxn("x", "other", "0", "202", "")), OrderAttempts{}, true},
	} {
		f := newNMIFake(t, reply(tc.body))
		got, err := f.client(t).ReadOrderAttempts(t.Context(), "order")
		if tc.mismatch {
			require.ErrorIs(t, err, ErrReceiptMismatch, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
	f := newNMIFake(t, reply(nmResponse(`<error_response>denied</error_response>`)))
	_, err := f.client(t).ReadOrderAttempts(t.Context(), "order")
	require.Error(t, err, "an error response is an inconclusive read, never zero attempts")
}
