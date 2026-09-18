//go:build integration

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/stretchr/testify/require"
)

func newPaymentDefaultsFixture(t *testing.T) *findingsFixture {
	fx := newFindingsFixture(t)
	fx.rt.PaymentMethodService = paymentmethods.NewPaymentMethodService(fx.dbi)
	fx.rt.MoneyService = money.NewMoneyService(fx.dbi, fx.rt.Clock)
	return fx
}
func seedDefaultMethod(t *testing.T, fx *findingsFixture, customer uuid.UUID) *models.PaymentMethod {
	t.Helper()
	pm := &models.PaymentMethod{ID: uuid.New(), CustomerID: customer, Rail: models.RailNMI, PspID: fx.pspFor("nmi"), RailCustomerRef: "default-" + uuid.NewString(), RebillDriver: models.RebillDriverProvider, CreatedAt: fx.rt.Clock.Now(), UpdatedAt: fx.rt.Clock.Now()}
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(fx.dbi).Create(fx.ctx, pm))
	return pm
}
func readDefaultMethods(t *testing.T, fx *findingsFixture, customer uuid.UUID, handler func(*httprequest.Request)) map[string][]string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/payment-methods", nil).WithContext(fx.ctx)
	req.SetPathValue("customer_id", customer.String())
	rec := httptest.NewRecorder()
	hr := httprequest.NewHTTP(rec, req, fx.rt)
	hr.SetUserContext(billingauth.UserContext{UserID: customer.String()})
	handler(hr)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var response struct {
		Data    []paymentMethodResponse `json:"data"`
		Methods []paymentMethodResponse `json:"payment_methods"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	methods := response.Data
	if methods == nil {
		methods = response.Methods
	}
	out := make(map[string][]string)
	for _, method := range methods {
		out[method.ID.String()] = method.CollectionDefaultCurrencies
	}
	return out
}

func setDefaultMethodHTTP(t *testing.T, fx *findingsFixture, customer uuid.UUID, currency, methodID string) (int, map[string]string) {
	t.Helper()
	body := `{"currency":"` + currency + `","payment_method_id":"` + methodID + `"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/me/collection-payment-method", strings.NewReader(body)).WithContext(fx.ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	hr := httprequest.NewHTTP(rec, req, fx.rt)
	hr.SetUserContext(billingauth.UserContext{UserID: customer.String()})
	SetMyCollectionPaymentMethod(hr)
	out := map[string]string{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestSavedMethodCollectionDefaultsAreCurrencyAndCustomerScoped(t *testing.T) {
	fx := newPaymentDefaultsFixture(t)
	payer := identity.CustomerID(fx.customer)
	subscriptionMethod := seedDefaultMethod(t, fx, fx.customer)
	usd := seedDefaultMethod(t, fx, fx.customer)
	eur := seedDefaultMethod(t, fx, fx.customer)
	for _, currencies := range readDefaultMethods(t, fx, fx.customer, ListPaymentMethods) {
		require.Empty(t, currencies, "no default is inferred from method order")
	}
	subID := fx.seedActiveSubscription("provider-default-" + uuid.NewString())
	fx.exec(`UPDATE openrails.subscriptions SET payment_method_id=$1 WHERE id=$2`, subscriptionMethod.ID, subID)
	code, body := setDefaultMethodHTTP(t, fx, fx.customer, "usd", openrails.PaymentMethodID(usd.ID).String())
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, map[string]string{"currency": "USD", "payment_method_id": openrails.PaymentMethodID(usd.ID).String()}, body)
	code, body = setDefaultMethodHTTP(t, fx, fx.customer, "EUR", openrails.PaymentMethodID(eur.ID).String())
	require.Equal(t, http.StatusOK, code, body)
	otherCustomer := uuid.New()
	fx.exec(`INSERT INTO openrails.customers(id,merchant_id) VALUES($1,$2)`, otherCustomer, fx.merchant)
	other := seedDefaultMethod(t, fx, otherCustomer)
	require.NoError(t, fx.rt.MoneyService.SetInvoiceCollectionPaymentMethod(fx.ctx, identity.CustomerID(otherCustomer), "JPY", other.ID))
	require.ErrorIs(t, fx.rt.MoneyService.SetInvoiceCollectionPaymentMethod(fx.ctx, payer, "JPY", other.ID), money.ErrCollectionPaymentMethodInvalid)
	code, body = setDefaultMethodHTTP(t, fx, fx.customer, "JPY", openrails.PaymentMethodID(other.ID).String())
	require.Equal(t, http.StatusBadRequest, code, "another customer's method is refused over HTTP: %v", body)
	code, _ = setDefaultMethodHTTP(t, fx, fx.customer, "USD", "not-a-method")
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = setDefaultMethodHTTP(t, fx, fx.customer, "XYZ", openrails.PaymentMethodID(usd.ID).String())
	require.Equal(t, http.StatusBadRequest, code, "unknown currency is a client error")
	for _, handler := range []func(*httprequest.Request){ListPaymentMethods, GetAdminUserPaymentMethods, GetAdminUserBillingProfile} {
		got := readDefaultMethods(t, fx, fx.customer, handler)
		require.Len(t, got, 3)
		require.Empty(t, got[openrails.PaymentMethodID(subscriptionMethod.ID).String()], "provider subscription method is not a collection default")
		require.Equal(t, []string{"USD"}, got[openrails.PaymentMethodID(usd.ID).String()])
		require.Equal(t, []string{"EUR"}, got[openrails.PaymentMethodID(eur.ID).String()])
	}
	otherMethods := readDefaultMethods(t, fx, otherCustomer, ListPaymentMethods)
	require.Len(t, otherMethods, 1)
	require.Equal(t, []string{"JPY"}, otherMethods[openrails.PaymentMethodID(other.ID).String()])
	foreign := newPaymentDefaultsFixture(t)
	foreignMethod := seedDefaultMethod(t, foreign, foreign.customer)
	require.NoError(t, foreign.rt.MoneyService.SetInvoiceCollectionPaymentMethod(foreign.ctx, identity.CustomerID(foreign.customer), "USD", foreignMethod.ID))
	require.Empty(t, readDefaultMethods(t, fx, foreign.customer, ListPaymentMethods))
	require.ErrorIs(t, fx.rt.MoneyService.SetInvoiceCollectionPaymentMethod(fx.ctx, payer, "USD", foreignMethod.ID), money.ErrCollectionPaymentMethodInvalid)
	code, _ = setDefaultMethodHTTP(t, fx, fx.customer, "USD", openrails.PaymentMethodID(foreignMethod.ID).String())
	require.Equal(t, http.StatusBadRequest, code, "another merchant's method is refused over HTTP")
	// Use the real local delete used by durable provider-delete finalization.
	// Its FK must clear USD without touching EUR or the provider subscription.
	require.NoError(t, fx.rt.PaymentMethodService.Delete(fx.ctx, usd.ID))
	got := readDefaultMethods(t, fx, fx.customer, ListPaymentMethods)
	require.NotContains(t, got, openrails.PaymentMethodID(usd.ID).String())
	require.Equal(t, []string{"EUR"}, got[openrails.PaymentMethodID(eur.ID).String()])
	row, err := fx.dbi.Gen(fx.ctx).GetMoneyAccountSettings(fx.ctx, gen.GetMoneyAccountSettingsParams{MerchantID: fx.merchant, CustomerID: fx.customer, Currency: "USD"})
	require.NoError(t, err)
	require.Nil(t, row.CollectionPaymentMethodID)
	var stillDefault uuid.UUID
	require.NoError(t, fx.dbi.Pool().QueryRow(fx.ctx, `SELECT payment_method_id FROM openrails.subscriptions WHERE id=$1`, subID).Scan(&stillDefault))
	require.Equal(t, subscriptionMethod.ID, stillDefault)
	_, err = fx.dbi.Gen(fx.ctx).SetMoneyAccountCollectionPaymentMethod(fx.ctx, gen.SetMoneyAccountCollectionPaymentMethodParams{MerchantID: fx.merchant, CustomerID: fx.customer, Currency: "EUR", PaymentMethodID: nil})
	require.NoError(t, err)
	got = readDefaultMethods(t, fx, fx.customer, GetAdminUserBillingProfile)
	require.Empty(t, got[openrails.PaymentMethodID(eur.ID).String()])
}

func TestCollectionDefaultRemovalDoesNotChooseAnotherSavedMethod(t *testing.T) {
	fx := newPaymentDefaultsFixture(t)
	payer := identity.CustomerID(fx.customer)
	explicit := seedDefaultMethod(t, fx, fx.customer)
	fallback := seedDefaultMethod(t, fx, fx.customer)
	require.NoError(t, fx.rt.MoneyService.SetInvoiceCollectionPaymentMethod(fx.ctx, payer, "USD", explicit.ID))
	got := readDefaultMethods(t, fx, fx.customer, ListPaymentMethods)
	require.Equal(t, []string{"USD"}, got[openrails.PaymentMethodID(explicit.ID).String()])
	require.Empty(t, got[openrails.PaymentMethodID(fallback.ID).String()])
	require.NoError(t, fx.rt.PaymentMethodService.Delete(fx.ctx, explicit.ID))
	got = readDefaultMethods(t, fx, fx.customer, ListPaymentMethods)
	require.Empty(t, got[openrails.PaymentMethodID(fallback.ID).String()], "removing the explicit collection method must not select another saved method")
	require.NoError(t, fx.rt.PaymentMethodService.Delete(fx.ctx, fallback.ID))
	defaults, err := fx.rt.MoneyService.CollectionPaymentMethodCurrencies(fx.ctx, payer)
	require.NoError(t, err)
	require.Empty(t, defaults)
}
