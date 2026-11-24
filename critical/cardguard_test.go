//go:build integration

package critical_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

func TestCardGuard(t *testing.T) {
	t.Run("OR-J12-S12/mastercard-length-rule-keeps-real-cards-blocked", func(t *testing.T) {
		for _, tc := range []struct {
			name, value string
			blocked     bool
		}{
			{"mastercard-51", "5100000000000008", true},
			{"mastercard-52", "5200000000000007", true},
			{"mastercard-53", "5300000000000006", true},
			{"mastercard-54", "5400000000000005", true},
			{"mastercard-55", "5500000000000004", true},
			{"reference-51", "510000000000000008", false},
			{"reference-52", "520000000000000007", false},
			{"reference-53", "530000000000000006", false},
			{"reference-54", "540000000000000005", false},
			{"reference-55", "550000000000000004", false},
			{"mastercard-grouped", "5100 0000 0000 0008", true},
			{"mastercard-dashes", "5200-0000-0000-0007", true},
			{"mastercard-in-text", "reference=5300000000000006", true},
			{"bad-check-digit", "5100000000000009", false},
			{"maestro-50", "500000000000000009", true},
			{"maestro-56", "560000000000000003", true},
			{"visa", "4111111111111111", true},
			{"amex", "378282246310005", true},
			{"mastercard-two-series", "2223000048400011", true},
			{"discover", "6011111111111117", true},
			{"jcb", "3530111333300000", true},
			{"uuid", "550e8400-e29b-41d4-a716-446655440000", false},
		} {
			t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.blocked, cardguard.ContainsPAN(tc.value)) })
		}
	})
	t.Run("OR-J12-S14/a-card-number-in-any-field-of-a-card-save-or-order-payment-is-refused", func(t *testing.T) {
		// Each refusal below is its own purchase request; the purchase limit is
		// not under test.
		a, journal := newNMIWorld(t, func(w *world) {
			w.cfg.RateLimits = openrails.RateLimitsConfig{"payment": {RequestsPerMinute: 40}}
		})
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		require.Len(t, psps.Items, 1)
		psp := psps.Items[0].ID
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "firewalled", DisplayName: "Firewalled"})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
		require.NoError(t, err)
		order := func(payment billing.OrderPaymentParams, reusable *bool) billing.CreateMyOrderParams {
			return billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}, ExpectedTotal: ptr(int64(10_000_000)), Payment: &payment, Reusable: reusable}
		}
		const pan = "4111 1111 1111 1111"
		holding := func(field string) *billing.BillingDetails {
			details, address := &billing.BillingDetails{}, &billing.BillingAddress{}
			*map[string]**string{"name": &details.Name, "email": &details.Email, "phone": &details.Phone,
				"address.line1": &address.Line1, "address.line2": &address.Line2, "address.city": &address.City,
				"address.state": &address.State, "address.postal_code": &address.PostalCode, "address.country": &address.Country}[field] = ptr(pan)
			if *address != (billing.BillingAddress{}) {
				details.Address = address
			}
			return details
		}
		refused := func(path string, body any, param string, headers ...map[string]string) {
			t.Helper()
			status, raw := a.request(t, http.MethodPost, path, a.aliceToken, body, headers...)
			require.Equal(t, http.StatusBadRequest, status, "%s: %s", param, raw)
			var answer struct {
				Error struct {
					Code  billing.ErrorCode `json:"code"`
					Param *string           `json:"param"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(raw, &answer))
			require.Equal(t, billing.CodeInvalidParam, answer.Error.Code, param)
			require.Equal(t, ptr(param), answer.Error.Param)
			require.NotContains(t, string(raw), "4111", "the refusal never echoes the number")
		}
		calls := len(journal.core.Calls())
		// A token save carries no card, and an order payment names a token:
		// neither exempts any billing detail.
		for _, field := range []string{"name", "email", "phone", "address.line1", "address.line2", "address.city", "address.state", "address.postal_code", "address.country"} {
			token := journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"})
			refused("/v1/me/payment-methods", billing.CreatePaymentMethodParams{PSPID: psp, Token: token, BillingDetails: holding(field)}, "billing_details."+field)
			refused("/v1/me/orders", order(billing.OrderPaymentParams{Token: token, BillingDetails: holding(field)}, ptr(false)), "payment.billing_details."+field, map[string]string{"Idempotency-Key": "firewall-" + field})
		}
		refused("/v1/me/payment-methods", billing.CreatePaymentMethodParams{PSPID: psp, Token: "4111111111111111"}, "token")
		refused("/v1/me/orders", order(billing.OrderPaymentParams{ReturnURL: "https://critical.invalid/return?card=4111111111111111"}, nil), "payment.return_url", map[string]string{"Idempotency-Key": "firewall-return-url"})
		refused("/v1/me/orders", order(billing.OrderPaymentParams{TokenSymbol: pan}, nil), "payment.token_symbol", map[string]string{"Idempotency-Key": "firewall-token-symbol"})

		require.Len(t, journal.core.Calls(), calls, "nothing reached NMI")
		require.Empty(t, journal.core.Vaults())
		status, raw := a.request(t, http.MethodGet, "/v1/me/payment-methods", a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var methods billing.ListPage[billing.PaymentMethod]
		require.NoError(t, json.Unmarshal(raw, &methods))
		require.Empty(t, methods.Items, "nothing was stored")
		orders, err := a.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID))})
		require.NoError(t, err)
		require.Empty(t, orders.Items)
	})
	t.Run("OR-J12-S16/legitimate-values-pass-the-firewall-on-every-card-route", func(t *testing.T) {
		a, journal := newNMIWorld(t, func(w *world) {
			w.cfg.RateLimits = openrails.RateLimitsConfig{"payment": {RequestsPerMinute: 40}}
		})
		psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
		require.NoError(t, err)
		psp := psps.Items[0].ID
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "firewall-legit", DisplayName: "Firewall legit"})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
		require.NoError(t, err)
		// A German mobile in E.164 is 13 digits that pass Luhn; a UUID, a Solana
		// address and a long numeric order id hold no card either.
		const phone, solana, orderRef = "+4915112345678", "4Nd1mBQtrMJVYVfKf2PJy9NZUZdTAsp7D4xWLs4gDB4T", "1234567890123456789"
		reference := uuid.NewString()
		legit := &billing.BillingDetails{Name: ptr("Alice Buyer"), Phone: ptr(phone), Address: &billing.BillingAddress{Line1: ptr(solana), Line2: ptr(reference), Country: ptr("DE")}}
		const firewall = "a card number is accepted only in a card field"
		type answer struct {
			Error struct {
				Code    billing.ErrorCode `json:"code"`
				Message string            `json:"message"`
				Param   *string           `json:"param"`
			} `json:"error"`
		}
		read := func(raw []byte) answer {
			var out answer
			_ = json.Unmarshal(raw, &out)
			return out
		}
		refused := func(method, path string, body any, param string, headers ...map[string]string) {
			t.Helper()
			status, raw := a.request(t, method, path, a.aliceToken, body, headers...)
			require.Equal(t, http.StatusBadRequest, status, "%s %s: %s", method, param, raw)
			got := read(raw)
			require.Equal(t, firewall, got.Error.Message, param)
			require.Equal(t, ptr(param), got.Error.Param)
			require.NotContains(t, string(raw), "4111")
		}
		passed := func(status int, raw []byte) {
			t.Helper()
			require.NotEqual(t, firewall, read(raw).Error.Message, "%d: %s", status, raw)
		}

		status, raw := a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psp,
			Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"}), BillingDetails: legit})
		require.Equal(t, http.StatusCreated, status, "legitimate billing details are saved: %s", raw)
		var method billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &method))
		refused(http.MethodPatch, "/v1/me/payment-methods/"+method.ID.String(), billing.UpdatePaymentMethodParams{BillingDetails: &billing.BillingDetails{Phone: ptr("4111 1111 1111 1111")}}, "billing_details.phone")
		status, raw = a.request(t, http.MethodPatch, "/v1/me/payment-methods/"+method.ID.String(), a.aliceToken, billing.UpdatePaymentMethodParams{BillingDetails: legit})
		passed(status, raw)
		refused(http.MethodPost, "/v1/me/payment-methods/"+method.ID.String()+"/confirm", billing.ConfirmPaymentMethodParams{Capture: &billing.CustodianCaptureReference{CustodianID: reference, SessionID: "ps_" + solana, Token: "4111111111111111"}}, "capture.token")
		status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods/"+method.ID.String()+"/confirm", a.aliceToken, billing.ConfirmPaymentMethodParams{Capture: &billing.CustodianCaptureReference{CustodianID: reference, SessionID: "ps_" + solana, Token: "pm_" + solana}})
		passed(status, raw)

		status, raw = a.request(t, http.MethodPost, "/v1/me/orders", a.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: price.ID}}})
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		pay := "/v1/me/orders/" + order.ID.String() + "/pay"
		token := journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"})
		refused(http.MethodPost, pay, billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: token, BillingDetails: &billing.BillingDetails{Phone: ptr("4111111111111111")}}, ExpectedTotal: 10_000_000, Reusable: ptr(false)}, "payment.billing_details.phone", map[string]string{"Idempotency-Key": "firewall-pay-refused"})
		status, raw = a.request(t, http.MethodPost, pay, a.aliceToken, billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: token, BillingDetails: legit}, ExpectedTotal: 10_000_000, Reusable: ptr(false)}, map[string]string{"Idempotency-Key": "firewall-pay"})
		require.Equal(t, http.StatusOK, status, "legitimate billing details pay: %s", raw)
		status, raw = a.request(t, http.MethodPost, pay, a.aliceToken, billing.PayOrderParams{Payment: billing.OrderPaymentParams{ReturnURL: "https://critical.invalid/return?order=" + orderRef}, ExpectedTotal: 10_000_000}, map[string]string{"Idempotency-Key": "firewall-pay-return"})
		passed(status, raw)

		// A card number as an unknown field's name is not echoed, and a test
		// header is never stored on the card.
		status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, map[string]any{"psp_id": psp, "4111111111111111": "x"})
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		require.Equal(t, billing.CodeUnknownField, read(raw).Error.Code)
		require.NotContains(t, string(raw), "4111")
		status, raw = a.request(t, http.MethodPost, "/v1/me/payment-methods", a.aliceToken, billing.CreatePaymentMethodParams{PSPID: psp,
			Token: journal.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4343", Exp: "1235"})}, map[string]string{"X-E2E-Run-ID": "run-4111111111111111"})
		require.Equal(t, http.StatusCreated, status, string(raw))
		var tagged billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &tagged))
		var metadata []byte
		require.NoError(t, a.pool.QueryRow(t.Context(), "SELECT metadata FROM "+pgx.Identifier{a.schema, "payment_methods"}.Sanitize()+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), tagged.ID.UUID()).Scan(&metadata))
		require.NotContains(t, string(metadata), "4111")
		require.NotContains(t, string(metadata), "e2e_run_id")
	})
	t.Run("OR-J38-S36/ccbill-references-import-and-export-unchanged", func(t *testing.T) {
		iw := bootImportWorld(t, "ccbill")
		product, err := iw.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: "legacy-ccbill", DisplayName: "Legacy CCBill", Entitlements: []string{"legacy:member"}})
		require.NoError(t, err)
		price, err := iw.alphaRemote.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "legacy-monthly", PSP: "ccbill", FlexID: "681cb38f-afb9-4665-931f-2b896072178a", RecurringBillingOptionID: "0000000023", UnitAmount: ptr(int64(23_000_000)), Currency: "USD", BillingIntervalHours: ptr(720), Archived: true})
		require.NoError(t, err)
		customer := billing.CustomerID(uuid.MustParse(iw.alice.ID))
		started, ended := iw.clock.Now().Add(-60*24*time.Hour), iw.clock.Now().Add(-30*24*time.Hour)
		book := billing.DeclaredBilling{AsOf: iw.clock.Now(), DefaultPSP: billing.PSPRef{Key: "ccbill"}, Customers: []billing.DeclaredCustomer{{Customer: customer}}}
		references := []string{"510000000000000008", "520000000000000007"}
		for i, ref := range references {
			book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: fmt.Sprintf("ccbill-%d", i), Customer: customer, Price: price.ID, Rail: "ccbill", RailSubscriptionID: ref, StartedAt: started, PaidThrough: &ended, Cancel: billing.CancelEvidence{Kind: "provider_terminated", At: ended}, Evidence: json.RawMessage(fmt.Sprintf(`{"ccbill_sub_id":%s,"provider_reference":"%s"}`, ref, ref))})
		}
		result, err := iw.alphaRemote.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.ElementsMatch(t, []string{"ccbill-0", "ccbill-1"}, result.Imported)
		require.Empty(t, result.Blocked)
		result, err = iw.alphaRemote.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, result.Imported)
		require.ElementsMatch(t, []string{"ccbill-0", "ccbill-1"}, result.Skipped)
		subscriptions, err := iw.alphaRemote.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
		require.NoError(t, err)
		require.Len(t, subscriptions.Items, 2)
		found := make([]string, 0, 2)
		for _, sub := range subscriptions.Items {
			require.NotNil(t, sub.RailSubscriptionID)
			found = append(found, *sub.RailSubscriptionID)
			var raw []byte
			err := iw.pool.QueryRow(t.Context(), "SELECT gateway_response FROM "+pgx.Identifier{iw.schema, "subscriptions"}.Sanitize()+" WHERE merchant_id=$1 AND id=$2", iw.alpha.MerchantID().UUID(), sub.ID.UUID()).Scan(&raw)
			require.NoError(t, err)
			var evidence struct {
				CCBillSubID json.Number `json:"ccbill_sub_id"`
				Reference   string      `json:"provider_reference"`
			}
			require.NoError(t, json.Unmarshal(raw, &evidence))
			require.Equal(t, *sub.RailSubscriptionID, evidence.CCBillSubID.String())
			require.Equal(t, *sub.RailSubscriptionID, evidence.Reference)
		}
		require.ElementsMatch(t, references, found)
		require.NotEmpty(t, archiveBytes(t, iw.alphaRemote), "archive validation must accept the same provider references")
		for _, field := range []string{"typed-reference", "evidence", "escaped-evidence"} {
			t.Run(field, func(t *testing.T) {
				refused := book
				refused.Subscriptions = []billing.DeclaredSubscription{book.Subscriptions[0]}
				refused.Subscriptions[0].SourceID = "refused-" + field
				refused.Subscriptions[0].RailSubscriptionID = "not-a-card"
				refused.Subscriptions[0].Evidence = nil
				switch field {
				case "typed-reference":
					refused.Subscriptions[0].RailSubscriptionID = "5100000000000008"
				case "evidence":
					refused.Subscriptions[0].Evidence = json.RawMessage(`{"reference":"5200-0000-0000-0007"}`)
				case "escaped-evidence":
					refused.Subscriptions[0].Evidence = json.RawMessage(`{"reference":"\u0035\u0033\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0030\u0036"}`)
				}
				_, err := iw.alphaRemote.ImportBilling(t.Context(), refused)
				require.Error(t, err)
				var apiErr *billing.StatusError
				require.ErrorAs(t, err, &apiErr)
				require.Equal(t, http.StatusBadRequest, apiErr.Status)
				require.Equal(t, billing.CodeInvalidParam, apiErr.Code)
				require.Contains(t, apiErr.Message, "card-number-shaped")
				require.False(t, strings.Contains(apiErr.Message, "5100000000000008"))
				after, err := iw.alphaRemote.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				require.NoError(t, err)
				require.Len(t, after.Items, 2, "a refused book must not write a subscription")
			})
		}
	})
}
