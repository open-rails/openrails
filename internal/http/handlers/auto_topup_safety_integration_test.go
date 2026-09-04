//go:build integration

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func TestAutoTopupSafetyMerchantHTTPPolicyAndCustomerStatus(t *testing.T) {
	fx := newPaymentDefaultsFixture(t)
	call := func(handler func(*httprequest.Request), method, body string, customer uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/settings", bytes.NewBufferString(body)).WithContext(fx.ctx)
		req.Header.Set("Content-Type", "application/json")
		if customer != uuid.Nil {
			req.SetPathValue("customer_id", customer.String())
		}
		rec := httptest.NewRecorder()
		handler(httprequest.NewHTTP(rec, req, fx.rt))
		return rec
	}
	good := call(ServiceSetMerchantSettings, http.MethodPut, `{"auto_topup_safety":{"max_daily":2,"max_weekly":8,"max_monthly":20,"declines_before_disable":2}}`, uuid.Nil)
	require.Equal(t, http.StatusOK, good.Code, good.Body.String())
	bad := call(ServiceSetMerchantSettings, http.MethodPut, `{"auto_topup_safety":{"max_daily":0,"max_weekly":8,"max_monthly":20,"declines_before_disable":2}}`, uuid.Nil)
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.Body.String())
	payer := identity.CustomerID(uuid.New())
	_, err := fx.rt.MoneyService.Deposit(fx.ctx, money.DepositParams{CustomerID: &payer, Invoker: payer.String(), Currency: "USD", Amount: 100, Source: "seed"})
	require.NoError(t, err)
	rec := call(GetAdminUserBillingProfile, http.MethodGet, "", payer.UUID())
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var profile adminUserBillingProfile
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &profile))
	require.Len(t, profile.CreditBalance, 1)
	status := profile.CreditBalance[0].AutoTopup
	require.NotNil(t, status)
	require.Equal(t, 2, status.Policy.MaxDaily)
	require.Equal(t, 8, status.Policy.MaxWeekly)
	require.False(t, status.Enabled)
	require.Zero(t, status.Daily)
}
