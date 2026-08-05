//go:build integration

package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	openrails "github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/http/middleware"
	httprouter "github.com/open-rails/openrails/internal/http/router"
	httproutes "github.com/open-rails/openrails/internal/http/routes"
	"github.com/open-rails/openrails/internal/http/routesurface"
	"github.com/open-rails/openrails/pkg/identity"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

func TestMerchantServiceJWTSpendDelegationRemoteClient(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()
	payerID := suite.ensureCustomer(ctx, uuid.NewString())
	payer := identity.CustomerID(payerID)
	_, err := suite.Pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
	})

	mux := http.NewServeMux()
	resolver := stubServiceCredentialResolver{
		serviceJWT: true,
		permissions: []string{
			controlplane.PermMerchantCustomerSettingsUpdate,
		},
	}
	httproutes.RegisterServiceRoutes(
		httprouter.NewMux(mux, "/v1/merchant", suite.App.Runtime),
		suite.App.Runtime,
		httproutes.Options{Gate: httproutes.NewGate(httproutes.GateOptions{ServiceCredentialResolver: resolver})},
	)
	router := middleware.ChainHTTP(mux, middleware.ResolveMerchantHTTP(middleware.StaticMerchant(dbtest.TestMerchantID)))
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	client := openrails.NewRemote(srv.URL, openrails.WithTokenProvider(func(context.Context) (string, error) {
		return "eyJ.service.jwt", nil
	}))

	first := openrails.SpendDelegationInput{
		Scope: "invoker", ScopeKey: "document-bound-invoker",
		Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 1000, Currency: "USD"}},
	}
	second := openrails.SpendDelegationInput{
		Scope: "role", ScopeKey: uuid.NewString(),
		Windows: []openrails.SpendLimitWindow{{Key: "week", WindowSeconds: 604800, Limit: 9000, Currency: "USD"}},
	}
	require.NoError(t, client.SetCustomerSpendDelegation(ctx, payerID.String(), first))
	require.NoError(t, client.SetCustomerSpendDelegation(ctx, payerID.String(), second))
	first.Windows[0].Limit = 321
	require.NoError(t, client.SetCustomerSpendDelegation(ctx, payerID.String(), first))

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	stored, err := svc.InvokerSpendLimits(ctx, payer)
	require.NoError(t, err)
	require.Len(t, stored, 2, "singular machine upsert must preserve sibling grants")

	require.NoError(t, client.SetCustomerSpendDelegations(ctx, payerID.String(), []openrails.SpendDelegationInput{second}))
	stored, err = svc.InvokerSpendLimits(ctx, payer)
	require.NoError(t, err)
	require.Len(t, stored, 1, "full replacement must remove omitted grants")
	require.Equal(t, "role", stored[0].Scope)

	err = client.SetCustomerSpendDelegations(ctx, payerID.String(), []openrails.SpendDelegationInput{
		{
			Scope: " role ", ScopeKey: " " + second.ScopeKey + " ", Windows: second.Windows,
		},
		second,
	})
	require.ErrorIs(t, err, openrails.ErrInvalid)
	var remoteStatus *openrails.StatusError
	require.ErrorAs(t, err, &remoteStatus)
	require.Equal(t, http.StatusBadRequest, remoteStatus.Status)
	require.Contains(t, err.Error(), "duplicate delegation for role")
	stored, err = svc.InvokerSpendLimits(ctx, payer)
	require.NoError(t, err)
	require.Len(t, stored, 1, "rejected remote duplicate document must not mutate policy")
}

func TestReplaceInvokerSpendLimitsRollsBackOnWriteFailure(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()
	payerID := suite.ensureCustomer(ctx, uuid.NewString())
	payer := identity.CustomerID(payerID)
	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
	})

	original := billingservice.InvokerSpendLimitInput{
		Scope: "invoker", ScopeKey: "rollback-original",
		Windows: []billingservice.SpendLimitWindowInput{{Key: "day", WindowSeconds: 86400, Limit: 111}},
	}
	require.NoError(t, svc.SetInvokerSpendLimits(ctx, payer, original))

	failingKey := "rollback-failure-" + uuid.NewString()
	suffix := strings.Split(uuid.NewString(), "-")[0]
	functionName := "fail_invoker_spend_" + suffix
	triggerName := "fail_invoker_spend_" + suffix
	_, err = suite.Pool.Exec(ctx, fmt.Sprintf(`
CREATE FUNCTION openrails.%s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'injected invoker spend-limit write failure';
END
$$;
CREATE TRIGGER %s
BEFORE INSERT OR UPDATE ON openrails.invoker_spend_limits
FOR EACH ROW WHEN (NEW.scope_key = '%s')
EXECUTE FUNCTION openrails.%s()`, functionName, triggerName, failingKey, functionName))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(context.Background(), fmt.Sprintf(
			"DROP TRIGGER IF EXISTS %s ON openrails.invoker_spend_limits; DROP FUNCTION IF EXISTS openrails.%s()",
			triggerName, functionName,
		))
	})

	err = svc.ReplaceInvokerSpendLimits(ctx, payer, []billingservice.InvokerSpendLimitInput{{
		Scope: "invoker", ScopeKey: failingKey,
		Windows: []billingservice.SpendLimitWindowInput{{Key: "day", WindowSeconds: 86400, Limit: 999}},
	}})
	require.ErrorContains(t, err, "injected invoker spend-limit write failure")

	stored, err := svc.InvokerSpendLimits(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, []billingservice.InvokerSpendLimitInput{original}, stored,
		"failed replacement must roll back deletes and upserts")
}

func TestReplaceInvokerSpendLimitsPurgesLegacyPaddedScopeKeys(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()
	payerID := suite.ensureCustomer(ctx, uuid.NewString())
	payer := identity.CustomerID(payerID)
	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
	})

	canonical := billingservice.InvokerSpendLimitInput{
		Scope: "invoker_tier", ScopeKey: "gold",
		Windows: []billingservice.SpendLimitWindowInput{{Key: "day", WindowSeconds: 86400, Limit: 222}},
	}
	require.NoError(t, svc.SetInvokerSpendLimits(ctx, payer, canonical))
	tag, err := suite.Pool.Exec(ctx, `
UPDATE openrails.invoker_spend_limits
SET scope_key = 'gold '
WHERE merchant_id = $1 AND customer_id = $2
  AND scope = 'invoker_tier' AND scope_key = 'gold'`, dbtest.TestMerchantID.UUID(), payerID)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())

	loadScopeKeys := func() []string {
		rows, err := suite.Pool.Query(ctx, `
SELECT scope_key
FROM openrails.invoker_spend_limits
WHERE merchant_id = $1 AND customer_id = $2
ORDER BY scope_key`, dbtest.TestMerchantID.UUID(), payerID)
		require.NoError(t, err)
		defer rows.Close()
		var keys []string
		for rows.Next() {
			var key string
			require.NoError(t, rows.Scan(&key))
			keys = append(keys, key)
		}
		require.NoError(t, rows.Err())
		return keys
	}

	require.Equal(t, []string{"gold "}, loadScopeKeys(), "fixture must contain the padded legacy key")
	require.NoError(t, svc.ReplaceInvokerSpendLimits(ctx, payer, []billingservice.InvokerSpendLimitInput{canonical}))
	require.Equal(t, []string{"gold"}, loadScopeKeys(), "canonical replacement must leave exactly its requested set")

	tag, err = suite.Pool.Exec(ctx, `
UPDATE openrails.invoker_spend_limits
SET scope_key = 'gold '
WHERE merchant_id = $1 AND customer_id = $2
  AND scope = 'invoker_tier' AND scope_key = 'gold'`, dbtest.TestMerchantID.UUID(), payerID)
	require.NoError(t, err)
	require.EqualValues(t, 1, tag.RowsAffected())
	require.NoError(t, svc.ReplaceInvokerSpendLimits(ctx, payer, nil))
	require.Empty(t, loadScopeKeys(), "empty replacement must revoke padded legacy rows")
}

func TestReplaceAndSetInvokerSpendLimitsSerialize(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := suite.MerchantCtx()
	payerID := suite.ensureCustomer(ctx, uuid.NewString())
	payer := identity.CustomerID(payerID)
	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = suite.Pool.Exec(ctx, "DELETE FROM openrails.invoker_spend_limits WHERE customer_id = $1", payerID)
	})

	blocker, err := suite.Pool.Acquire(ctx)
	require.NoError(t, err)
	tx, err := blocker.Begin(ctx)
	require.NoError(t, err)
	released := false
	t.Cleanup(func() {
		if !released {
			_ = tx.Rollback(context.Background())
			blocker.Release()
		}
	})
	lockKey := dbtest.TestMerchantID.String() + ":" + payerID.String()
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey)
	require.NoError(t, err)

	replacement := billingservice.InvokerSpendLimitInput{
		Scope: "role", ScopeKey: uuid.NewString(),
		Windows: []billingservice.SpendLimitWindowInput{{Key: "week", WindowSeconds: 604800, Limit: 700}},
	}
	singular := billingservice.InvokerSpendLimitInput{
		Scope: "invoker", ScopeKey: "serialized-upsert",
		Windows: []billingservice.SpendLimitWindowInput{{Key: "day", WindowSeconds: 86400, Limit: 300}},
	}
	// Each caller gets its OWN pinned merchant connection, as two concurrent
	// requests would. Sharing one pinned context would serialize them on that
	// single connection and they would never contend on the advisory lock the
	// test is about.
	replaceCtx, upsertCtx := suite.MerchantCtx(), suite.MerchantCtx()
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- svc.ReplaceInvokerSpendLimits(replaceCtx, payer, []billingservice.InvokerSpendLimitInput{replacement})
	}()

	waiters := func(want int) bool {
		var got int
		err := suite.Pool.QueryRow(ctx, `
SELECT count(*)
FROM pg_stat_activity
WHERE datname = current_database()
  AND wait_event_type = 'Lock'
  AND query LIKE '%pg_advisory_xact_lock(hashtextextended%'`).Scan(&got)
		return err == nil && got >= want
	}
	require.Eventually(t, func() bool { return waiters(1) }, 5*time.Second, 20*time.Millisecond,
		"replacement must wait on the payer-scoped write lock")

	upsertDone := make(chan error, 1)
	go func() {
		upsertDone <- svc.SetInvokerSpendLimits(upsertCtx, payer, singular)
	}()
	require.Eventually(t, func() bool { return waiters(2) }, 5*time.Second, 20*time.Millisecond,
		"singular upsert must queue behind replacement on the same lock")

	require.NoError(t, tx.Commit(ctx))
	blocker.Release()
	released = true
	require.NoError(t, <-replaceDone)
	require.NoError(t, <-upsertDone)

	stored, err := svc.InvokerSpendLimits(ctx, payer)
	require.NoError(t, err)
	require.Len(t, stored, 2, "queued replace then upsert must commit in that serial order")
	limits := map[string]int64{}
	for _, row := range stored {
		limits[row.Scope+"\x00"+row.ScopeKey] = row.Windows[0].Limit
	}
	require.EqualValues(t, 700, limits["role\x00"+replacement.ScopeKey])
	require.EqualValues(t, 300, limits["invoker\x00"+singular.ScopeKey])
}

func TestCustomerTreasurySpendDelegationsHTTPFullReplacement(t *testing.T) {
	suite := setupTestSuite(t)

	router := http.NewServeMux()
	httproutes.RegisterCustomerTreasuryRoutes(httprouter.NewMux(router, "/v1/customers", suite.App.Runtime), suite.App.Runtime,
		middleware.DelegatedPrincipalRequired(hostSeamAuthenticator{
			subject: uuid.NewString(),
			perms: []string{
				controlplane.PermCustomerSpendDelegationsRead,
				controlplane.PermCustomerSpendDelegationsUpdate,
			},
		}), routesurface.AllProviderRoutes())

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	invokerKey := uuid.NewString()
	roleKey := uuid.NewString()
	trustLevelKey := "trust_1"

	firstDoc := map[string]any{"delegations": []map[string]any{
		{
			"scope":     "invoker",
			"scope_key": invokerKey,
			"windows": []map[string]any{
				{"key": "day", "window_seconds": 86400, "limit": 1200, "currency": "USD"},
			},
		},
		{
			"scope":     "role",
			"scope_key": roleKey,
			"windows": []map[string]any{
				{"key": "week", "window_seconds": 604800, "limit": 9000, "currency": "USD"},
			},
		},
		{
			"scope":     "invoker_tier",
			"scope_key": trustLevelKey,
			"windows": []map[string]any{
				{"key": "month", "window_seconds": 2592000, "limit": 15000, "currency": "USD"},
			},
		},
	}}
	resp := requestCustomerTreasuryJSON(t, srv, http.MethodPut, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", firstDoc)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	resp = requestCustomerTreasuryJSON(t, srv, http.MethodGet, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", nil)
	require.Equal(t, http.StatusOK, resp.status, resp.body)
	require.Len(t, decodeDelegations(t, resp.body), 3)

	resp = requestCustomerTreasuryJSON(t, srv, http.MethodPut, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations:upsert", map[string]any{
		"scope":     "invoker",
		"scope_key": invokerKey,
		"windows": []map[string]any{
			{"key": "day", "window_seconds": 86400, "limit": 321, "currency": "USD"},
		},
	})
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	svc, err := billingservice.New(suite.App.Runtime)
	require.NoError(t, err)
	payer := identity.CustomerID(dbtest.TestMerchantID.UUID())
	stored, err := svc.InvokerSpendLimits(dbtest.WithTestMerchant(context.Background()), payer)
	require.NoError(t, err)
	require.Len(t, stored, 3, "single upsert must preserve unrelated role and invoker-tier rows")
	limits := map[string]int64{}
	for _, row := range stored {
		limits[row.Scope+"\x00"+row.ScopeKey] = row.Windows[0].Limit
	}
	require.EqualValues(t, 321, limits["invoker\x00"+invokerKey])
	require.EqualValues(t, 9000, limits["role\x00"+roleKey])
	require.EqualValues(t, 15000, limits["invoker_tier\x00"+trustLevelKey])

	secondDoc := map[string]any{"delegations": []map[string]any{
		{
			"scope":     "role",
			"scope_key": roleKey,
			"windows": []map[string]any{
				{"key": "day", "window_seconds": 86400, "limit": 500, "currency": "USD"},
			},
		},
	}}
	resp = requestCustomerTreasuryJSON(t, srv, http.MethodPut, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations", secondDoc)
	require.Equal(t, http.StatusOK, resp.status, resp.body)

	stored, err = svc.InvokerSpendLimits(dbtest.WithTestMerchant(context.Background()), payer)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, "role", stored[0].Scope)
	require.Equal(t, roleKey, stored[0].ScopeKey)
	require.Len(t, stored[0].Windows, 1)
	require.EqualValues(t, 500, stored[0].Windows[0].Limit)

	resp = requestCustomerTreasuryJSON(t, srv, http.MethodPut, "/v1/customers/"+dbtest.TestMerchantSlug+"/spend-delegations",
		map[string]any{"customer_id": uuid.NewString(), "delegations": []map[string]any{}})
	require.Equal(t, http.StatusBadRequest, resp.status, resp.body)
}

type customerTreasuryResponse struct {
	status int
	body   string
}

func requestCustomerTreasuryJSON(t *testing.T, srv *httptest.Server, method, path string, body any) customerTreasuryResponse {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		require.NoError(t, err)
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer host-credential")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return customerTreasuryResponse{status: resp.StatusCode, body: string(data)}
}

func decodeDelegations(t *testing.T, body string) []any {
	t.Helper()
	var doc struct {
		Delegations []any `json:"delegations"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &doc), body)
	return doc.Delegations
}
