//go:build integration

package tests

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/permissions"
)

func checkPolicyInvokerWindows(t *testing.T, f treasuryWorkflow) {
	ctx := t.Context()
	payer, payerToken := f.actor(t, []string{permissions.CustomerAll})
	_, err := f.client.DepositCredits(ctx, openrails.DepositCreditsRequest{CustomerID: new(payer.String()), Invoker: payer.String(), Currency: "USD", Amount: 1_000_000_000, Source: "window", SourceID: uuid.NewString()})
	require.NoError(t, err)
	a, tokenA := f.actor(t, nil)
	b, tokenB := f.actor(t, nil)
	invokerA, invokerB := "worker:a-"+uuid.NewString(), "worker:b-"+uuid.NewString()
	f.hostInvokers.Store(a.String(), hostInvokerBinding{payer: payer, invoker: invokerA})
	f.hostInvokers.Store(b.String(), hostInvokerBinding{payer: payer, invoker: invokerB})
	grantPath := f.hostURL + "/v1/customers/" + payer.String() + "/spend-delegations"
	grants := []openrails.SpendDelegationInput{
		{Scope: "invoker", ScopeKey: invokerA, Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 1000, Currency: "USD"}}},
		{Scope: "invoker", ScopeKey: invokerB, Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 250, Currency: "USD"}}},
	}
	status, raw := requestWorkflowJSON(t, http.MethodPut, grantPath, payerToken, map[string]any{"delegations": grants})
	require.Equal(t, http.StatusOK, status, string(raw))
	admit := func(invoker string, amount int64, requestID string) openrails.AdmitBatchVerdict {
		t.Helper()
		deadline := time.Now().Add(time.Hour)
		items, err := f.client.AdmitBatch(ctx, []openrails.AdmitRequest{{CustomerID: (payer).String(), Invoker: invoker, InvokerType: openrails.InvokerTypeDelegated,
			Currency: "USD", EstimatedAmount: amount, RequestID: requestID, Source: "window", ExpiresAt: &deadline}})
		require.NoError(t, err)
		require.Len(t, items, 1)
		return items[0]
	}
	holdKey := uuid.NewString()
	first := admit(invokerA, 600, holdKey)
	require.Equal(t, http.StatusOK, first.Status)
	require.True(t, first.Result.Allowed)
	read := func(token string) map[string]any {
		t.Helper()
		status, raw := requestWorkflowJSON(t, http.MethodGet, f.hostURL+"/v1/me/spend-limits?currency=USD", token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		return workflowObject(t, raw)
	}
	doc := read(tokenA)
	require.Equal(t, invokerA, doc["invoker"])
	windows := doc["windows"].([]any)
	require.Len(t, windows, 1)
	window := windows[0].(map[string]any)
	for key, want := range map[string]string{"scope": "invoker", "key": "day", "limit": "1000", "currency": "USD", "used": "600", "reserved": "600", "remaining": "400"} {
		require.Equal(t, want, window[key], key)
	}
	require.EqualValues(t, 86400, window["window_seconds"])
	reset, err := time.Parse(time.RFC3339Nano, window["resets_at"].(string))
	require.NoError(t, err)
	parts, err := json.Marshal([]string{fmt.Sprintf("%s/%s/USD", f.merchant.MerchantID, payer), "invoker", invokerA, "", "day"})
	require.NoError(t, err)
	digest := sha256.Sum256(parts)
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(hex.EncodeToString(digest[:])))
	const dayMillis = int64(86400 * 1000)
	offset := int64(hash.Sum64() % uint64(dayMillis))
	require.Zero(t, (reset.UnixMilli()-offset)%dayMillis, "reset follows the actual staggered boundary, not now plus duration")
	require.True(t, reset.After(time.Now()))
	require.True(t, reset.Before(time.Now().Add(24*time.Hour)))
	denied := admit(invokerA, 600, uuid.NewString())
	require.Equal(t, http.StatusForbidden, denied.Status)
	require.False(t, denied.Result.Allowed)
	require.Equal(t, "budget", denied.Result.BlockedBy)
	require.Equal(t, admission.DenyBudgetExceeded, denied.Result.DenyCode)
	stranger := admit("ungranted-"+uuid.NewString(), 1, uuid.NewString())
	require.Equal(t, http.StatusForbidden, stranger.Status)
	require.Equal(t, admission.DenyDelegatedSpendNotAllowed, stranger.Result.DenyCode)
	_, err = f.client.Capture(ctx, holdKey, 600, nil)
	require.NoError(t, err)
	window = read(tokenA)["windows"].([]any)[0].(map[string]any)
	for key, want := range map[string]string{"used": "600", "reserved": "0", "remaining": "400"} {
		require.Equal(t, want, window[key], key)
	}
	doc = read(tokenB)
	require.Equal(t, invokerB, doc["invoker"])
	windows = doc["windows"].([]any)
	require.Len(t, windows, 1)
	window = windows[0].(map[string]any)
	for key, want := range map[string]string{"limit": "250", "used": "0", "remaining": "250"} {
		require.Equal(t, want, window[key], key)
	}
	status, raw = requestWorkflowJSON(t, http.MethodGet, f.hostURL+"/v1/me/spend-limits?currency=USD&invoker="+url.QueryEscape(invokerA), tokenB, nil)
	require.Equal(t, http.StatusBadRequest, status, string(raw))
	require.Contains(t, string(raw), "spend_scope_not_addressable")
	for _, path := range []string{"/v1/me/balance?currency=USD", "/v1/me/transactions", "/v1/me/invoices", "/v1/customers/" + payer.String() + "/spend-delegations"} {
		status, raw = requestWorkflowJSON(t, http.MethodGet, f.hostURL+path, tokenA, nil)
		require.Equal(t, http.StatusForbidden, status, string(raw))
		require.Contains(t, string(raw), "invoker_scoped_principal")
	}
	for _, token := range []string{"", "not-a-token"} {
		status, raw = requestWorkflowJSON(t, http.MethodGet, f.hostURL+"/v1/me/spend-limits?currency=USD", token, nil)
		require.Equal(t, http.StatusUnauthorized, status, string(raw), "membership mapping cannot bypass token verification")
	}
	status, raw = requestWorkflowJSON(t, http.MethodGet, grantPath, payerToken, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	policy := workflowObject(t, raw)
	require.Len(t, policy["delegations"].([]any), 2)
	require.NotContains(t, string(raw), `"remaining"`, "treasury returns policy, not invoker usage")
}
