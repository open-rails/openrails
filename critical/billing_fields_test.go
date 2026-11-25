//go:build integration

package critical_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

// billingShop is an NMI merchant selling one 10 USD pass, whose card forms
// ask for the billing fields it declares.
type billingShop struct {
	*authorityWorld
	nmi   *nmiJournal
	psp   billing.PSPID
	price billing.PriceID
}

func newBillingShop(t *testing.T, fields *billing.BillingFieldSettings) *billingShop {
	t.Helper()
	a, journal := newNMIWorld(t, func(w *world) {
		w.cfg.RateLimits = paymentLimitOnly
		if fields != nil {
			merchant := cfgMerchant(w.cfg, "alpha")
			merchant.Checkout = &billing.MerchantCheckoutSettings{BillingFields: fields}
			setCfgMerchant(&w.cfg, "alpha", merchant)
		}
	})
	psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
	require.NoError(t, err)
	require.Len(t, psps.Items, 1)
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "billed-pass", DisplayName: "Billed pass"})
	require.NoError(t, err)
	price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD"})
	require.NoError(t, err)
	return &billingShop{authorityWorld: a, nmi: journal, psp: psps.Items[0].ID, price: price.ID}
}

// fields are the billing fields GET /v1/me/config serves the card forms.
func (s *billingShop) fields(t *testing.T) billing.BillingFieldSettings {
	t.Helper()
	status, raw := s.request(t, http.MethodGet, "/v1/me/config", s.aliceToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	var config billing.CustomerConfig
	require.NoError(t, json.Unmarshal(raw, &config))
	return config.Payment.BillingFields
}

func (s *billingShop) token() string {
	return s.nmi.core.Tokenize(nmimock.Card{Brand: "visa", Last4: "4242", Exp: "1235"})
}

func (s *billingShop) save(t *testing.T, details *billing.BillingDetails) (int, []byte) {
	t.Helper()
	return s.request(t, http.MethodPost, "/v1/me/payment-methods", s.aliceToken, billing.CreatePaymentMethodParams{PSPID: s.psp, Token: s.token(), BillingDetails: details})
}

// buy is a one-call buy of the pass with a new card, kept so that NMI's sale
// also vaults it with its billing.
func (s *billingShop) buy(t *testing.T, details *billing.BillingDetails, key string) (int, []byte) {
	t.Helper()
	return s.request(t, http.MethodPost, "/v1/me/orders", s.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: s.price}}, ExpectedTotal: ptr(int64(10_000_000)),
		Reusable: ptr(true), Payment: &billing.OrderPaymentParams{Token: s.token(), BillingDetails: details}}, map[string]string{"Idempotency-Key": key})
}

// vaulted is the billing NMI was last sent with a new vault customer.
func (s *billingShop) vaulted(t *testing.T) map[string]any {
	t.Helper()
	created := s.nmi.core.CallsTo(http.MethodPost, "/customers", nil)
	require.NotEmpty(t, created)
	var body struct {
		Billing map[string]any `json:"billing"`
	}
	require.NoError(t, json.Unmarshal(created[len(created)-1].Body, &body))
	return body.Billing
}

// sold is the form of the last sale NMI was sent.
func (s *billingShop) sold(t *testing.T) url.Values {
	t.Helper()
	sales := s.nmi.core.CallsTo(http.MethodPost, "transact.php", func(form url.Values) bool { return form.Get("type") == "sale" })
	require.NotEmpty(t, sales)
	return sales[len(sales)-1].Form
}

func requireBillingRefusal(t *testing.T, status int, raw []byte, param string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, status, "%s: %s", param, raw)
	var answer struct {
		Error struct {
			Code  billing.ErrorCode `json:"code"`
			Type  billing.ErrorType `json:"type"`
			Param *string           `json:"param"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &answer))
	require.Equal(t, billing.CodeBillingDetailsRequired, answer.Error.Code, param)
	require.Equal(t, billing.ErrorTypeInvalidRequest, answer.Error.Type)
	require.Equal(t, ptr(param), answer.Error.Param)
}

func TestBillingFields(t *testing.T) {
	t.Run("OR-J12-S17/a-us-card-without-a-postal-code-is-refused-before-nmi", func(t *testing.T) {
		s := newBillingShop(t, nil)
		require.Equal(t, billing.BillingFieldSettings{Country: billing.BillingFieldRequired, PostalCode: billing.BillingFieldAuto}, s.fields(t))
		calls := len(s.nmi.core.Calls())

		status, raw := s.save(t, billedTo("US", ""))
		requireBillingRefusal(t, status, raw, "billing_details.address.postal_code")
		status, raw = s.save(t, &billing.BillingDetails{Name: ptr("Alice Buyer")})
		requireBillingRefusal(t, status, raw, "billing_details.address.country")
		status, raw = s.buy(t, billedTo("US", ""), "us-without-zip")
		requireBillingRefusal(t, status, raw, "payment.billing_details.address.postal_code")
		orders, err := s.alpha.ListOrders(t.Context(), billing.OrderListParams{CustomerID: billing.CustomerID(uuid.MustParse(s.alice.ID))})
		require.NoError(t, err)
		require.Empty(t, orders.Items, "a refused one-call buy makes no order")

		status, raw = s.request(t, http.MethodPost, "/v1/me/orders", s.aliceToken, billing.CreateMyOrderParams{Lines: []billing.OrderLineParams{{PriceID: s.price}}})
		require.Contains(t, []int{http.StatusOK, http.StatusCreated}, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		pay := "/v1/me/orders/" + order.ID.String() + "/pay"
		status, raw = s.request(t, http.MethodPost, pay, s.aliceToken, billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: s.token(), BillingDetails: billedTo("US", "")}, ExpectedTotal: 10_000_000},
			map[string]string{"Idempotency-Key": "pay-us-without-zip"})
		requireBillingRefusal(t, status, raw, "payment.billing_details.address.postal_code")
		status, raw = s.request(t, http.MethodPost, pay, s.aliceToken, billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: s.token()}, ExpectedTotal: 10_000_000},
			map[string]string{"Idempotency-Key": "pay-without-country"})
		requireBillingRefusal(t, status, raw, "payment.billing_details.address.country")
		require.Len(t, s.nmi.core.Calls(), calls, "nothing reached NMI")

		// With its ZIP code the card goes to NMI exactly as entered.
		status, raw = s.save(t, billedTo("US", "94105"))
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Equal(t, "94105", s.vaulted(t)["zip"])
		require.Equal(t, "US", s.vaulted(t)["country"])
		status, raw = s.request(t, http.MethodPost, pay, s.aliceToken, billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: s.token(), BillingDetails: billedTo("US", "94105")}, ExpectedTotal: 10_000_000, Reusable: ptr(true)},
			map[string]string{"Idempotency-Key": "pay-us-with-zip"})
		require.Equal(t, http.StatusOK, status, string(raw))
		require.Equal(t, "94105", s.sold(t).Get("zip"))
		require.Equal(t, "US", s.sold(t).Get("country"))
	})

	t.Run("OR-J12-S18/a-card-outside-the-us-needs-no-postal-code", func(t *testing.T) {
		s := newBillingShop(t, nil)
		status, raw := s.save(t, billedTo("DE", ""))
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Equal(t, "DE", s.vaulted(t)["country"])
		require.NotContains(t, s.vaulted(t), "zip")

		status, raw = s.buy(t, billedTo("DE", ""), "de-without-postcode")
		require.Equal(t, http.StatusCreated, status, string(raw))
		var order billing.Order
		require.NoError(t, json.Unmarshal(raw, &order))
		require.Equal(t, billing.OrderComplete, order.Status)
		require.Equal(t, "add_customer", s.sold(t).Get("customer_vault"))
		require.Equal(t, "DE", s.sold(t).Get("country"))
		require.False(t, s.sold(t).Has("zip"))

		// auto does not ask outside the US, but keeps a postal code sent.
		status, raw = s.save(t, billedTo("GB", "SW1A 1AA"))
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Equal(t, "SW1A 1AA", s.vaulted(t)["zip"])
	})

	t.Run("OR-J12-S19/hidden-billing-fields-are-not-required-and-never-sent", func(t *testing.T) {
		s := newBillingShop(t, &billing.BillingFieldSettings{Country: billing.BillingFieldHidden, PostalCode: billing.BillingFieldHidden})
		require.Equal(t, billing.BillingFieldSettings{Country: billing.BillingFieldHidden, PostalCode: billing.BillingFieldHidden}, s.fields(t))

		status, raw := s.save(t, &billing.BillingDetails{Name: ptr("Alice Buyer")})
		require.Equal(t, http.StatusCreated, status, string(raw))
		status, raw = s.save(t, billedTo("US", "94105"))
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.NotContains(t, s.vaulted(t), "country", "a hidden field sent is dropped")
		require.NotContains(t, s.vaulted(t), "zip")
		var method billing.PaymentMethod
		require.NoError(t, json.Unmarshal(raw, &method))
		require.NotNil(t, method.BillingDetails)
		require.Nil(t, method.BillingDetails.Address, "nor stored")

		status, raw = s.buy(t, nil, "hidden-without-either")
		require.Equal(t, http.StatusCreated, status, string(raw))
		require.Equal(t, "add_customer", s.sold(t).Get("customer_vault"), "the sale vaults the card")
		require.False(t, s.sold(t).Has("country"))
		require.False(t, s.sold(t).Has("zip"))
	})
}
