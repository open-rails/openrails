//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/stretchr/testify/require"
)

type bindingAuthenticator struct {
	auth.Authenticator
	calls atomic.Int64
}

func (a *bindingAuthenticator) Authenticate(r *http.Request) (auth.Verified, error) {
	a.calls.Add(1)
	return a.Authenticator.Authenticate(r)
}

func TestCustomerBindingContracts(t *testing.T) {
	t.Run("OR-J01-S09/host-mapping-preserves-source-and-owned-customer-views", func(t *testing.T) {
		a, _ := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.New())
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		price := hostProduct(t, a.alpha, "mapped-customer")
		order, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: "mapped-order"})
		require.NoError(t, err)
		link := uuid.NewString()
		credential := &bindingAuthenticator{Authenticator: a.auth.Authenticator()}
		var active atomic.Bool
		active.Store(true)
		_, err = a.beta.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		missing := billing.CustomerID(uuid.New())
		selection := openrails.CustomerRoutes{Prefix: "/account/links/{link_id}/me", Auth: credential, Resolve: func(r *http.Request, verified auth.Verified) (openrails.CustomerBinding, error) {
			actor, err := a.auth.ResolveTenantActor(r.Context(), verified)
			if err != nil {
				return openrails.CustomerBinding{}, err
			}
			if !active.Load() || actor.AccountID != a.alice.ID || actor.TenantID != a.tenant {
				return openrails.CustomerBinding{}, billing.ErrNotFound
			}
			switch r.PathValue("link_id") {
			case link:
				return openrails.CustomerBinding{MerchantID: a.alpha.MerchantID(), CustomerID: customer}, nil
			case "other-merchant":
				return openrails.CustomerBinding{MerchantID: a.beta.MerchantID(), CustomerID: customer}, nil
			case "missing-customer":
				return openrails.CustomerBinding{MerchantID: a.alpha.MerchantID(), CustomerID: missing}, nil
			case "resolver-outage":
				return openrails.CustomerBinding{}, errors.Join(auth.ErrUnavailable, errors.New("private resolver failure"))
			case "empty-binding":
				return openrails.CustomerBinding{}, nil
			default:
				return openrails.CustomerBinding{}, billing.ErrNotFound
			}
		}}
		routes, err := a.engine.CustomerRoutes(selection)
		require.NoError(t, err)
		mux := http.NewServeMux()
		for _, route := range routes {
			require.NotContains(t, route.Path, "network-link")
			require.NotContains(t, route.Path, "/admin/")
			mux.Handle(route.Method+" "+route.Path, route.Handler)
		}
		request := httptest.NewRequest(http.MethodGet, "/account/links/"+link+"/me/orders/"+order.ID.String(), nil)
		request.Header.Set("Authorization", "Bearer "+a.aliceToken)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, request)
		require.EqualValues(t, 1, credential.calls.Load(), "the actual credential is authenticated once")
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var got billing.Order
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
		require.Equal(t, customer, got.CustomerID)
		require.NotEqual(t, a.alice.ID, got.CustomerID.String())
		require.Equal(t, order.ID, got.ID)
		server := httptest.NewServer(mux)
		defer server.Close()
		a.server = server
		status, raw := a.request(t, http.MethodGet, request.URL.Path, a.aliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Equal(t, customer, got.CustomerID)
		sdk := os.Getenv("OPENRAILS_CRITICAL_SDK_CLIENT")
		if sdk == "" {
			sdk, err = filepath.Abs("../sdk/billing-ui/dist/client.js")
			require.NoError(t, err)
		}
		sdkInput, err := json.Marshal(map[string]string{"sdk": sdk, "base": server.URL + "/account/links/" + link, "token": a.aliceToken, "order": order.ID.String(), "customer": customer.String()})
		require.NoError(t, err)
		sdkCall := exec.CommandContext(t.Context(), "node", "customer_binding_sdk.mjs")
		sdkCall.Stdin = bytes.NewReader(sdkInput)
		output, err := sdkCall.CombinedOutput()
		require.NoError(t, err, string(output))
		require.Contains(t, string(output), "/account/links/"+link+"/me/orders/"+order.ID.String())
		status, raw = a.request(t, http.MethodGet, request.URL.Path, a.bobToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.NotContains(t, string(raw), customer.String())
		for _, foreign := range []string{"other-merchant", "missing-customer", "unknown-link"} {
			status, raw = a.request(t, http.MethodGet, "/account/links/"+foreign+"/me/orders/"+order.ID.String(), a.aliceToken, nil)
			require.Equal(t, http.StatusNotFound, status, string(raw))
			require.NotContains(t, string(raw), customer.String())
		}
		for _, unavailable := range []string{"resolver-outage", "empty-binding"} {
			status, raw = a.request(t, http.MethodGet, "/account/links/"+unavailable+"/me/orders/"+order.ID.String(), a.aliceToken, nil)
			require.Equal(t, http.StatusServiceUnavailable, status, string(raw))
			require.NotContains(t, string(raw), "private resolver failure")
			require.NotContains(t, string(raw), customer.String())
		}
		_, err = a.alpha.GetCustomer(t.Context(), missing)
		require.ErrorIs(t, err, billing.ErrNotFound, "mapping does not provision a customer")
		otherCustomer := billing.CustomerID(uuid.New())
		otherOrder, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: otherCustomer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: "other-customer-order"})
		require.NoError(t, err)
		status, raw = a.request(t, http.MethodGet, "/account/links/"+link+"/me/orders/"+otherOrder.ID.String(), a.aliceToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		status, raw = a.request(t, http.MethodGet, request.URL.Path, a.aliceToken, nil, map[string]string{"OpenRails-Expected-Merchant": a.beta.MerchantID().String()})
		require.Equal(t, http.StatusPreconditionFailed, status, string(raw))
		require.Contains(t, string(raw), "merchant_mismatch")
		status, raw = a.request(t, http.MethodPost, "/account/links/"+link+"/me/orders", a.aliceToken, map[string]any{})
		require.Equal(t, http.StatusMethodNotAllowed, status, "unclassified writes are unavailable: "+string(raw))
		expired := a.signedToken(t, func(_ map[string]any, claims map[string]any) {
			claims["exp"], claims["iat"], claims["nbf"] = time.Now().Add(-time.Hour).Unix(), time.Now().Add(-2*time.Hour).Unix(), time.Now().Add(-2*time.Hour).Unix()
		})
		for _, token := range []string{"", "malformed", expired} {
			status, raw = a.request(t, http.MethodGet, request.URL.Path, token, nil)
			require.Equal(t, http.StatusUnauthorized, status, string(raw))
		}
		status, raw = a.request(t, http.MethodGet, request.URL.Path, a.alphaKey.Secret, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		active.Store(false)
		status, raw = a.request(t, http.MethodGet, request.URL.Path, a.aliceToken, nil)
		require.Equal(t, http.StatusNotFound, status, "revoked host relationship is read live: "+string(raw))
		active.Store(true)
		_, err = a.auth.RevokeAccountSessions(t.Context(), iam.UserIdentity(a.alice.ID), a.alice.ID)
		require.NoError(t, err)
		status, raw = a.request(t, http.MethodGet, request.URL.Path, a.aliceToken, nil)
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
		invalid := selection
		invalid.Prefix = "/mapped/{id}"
		_, err = a.engine.CustomerRoutes(invalid)
		require.Error(t, err, "prefix wildcards must not collide with operation parameters")
		invalid = selection
		invalid.Resolve = nil
		_, err = a.engine.CustomerRoutes(invalid)
		require.Error(t, err)
		require.NoError(t, a.engine.Close(context.Background()))
		status, raw = a.request(t, http.MethodGet, request.URL.Path, a.bobToken, nil)
		require.Equal(t, http.StatusServiceUnavailable, status, "retained mount obeys engine lifetime: "+string(raw))
	})
	t.Run("OR-J01-S09/standalone-mapping-remains-independent-of-hosted-account-owner", func(t *testing.T) {
		w := newStandaloneWorld(t, func(settings map[string]any) { settings["MappedCustomers"] = true })
		customer := w.Merchants["alpha"].CustomerID
		_, err := w.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		price := hostProduct(t, w.alpha, "standalone-mapped")
		order, err := w.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: "standalone-mapped-order"})
		require.NoError(t, err)
		path := "/__mapped/alpha/me/orders/" + order.ID.String()
		status, raw := w.request(t, http.MethodGet, path, w.AliceToken, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var got billing.Order
		require.NoError(t, json.Unmarshal(raw, &got))
		require.Equal(t, customer, got.CustomerID)
		require.NotEqual(t, w.AliceID, got.CustomerID.String())
		status, raw = w.request(t, http.MethodGet, path, w.BobToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		status, raw = w.request(t, http.MethodGet, "/v1/network/links", w.AliceToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw), "the removed hosted account route is not a fallback enrollment path")
		require.Empty(t, w.hostResult(t, map[string]any{"Op": "host-delete", "Merchant": w.Merchants["alpha"].ID.String()}, nil))
		status, raw = w.request(t, http.MethodGet, path, w.AliceToken, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), "merchant_unresolved")
	})
	t.Run("OR-J01-S10/mapped-customer-keeps-next-action-idempotency-and-original-step-up", func(t *testing.T) {
		a, provider := newPaymentWorld(t)
		customer := billing.CustomerID(uuid.New())
		_, err := a.alpha.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
		require.NoError(t, err)
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "mapped-membership", DisplayName: "Mapped membership"})
		require.NoError(t, err)
		price, err := a.alpha.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: ptr(24), AccessDurationHours: ptr(24)})
		require.NoError(t, err)
		order, err := a.alpha.CreateOrder(t.Context(), billing.CreateOrderParams{CustomerID: customer, Lines: []billing.OrderLineParams{{PriceID: price.ID}}, IdempotencyKey: "mapped-membership-order"})
		require.NoError(t, err)
		routes, err := a.engine.CustomerRoutes(openrails.CustomerRoutes{Prefix: "/mapped", Auth: a.auth.Authenticator(), Resolve: func(r *http.Request, v auth.Verified) (openrails.CustomerBinding, error) {
			actor, err := a.auth.ResolveTenantActor(r.Context(), v)
			if err != nil {
				return openrails.CustomerBinding{}, err
			}
			if actor.AccountID != a.alice.ID || actor.TenantID != a.tenant {
				return openrails.CustomerBinding{}, billing.ErrNotFound
			}
			return openrails.CustomerBinding{MerchantID: a.alpha.MerchantID(), CustomerID: customer}, nil
		}})
		require.NoError(t, err)
		mux := http.NewServeMux()
		for _, route := range routes {
			mux.Handle(route.Method+" "+route.Path, route.Handler)
		}
		a.server = httptest.NewServer(mux)
		defer a.server.Close()
		stale := authtest.StaleSession(t, a.auth, a.aliceToken)
		path := "/mapped/orders/" + order.ID.String()
		status, raw := a.request(t, http.MethodGet, path, stale, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		payment := billing.PayOrderParams{Payment: billing.OrderPaymentParams{Token: provider.core.NewPaymentMethod(stripemock.Card{Brand: "visa", Last4: "4242", Decline: "auth"})}, ExpectedTotal: 10_000_000, Reusable: ptr(true)}
		status, raw = a.request(t, http.MethodPost, path+"/pay", stale, payment, map[string]string{"Idempotency-Key": "mapped-pay"})
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
		require.Contains(t, string(raw), "step_up_required")
		require.Empty(t, provider.effects(t))
		fresh := authtest.SignIn(t, a.auth, a.alice).AccessToken
		var waiting billing.Order
		for range 2 {
			status, raw = a.request(t, http.MethodPost, path+"/pay", fresh, payment, map[string]string{"Idempotency-Key": "mapped-pay"})
			require.Equal(t, http.StatusOK, status, string(raw))
			require.NoError(t, json.Unmarshal(raw, &waiting))
			require.Equal(t, customer, waiting.CustomerID)
			require.Equal(t, billing.OrderPaymentRequiresAction, waiting.Payment.Status)
		}
		status, raw = a.request(t, http.MethodGet, path, fresh, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &waiting))
		require.NotNil(t, waiting.Payment.NextAction, "customer view carries its own provider continuation")
		staff, err := a.alpha.GetOrder(t.Context(), order.ID)
		require.NoError(t, err)
		require.Nil(t, staff.Payment.NextAction)
		secret := waiting.Payment.NextAction.Payload["client_secret"]
		status, raw = a.request(t, http.MethodGet, path, a.bobToken, nil)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		require.NotContains(t, string(raw), secret)
		intent, _, ok := strings.Cut(secret, "_secret_")
		require.True(t, ok)
		require.True(t, provider.core.Authenticate(intent))
		var completed billing.Order
		for range 2 {
			status, raw = a.request(t, http.MethodPost, path+"/confirm", fresh, map[string]any{})
			require.Equal(t, http.StatusOK, status, string(raw))
			require.NoError(t, json.Unmarshal(raw, &completed))
			require.Equal(t, billing.OrderComplete, completed.Status)
		}
		require.Len(t, provider.effects(t), 1)
		require.EqualValues(t, 1000, provider.effects(t)[0].Amount)
		require.NotNil(t, completed.Lines[0].SubscriptionID)
		subPath := "/mapped/subscriptions/" + completed.Lines[0].SubscriptionID.String()
		status, raw = a.request(t, http.MethodGet, subPath, stale, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var subscription billing.Subscription
		require.NoError(t, json.Unmarshal(raw, &subscription))
		require.Equal(t, customer, subscription.CustomerID)
		// Each mapped mutation has an explicit sensitivity decision. A valid
		// older sign-in may read its customer, but cannot mutate it through an
		// alias that accidentally lost the linked-customer step-up gate.
		for _, mutation := range []struct{ method, path string }{
			{http.MethodPost, path + "/pay"},
			{http.MethodPost, path + "/confirm"},
			{http.MethodPost, subPath + "/cancel"},
			{http.MethodPost, subPath + "/resume"},
			{http.MethodPut, subPath + "/payment-method"},
			{http.MethodPost, "/mapped/payment-methods"},
			{http.MethodDelete, "/mapped/payment-methods/" + uuid.NewString()},
			{http.MethodPost, "/mapped/agreements/" + uuid.NewString() + "/revoke"},
		} {
			status, raw = a.request(t, mutation.method, mutation.path, stale, map[string]any{})
			require.Equal(t, http.StatusUnauthorized, status, mutation.method+" "+mutation.path+": "+string(raw))
			require.Contains(t, string(raw), "step_up_required")
		}
		status, raw = a.request(t, http.MethodPost, subPath+"/cancel", stale, map[string]any{"reason": "Stop future renewal"})
		require.Equal(t, http.StatusUnauthorized, status, string(raw))
		require.Contains(t, string(raw), "step_up_required")
		status, raw = a.request(t, http.MethodPost, subPath+"/cancel", fresh, map[string]any{"reason": "Stop future renewal"})
		require.Equal(t, http.StatusOK, status, string(raw))
		var canceled billing.Subscription
		require.NoError(t, json.Unmarshal(raw, &canceled))
		require.Equal(t, billing.SubscriptionCanceled, canceled.Status)
		require.True(t, canceled.Resumable)
		require.Equal(t, subscription.CurrentPeriodEndsAt, canceled.CurrentPeriodEndsAt)
		require.Len(t, provider.effects(t), 1)
	})
}
