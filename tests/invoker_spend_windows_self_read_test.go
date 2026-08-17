//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprouter "github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/permissions"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#930: the delegated invoker's own spend-window read.
//
// invokerSeamAuthenticator is the host's DelegatedAuthenticator for an
// INVOKER-SCOPED credential: the host has verified its end-user, and maps it to
// {payer account it spends on, the opaque invoker string it spends as} — the
// SAME invoker the host passes to admission. It grants no permissions: this
// credential exists to read its own budget and nothing else.
type invokerSeamAuthenticator struct {
	payer   string
	invoker string
}

func (h invokerSeamAuthenticator) AuthenticateDelegated(context.Context, *http.Request) (*billingauth.DelegatedPrincipal, error) {
	return &billingauth.DelegatedPrincipal{
		MerchantID:   dbtest.TestMerchantID.String(),
		MerchantSlug: dbtest.TestMerchantSlug,
		SubjectID:    h.payer,
		Invoker:      h.invoker,
		Issuer:       "https://platform.host.example",
	}, nil
}

type selfSpendWindowJSON struct {
	Scope         string    `json:"scope"`
	Key           string    `json:"key"`
	WindowSeconds int64     `json:"window_seconds"`
	Limit         int64     `json:"limit"`
	Currency      string    `json:"currency"`
	Used          int64     `json:"used"`
	Reserved      int64     `json:"reserved"`
	Remaining     int64     `json:"remaining"`
	ResetsAt      time.Time `json:"resets_at"`
}

type selfSpendLimitsJSON struct {
	Currency string                `json:"currency"`
	Invoker  string                `json:"invoker"`
	Windows  []selfSpendWindowJSON `json:"windows"`
}

// TestOr930InvokerSpendWindowsSelfRead drives the whole read on the REAL HTTP
// surfaces against the suite's live Postgres + Redis:
//
//  1. the payer grants invoker A and invoker B DISTINCT windows through the real
//     customer-treasury route;
//  2. A spends through the real merchant admissions route;
//  3. A's own GET /v1/me/spend-limits reports A's org-declared limit with the
//     usage that ADMISSION just produced — never a fixture write;
//  4. capturing the hold moves `reserved` and leaves `used` — the estimate-based
//     window model, read back through the same numbers the gate keeps;
//  5. `resets_at` sits on the window's real staggered boundary, re-derived here
//     independently of the engine;
//  6. B reads B's window and never A's; addressing A is refused, typed;
//  7. an invoker-scoped credential is refused on the payer money surface;
//  8. the treasury route answers exactly what it always did (policy only).
func TestOr930InvokerSpendWindowsSelfRead(t *testing.T) {
	suite := getSharedTestSuite(t)
	require.NotNil(t, suite.App.Runtime.RedisClient, "the spendgate read needs Redis")
	ctx := suite.MerchantCtx()

	// The customer-treasury surface rebinds the payer to the customer's payable
	// subject = the merchant uuid (#567), so the grant, the admit and the read
	// all name the SAME payer.
	payerID := dbtest.TestMerchantID.UUID()
	payer := identity.CustomerID(payerID)
	currency := money.DefaultCurrency
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
	})

	ms := money.NewMoneyService(suite.App.Runtime.DB)
	_, err := ms.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payerID.String(), Currency: currency, Amount: 1_000_000_000, Source: "seed",
	})
	require.NoError(t, err)

	invokerA := "user:a-" + uuid.NewString()
	invokerB := "user:b-" + uuid.NewString()

	// --- 1) The payer grants A and B distinct windows via the real treasury route. ---
	treasury := http.NewServeMux()
	httproutes.RegisterCustomerTreasuryRoutes(httprouter.NewMux(treasury, "/v1/customers", suite.App.Runtime), suite.App.Runtime,
		middleware.DelegatedPrincipalRequired(hostSeamAuthenticator{
			subject: uuid.NewString(),
			perms: []string{
				permissions.MerchantAll,
				controlplane.PermCustomerSpendDelegationsRead,
				controlplane.PermCustomerSpendDelegationsUpdate,
			},
		}), routesurface.AllProviderRoutes())
	treasurySrv := httptest.NewServer(treasury)
	t.Cleanup(treasurySrv.Close)

	const windowSeconds = 86400
	grant := map[string]any{"delegations": []map[string]any{
		{
			"scope": "invoker", "scope_key": invokerA,
			"windows": []map[string]any{{"key": "day", "window_seconds": windowSeconds, "limit": 1000, "currency": currency}},
		},
		{
			"scope": "invoker", "scope_key": invokerB,
			"windows": []map[string]any{{"key": "day", "window_seconds": windowSeconds, "limit": 250, "currency": currency}},
		},
	}}
	resp := requestCustomerTreasuryJSON(t, treasurySrv, http.MethodPut,
		"/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", grant)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	// --- 2) A spends 600 through the real merchant admissions route. ---
	admitMux := http.NewServeMux()
	httproutes.RegisterServiceRoutes(httprouter.NewMux(admitMux, "/v1/merchant", suite.App.Runtime), suite.App.Runtime,
		httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{
			ServiceCredentialResolver: stubServiceCredentialResolver{
				permissions: []string{controlplane.PermMerchantAdmissionsCreate},
			},
		})})
	admitSrv := httptest.NewServer(middleware.ChainHTTP(admitMux, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID))))
	t.Cleanup(admitSrv.Close)

	requestID := "or930-" + uuid.NewString()
	require.True(t, admitDelegatedSpend(t, admitSrv, payerID, invokerA, requestID, currency, 600),
		"600 fits A's 1000 window")

	// --- 3) A reads its OWN windows. Everything here came from the admit above. ---
	selfA := newInvokerScopedSelfRouter(t, suite, payerID.String(), invokerA)
	docA := readSelfSpendLimits(t, selfA, "/v1/me/spend-limits?currency="+currency)
	require.Equal(t, invokerA, docA.Invoker)
	require.Len(t, docA.Windows, 1, "A holds exactly the one grant the payer wrote")
	winA := docA.Windows[0]
	require.Equal(t, "invoker", winA.Scope)
	require.Equal(t, "day", winA.Key)
	require.Equal(t, int64(windowSeconds), winA.WindowSeconds)
	require.Equal(t, int64(1000), winA.Limit, "the ORG-declared limit, not an operator default")
	require.Equal(t, currency, winA.Currency)
	require.Equal(t, int64(600), winA.Used, "the admission's own decrement")
	require.Equal(t, int64(600), winA.Reserved, "the admit's hold is still live")
	require.Equal(t, int64(400), winA.Remaining)

	// --- 5) resets_at is the window's REAL fixed boundary, re-derived here. ---
	// The gate staggers each payer's boundaries by a deterministic phase derived
	// from the window's Redis key; boundaries land at offset + k*duration forever.
	// A now+duration implementation lands off-phase and fails this.
	durMs := int64(windowSeconds) * 1000
	// The gate's key structure: sg:{merchant:customer}:currency:w:scope:scope_id:key,
	// with ':' scrubbed out of the scope id so it cannot break the composite key.
	offsetMs := fixedWindowOffsetMs(fmt.Sprintf("sg:{%s:%s}:%s:w:invoker:%s:day",
		dbtest.TestMerchantID.UUID().String(), payerID.String(), currency,
		strings.ReplaceAll(invokerA, ":", "_")), durMs)
	require.Zero(t, (winA.ResetsAt.UnixMilli()-offsetMs)%durMs,
		"resets_at must sit on offset + k*duration, not now+duration")
	require.True(t, winA.ResetsAt.After(time.Now()), "the boundary is in the future")
	require.True(t, winA.ResetsAt.Before(time.Now().Add(time.Duration(durMs)*time.Millisecond)),
		"and at most one window away")

	// --- 4) Capture settles the hold. Estimate-based windows KEEP the spend; the
	// reservation is what goes away. Two numbers that move independently is the
	// whole reason `reserved` is reported next to `used`. ---
	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	_, err = svc.CaptureHold(ctx, billingservice.CaptureHoldRequest{
		RequestID: requestID, Amount: 600, CustomerID: payerID.String(), Currency: currency, Invoker: invokerA,
	})
	require.NoError(t, err)

	afterCapture := readSelfSpendLimits(t, selfA, "/v1/me/spend-limits?currency="+currency)
	require.Len(t, afterCapture.Windows, 1)
	require.Equal(t, int64(600), afterCapture.Windows[0].Used, "capture does not refund the window")
	require.Equal(t, int64(0), afterCapture.Windows[0].Reserved, "the hold is settled, nothing is reserved")
	require.Equal(t, int64(400), afterCapture.Windows[0].Remaining)

	// --- 6) B sees ONLY B's grant: a different limit, and no trace of A's spend. ---
	selfB := newInvokerScopedSelfRouter(t, suite, payerID.String(), invokerB)
	docB := readSelfSpendLimits(t, selfB, "/v1/me/spend-limits?currency="+currency)
	require.Equal(t, invokerB, docB.Invoker)
	require.Len(t, docB.Windows, 1)
	require.Equal(t, int64(250), docB.Windows[0].Limit, "B's own limit, not A's 1000")
	require.Equal(t, int64(0), docB.Windows[0].Used, "A's spend is not B's")
	require.Equal(t, int64(250), docB.Windows[0].Remaining)

	// B naming A is refused, not silently ignored: a quietly-dropped parameter
	// reads to the caller as a successful cross-read.
	crossRead := doHostSeamSelf(selfB, http.MethodGet, "/v1/me/spend-limits?currency="+currency+"&invoker="+invokerA, "")
	require.Equal(t, http.StatusBadRequest, crossRead.Code, crossRead.Body.String())
	require.Contains(t, crossRead.Body.String(), "spend_scope_not_addressable")

	// --- 7) The invoker-scoped credential reaches NOTHING else. Its subject names
	// the payer's account, which it does not own. ---
	for _, path := range []string{"/v1/me/balance?currency=" + currency, "/v1/me/transactions", "/v1/me/invoices"} {
		refused := doHostSeamSelf(selfA, http.MethodGet, path, "")
		require.Equal(t, http.StatusForbidden, refused.Code, path+" -> "+refused.Body.String())
		require.Contains(t, refused.Body.String(), "invoker_scoped_principal", path)
	}
	treasuryRefusal := httptest.NewRecorder()
	invokerTreasury := http.NewServeMux()
	httproutes.RegisterCustomerTreasuryRoutes(httprouter.NewMux(invokerTreasury, "/v1/customers", suite.App.Runtime), suite.App.Runtime,
		middleware.DelegatedPrincipalRequired(invokerSeamAuthenticator{payer: payerID.String(), invoker: invokerA}),
		routesurface.AllProviderRoutes())
	treasuryReq := httptest.NewRequest(http.MethodGet, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", nil)
	treasuryReq.Header.Set("Authorization", "Bearer host-credential")
	invokerTreasury.ServeHTTP(treasuryRefusal, treasuryReq)
	require.Equal(t, http.StatusForbidden, treasuryRefusal.Code, treasuryRefusal.Body.String())
	require.Contains(t, treasuryRefusal.Body.String(), "invoker_scoped_principal")

	// --- 8) The payer's admin read is UNCHANGED: policy only, both delegations,
	// no usage fields invented on it. ---
	policyResp := requestCustomerTreasuryJSON(t, treasurySrv, http.MethodGet,
		"/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", nil)
	require.Equal(t, http.StatusOK, policyResp.status, policyResp.body)
	require.Len(t, decodeDelegations(t, policyResp.body), 2)
	for _, field := range []string{"\"used\"", "\"reserved\"", "\"remaining\"", "\"resets_at\""} {
		require.NotContains(t, policyResp.body, field, "the treasury route stays policy-only")
	}
}

// newInvokerScopedSelfRouter mounts the real /v1/me surface behind an
// INVOKER-SCOPED host principal.
func newInvokerScopedSelfRouter(t *testing.T, suite *TestContainerSuite, payer, invoker string) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	httproutes.RegisterSelfServiceRoutes(httprouter.NewMux(mux, "/v1/me", suite.App.Runtime), suite.App.Runtime,
		middleware.DelegatedPrincipalRequired(invokerSeamAuthenticator{payer: payer, invoker: invoker}),
		routesurface.AllProviderRoutes())
	return mux
}

func readSelfSpendLimits(t *testing.T, e http.Handler, path string) selfSpendLimitsJSON {
	t.Helper()
	w := doHostSeamSelf(e, http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var doc selfSpendLimitsJSON
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &doc), w.Body.String())
	return doc
}

// admitDelegatedSpend posts one delegated admission through the real merchant
// admissions route and reports whether it was allowed.
func admitDelegatedSpend(t *testing.T, srv *httptest.Server, payer uuid.UUID, invoker, requestID, currency string, amount int64) bool {
	t.Helper()
	body, err := json.Marshal(map[string]any{"items": []map[string]any{{
		"customer_id": payer.String(), "invoker": invoker, "invoker_type": "delegated",
		"currency": currency, "estimated_amount": amount, "request_id": requestID,
	}}})
	require.NoError(t, err)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/merchant/admissions", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer openrails_st_testkeyid_testsecret")
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(httpResp.Body)
	require.Equal(t, http.StatusOK, httpResp.StatusCode, string(raw))
	var batch struct {
		Items []struct {
			Result struct {
				Allowed bool `json:"allowed"`
			} `json:"result"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(raw, &batch), string(raw))
	require.Len(t, batch.Items, 1)
	return batch.Items[0].Result.Allowed
}

// fixedWindowOffsetMs re-derives the gate's deterministic per-window phase from
// the window's Redis key, independently of the engine, so the boundary assertion
// is a real check and not the implementation restating itself.
func fixedWindowOffsetMs(prefix string, durMs int64) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(prefix))
	return int64(h.Sum64() % uint64(durMs))
}
