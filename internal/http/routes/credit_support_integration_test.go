//go:build integration

package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/router"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { dbtest.RunMain(m) }

type creditSupportGate struct{ actor string }

func (g creditSupportGate) Authorize(_ context.Context, r *http.Request, permission string) (billingauth.Principal, error) {
	mid, err := merchant.ParseID(r.Header.Get("X-Test-Merchant"))
	if err != nil {
		return billingauth.Principal{}, billingauth.GateError{Status: 403, Message: "merchant required"}
	}
	role := r.Header.Get("X-Test-Role")
	allowed := role == "owner" || ((role == "viewer" || role == "grant-only") && permission == controlplane.PermMerchantCustomerSettingsRead) || (role == "grant-only" && permission == controlplane.PermMerchantCreditsGrant)
	if !allowed {
		return billingauth.Principal{}, billingauth.GateError{Status: 403, Message: "permission denied"}
	}
	return billingauth.Principal{MerchantID: mid, UserContext: billingauth.UserContext{UserID: g.actor}}, nil
}

func TestCreditSupportHTTPGrantListRevokeIsolation(t *testing.T) {
	ctx := context.Background()
	database := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	admin := dbtest.SharedSuperuserPGXPool(t)
	otherMerchant := uuid.New()
	_, err := admin.Exec(ctx, `INSERT INTO openrails.merchants(id,slug,status) VALUES($1,$2,'active')`, otherMerchant, "credit-"+otherMerchant.String())
	require.NoError(t, err)
	redis := dbtest.NewSharedRedisClient(t)
	t.Cleanup(func() { _ = redis.Close() })
	require.NoError(t, redis.Ping(ctx).Err())
	ms := money.NewMoneyService(database)
	directory, err := merchants.NewDirectoryService(db.WrapPool(database.Pool(), ""))
	require.NoError(t, err)
	rt := &app.Runtime{Merchants: directory, DB: database, MoneyService: ms, EntitlementService: entitlements.NewEntitlementService(database), RedisClient: redis}
	mux := http.NewServeMux()
	RegisterMerchantActionRoutes(router.NewMux(mux, "/v1/merchant", rt), rt, Options{Gate: creditSupportGate{actor: uuid.NewString()}})
	customer := identity.CustomerID(uuid.New())
	mid := dbtest.TestMerchantID.UUID()
	path := fmt.Sprintf("/v1/merchant/customers/%s/credits", customer.UUID())
	request := func(method, path, role string, merchantID uuid.UUID, body any) (int, map[string]any) {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Test-Role", role)
		req.Header.Set("X-Test-Merchant", merchantID.String())
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var result map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result), rec.Body.String())
		return rec.Code, result
	}
	input := map[string]any{"currency": "USD", "amount": 1000000, "source_id": uuid.NewString(), "description": "support credit"}
	code, body := request(http.MethodPost, path, "viewer", mid, input)
	require.Equal(t, 403, code, body)
	code, body = request(http.MethodPost, path, "owner", mid, input)
	require.Equal(t, 200, code, body)
	grant, ok := body["ID"].(string)
	require.True(t, ok, "creation response: %v", body)
	code, page := request(http.MethodGet, path+"?currency=USD&limit=1", "viewer", mid, nil)
	require.Equal(t, 200, code, page)
	require.Equal(t, false, page["can_grant"])
	require.Equal(t, false, page["can_revoke"])
	require.EqualValues(t, 1, page["total"])
	code, page = request(http.MethodGet, path+"?currency=USD", "grant-only", mid, nil)
	require.Equal(t, 200, code, page)
	require.Equal(t, true, page["can_grant"])
	require.Equal(t, false, page["can_revoke"])
	revokePath := path + "/" + grant
	code, body = request(http.MethodDelete, revokePath, "grant-only", mid, map[string]string{"reason": "unauthorized"})
	require.Equal(t, 403, code, body)
	code, page = request(http.MethodGet, path+"?currency=USD", "owner", otherMerchant, nil)
	require.Equal(t, 200, code, page)
	require.EqualValues(t, 0, page["total"])
	code, body = request(http.MethodDelete, revokePath, "owner", otherMerchant, map[string]string{"reason": "wrong merchant"})
	require.Equal(t, 404, code, body)
	wrongCustomer := fmt.Sprintf("/v1/merchant/customers/%s/credits/%s", uuid.New(), grant)
	code, body = request(http.MethodDelete, wrongCustomer, "owner", mid, map[string]string{"reason": "wrong customer"})
	require.Equal(t, 404, code, body)
	// Real Redis admission reserves capacity while holding the same PG row lock.
	gate := spendgate.New(redis)
	holdID := uuid.NewString()
	mctx := merchant.WithID(ctx, merchant.ID(mid))
	require.NoError(t, database.RunInMerchantScope(mctx, merchant.ID(mid), "credit support hold", func(ctx context.Context) error {
		return ms.WithLockedAdmissionCapacity(ctx, customer, "USD", func(cap money.AdmissionCapacity) error {
			decision, err := gate.Admit(ctx, spendgate.AdmitInput{Merchant: mid.String(), Customer: customer.UUID().String(), Currency: "USD", RequestID: holdID, Cost: 100, AccountBalance: cap.Balance - cap.Held, HoldTTL: time.Minute})
			if err != nil {
				return err
			}
			require.True(t, decision.Allowed)
			return nil
		})
	}))
	code, body = request(http.MethodDelete, revokePath, "owner", mid, map[string]string{"reason": "held"})
	require.Equal(t, 409, code, body)
	require.Equal(t, "credit_grant_held", body["error"].(map[string]any)["code"])
	require.NoError(t, gate.Release(ctx, spendgate.ReleaseInput{Merchant: mid.String(), Customer: customer.UUID().String(), Currency: "USD", RequestID: holdID}))
	code, body = request(http.MethodDelete, revokePath, "owner", mid, map[string]string{"reason": "support removal"})
	require.Equal(t, 200, code, body)
	require.Equal(t, "revoked", body["grant"].(map[string]any)["state"])
	code, body = request(http.MethodDelete, revokePath, "owner", mid, map[string]string{"reason": "retry"})
	require.Equal(t, 200, code, body)
	require.Equal(t, true, body["replayed"])
	code, ledger := request(http.MethodGet, fmt.Sprintf("/v1/merchant/customers/%s/credit-transactions?currency=USD&limit=1", customer.UUID()), "viewer", mid, nil)
	require.Equal(t, 200, code, ledger)
	require.EqualValues(t, 2, ledger["total"])
	require.EqualValues(t, -1000000, ledger["transactions"].([]any)[0].(map[string]any)["amount"])
	// Registry scales are authoritative: native JPY differs from USD; custom
	// credits have their own scale and must not run invoice/owed calculations.
	for _, unit := range []struct {
		code     string
		decimals int
	}{{"JPY", 4}, {"test/support-" + uuid.NewString(), 2}} {
		if unit.decimals == 2 {
			_, err = admin.Exec(ctx, `INSERT INTO openrails.custom_credit_types(id,merchant_id,name,decimals,active) VALUES(uuidv7(),$1,$2,2,true)`, mid, strings.TrimPrefix(unit.code, "test/"))
			require.NoError(t, err)
		}
		code, body = request(http.MethodPost, path, "owner", mid, map[string]any{"currency": unit.code, "amount": 125, "source_id": uuid.NewString()})
		require.Equal(t, 200, code, body)
		code, page = request(http.MethodGet, path+"?currency="+url.QueryEscape(unit.code), "viewer", mid, nil)
		require.Equal(t, 200, code, page)
		require.EqualValues(t, unit.decimals, page["unit_decimals"])
		require.EqualValues(t, 125, page["grants"].([]any)[0].(map[string]any)["amount"])
	}
	code, profile := request(http.MethodGet, fmt.Sprintf("/v1/merchant/customers/%s", customer.UUID()), "viewer", mid, nil)
	require.Equal(t, 200, code, profile)
	for _, item := range profile["credit_balance"].([]any) {
		balance := item.(map[string]any)
		if balance["currency"] == "JPY" {
			require.EqualValues(t, 4, balance["decimal_places"])
		}
		if strings.HasPrefix(balance["currency"].(string), "test/support-") {
			require.EqualValues(t, 2, balance["decimal_places"])
			require.EqualValues(t, 0, balance["outstanding_owed_amount"])
		}
	}

}
