//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprouter "github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Issue #339 full loop: a HOST-AUTHENTICATED principal (the host-pluggable
// billingauth.DelegatedAuthenticator seam, NO control plane delegated
// verifier) drives the /v1/me money surface end-to-end against a real
// database — reads its account, configures its settings, lists its
// transactions — and another subject's data is never visible.

// hostSeamAuthenticator is the host's DelegatedAuthenticator: the host has
// already verified its own credential and supplies the explicitly mapped
// principal (merchant + subject + optional permissions).
type hostSeamAuthenticator struct {
	subject string
	perms   []string
}

func (h hostSeamAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return &billingauth.DelegatedPrincipal{
		MerchantID:   dbtest.TestMerchantID.String(),
		MerchantSlug: dbtest.TestMerchantSlug,
		SubjectID:    h.subject,
		Issuer:       "https://auth.host.example",
		Permissions:  h.perms,
	}, nil
}

func newHostSeamSelfRouter(t *testing.T, suite *TestContainerSuite, subject string, perms []string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	httproutes.RegisterSelfServiceRoutes(httprouter.NewMux(mux, "/v1/me", suite.App.Runtime), suite.App.Runtime,
		middleware.DelegatedPrincipalRequired(hostSeamAuthenticator{subject: subject, perms: perms}), routesurface.AllProviderRoutes())
	return mux
}

func doHostSeamSelf(e http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer host-credential")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, req)
	return w
}

func decodeHostSeamBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body), w.Body.String())
	return body
}

func TestSelfAccountSurface_HostPrincipalFullLoopAndScoping(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := dbtest.WithTestMerchant(context.Background())

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)

	currency := "EUR"

	subjectA := uuid.NewString()
	subjectB := uuid.NewString()
	payerA := identity.CustomerIDFromString(subjectA)
	sourceID := uuid.New()

	// Fund subject A so its account has a balance and a transaction.
	_, err = svc.DepositCredits(ctx, billingservice.DepositCreditsRequest{
		CustomerID: &payerA,
		Invoker:    subjectA,
		Currency:   currency,
		Amount:     7_500_000,
		Source:     "test_seed",
		SourceID:   &sourceID,
	})
	require.NoError(t, err)

	routerA := newHostSeamSelfRouter(t, suite, subjectA, nil)
	routerB := newHostSeamSelfRouter(t, suite, subjectB, nil)

	// --- GET /v1/me/balance: A sees its own durable balance. ---
	w := doHostSeamSelf(routerA, http.MethodGet, "/v1/me/balance?currency="+currency, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	acct := decodeHostSeamBody(t, w)
	require.Equal(t, currency, acct["currency"])
	require.EqualValues(t, 7_500_000, acct["balance_amount"])
	require.NotContains(t, acct, "held_amount")
	require.NotContains(t, acct, "available_amount")

	// --- PUT /v1/me/settings: A configures its own self-imposed settings. ---
	settingsBody := fmt.Sprintf(`{
		"currency": %q,
		"low_balance_threshold": 250000
	}`, currency)
	w = doHostSeamSelf(routerA, http.MethodPut, "/v1/me/settings", settingsBody)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	stored := decodeHostSeamBody(t, w)
	require.EqualValues(t, 250_000, stored["low_balance_threshold"])
	require.NotContains(t, stored, "billing_mode")

	settings, err := svc.GetCreditAccountSettings(ctx, payerA, currency)
	require.NoError(t, err)
	require.NotNil(t, settings.LowBalanceThreshold)
	require.EqualValues(t, 250_000, *settings.LowBalanceThreshold)
	require.Equal(t, "prepaid", settings.BillingMode, "customer self-service must not change platform billing mode")

	// --- GET /v1/me/transactions: A sees exactly its deposit. ---
	w = doHostSeamSelf(routerA, http.MethodGet, "/v1/me/transactions?currency="+currency+"&limit=10", "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	txns := decodeHostSeamBody(t, w)
	require.EqualValues(t, 1, txns["total"], w.Body.String())
	list, ok := txns["transactions"].([]any)
	require.True(t, ok, w.Body.String())
	require.Len(t, list, 1)
	first, ok := list[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, payerA.UUID().String(), first["customer_id"])
	require.Equal(t, currency, first["currency"])
	require.EqualValues(t, 7_500_000, first["amount"])

	// --- SCOPING: subject B sees NONE of A's data. ---
	w = doHostSeamSelf(routerB, http.MethodGet, "/v1/me/balance?currency="+currency, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	acctB := decodeHostSeamBody(t, w)
	require.EqualValues(t, 0, acctB["balance_amount"], "B must not see A's balance")

	w = doHostSeamSelf(routerB, http.MethodGet, "/v1/me/transactions?currency="+currency, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	txnsB := decodeHostSeamBody(t, w)
	require.EqualValues(t, 0, txnsB["total"], "B must not see A's transactions: %s", w.Body.String())

}
