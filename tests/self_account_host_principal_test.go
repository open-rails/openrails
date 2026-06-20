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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	ginroutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// Issue #339 full loop: a HOST-AUTHENTICATED principal (the host-pluggable
// billingauth.DelegatedAuthenticator seam, NO control plane delegated
// verifier) drives the /v1/me/account surface end-to-end against a real
// database — reads its account, configures its settings, lists its
// transactions — and another subject's data is never visible. A read-only
// principal is denied the settings write (scope separation).

// hostSeamAuthenticator is the host's DelegatedAuthenticator: the host has
// already verified its own credential and supplies the explicitly mapped
// principal (tenant + subject + permissions).
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

func newHostSeamSelfRouter(t *testing.T, suite *TestContainerSuite, subject string, perms []string) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	e := gin.New()
	group := e.Group("/v1/me")
	ginroutes.RegisterSelfServiceRoutes(group, suite.App.Runtime,
		ginmw.DelegatedPrincipalRequired(hostSeamAuthenticator{subject: subject, perms: perms}))
	return e
}

func doHostSeamSelf(e *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
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

	readWrite := []string{controlplane.PermSelfBillingRead, controlplane.PermSelfBillingWrite}
	routerA := newHostSeamSelfRouter(t, suite, subjectA, readWrite)
	routerB := newHostSeamSelfRouter(t, suite, subjectB, readWrite)
	routerAReadOnly := newHostSeamSelfRouter(t, suite, subjectA, []string{controlplane.PermSelfBillingRead})

	// --- GET /v1/me/account: A sees its own balance. ---
	w := doHostSeamSelf(routerA, http.MethodGet, "/v1/me/account?currency="+currency, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	acct := decodeHostSeamBody(t, w)
	require.Equal(t, currency, acct["currency"])
	require.EqualValues(t, 7_500_000, acct["balance_amount"])
	require.EqualValues(t, 7_500_000, acct["available_amount"])
	require.EqualValues(t, 0, acct["outstanding_owed_amount"])
	require.Equal(t, "prepaid", acct["billing_mode"])
	require.NotNil(t, acct["settings"])

	// --- PUT /v1/me/account/settings: A configures its own account. ---
	settingsBody := fmt.Sprintf(`{
		"currency": %q,
		"billing_mode": "arrears",
		"max_spend_per_day": 1000000,
		"max_outstanding_owed_amount": 2000000,
		"hard_stop_on_breach": true
	}`, currency)
	w = doHostSeamSelf(routerA, http.MethodPut, "/v1/me/account/settings", settingsBody)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	stored := decodeHostSeamBody(t, w)
	require.Equal(t, "arrears", stored["billing_mode"], w.Body.String())
	require.EqualValues(t, 1_000_000, stored["max_spend_per_day"])
	require.EqualValues(t, 2_000_000, stored["max_outstanding_owed_amount"])
	require.Equal(t, true, stored["hard_stop_on_breach"])
	require.Equal(t, payerA.UUID().String(), stored["customer_id"],
		"settings must be stored under the delegated subject, never caller-supplied")

	// The settings round-trip through the account read.
	w = doHostSeamSelf(routerA, http.MethodGet, "/v1/me/account?currency="+currency, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Equal(t, "arrears", decodeHostSeamBody(t, w)["billing_mode"])

	// --- GET /v1/me/account/transactions: A sees exactly its deposit. ---
	w = doHostSeamSelf(routerA, http.MethodGet, "/v1/me/account/transactions?currency="+currency+"&limit=10", "")
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
	w = doHostSeamSelf(routerB, http.MethodGet, "/v1/me/account?currency="+currency, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	acctB := decodeHostSeamBody(t, w)
	require.EqualValues(t, 0, acctB["balance_amount"], "B must not see A's balance")
	require.Equal(t, "prepaid", acctB["billing_mode"], "B must not see A's arrears settings")

	w = doHostSeamSelf(routerB, http.MethodGet, "/v1/me/account/transactions?currency="+currency, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	txnsB := decodeHostSeamBody(t, w)
	require.EqualValues(t, 0, txnsB["total"], "B must not see A's transactions: %s", w.Body.String())

	// --- NEGATIVE: a read-only principal cannot write settings. ---
	w = doHostSeamSelf(routerAReadOnly, http.MethodPut, "/v1/me/account/settings", settingsBody)
	require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	// ...but can still read its account.
	w = doHostSeamSelf(routerAReadOnly, http.MethodGet, "/v1/me/account?currency="+currency, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
}
