//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// The four running-charge routes over HTTP, for an application holding Usage:
// a start answers 201 and then 200, a stop, a read and the list 200; a
// refused minimum 402, an unknown reference 404, a changed term
// idempotency_key_reused. Without Usage every one of them is 403.
func TestRunningChargeRoutes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	_, err := w.client[embedded].CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
		{CustomerID: c.cid(), Currency: "USD", Amount: 10_000_000, Source: "support", SourceID: "seed"},
	})
	require.NoError(t, err)
	application := func(role string) string {
		return w.auth.issue(t, grant{subject: "backend-" + uuid.NewString()[:8], role: role,
			kind: string(openrails.SubjectApplication), credential: string(openrails.CredentialAPIKey)})
	}
	usage := application("only:" + string(appUsage))
	refusal := func(status int, body map[string]any, want int, code string) map[string]any {
		t.Helper()
		require.Equal(t, want, status, "%v", body)
		details := body["error"].(map[string]any)
		require.Equal(t, code, details["code"], "%v", body)
		return details
	}

	reference := "rental/" + uuid.NewString()[:8] + " a"
	path := "/v1/app/running-charges/" + url.PathEscape(reference)
	startedAt := w.clock.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	start := map[string]any{
		"reference": reference, "customer_id": c.id, "currency": "usd", "rate_per_hour": "3600000",
		"started_at": startedAt.Format(time.RFC3339), "low_balance_amount": "1000000", "resource": "gpu:a100",
	}
	status, started := w.merchantJSON(usage, http.MethodPost, "/v1/app/running-charges", start)
	require.Equal(t, http.StatusCreated, status, "%v", started)
	require.Equal(t, "running", started["state"])
	require.Equal(t, reference, started["reference"])
	require.Equal(t, "USD", started["currency"])
	require.Equal(t, "3600000", started["rate_per_hour"])
	require.Equal(t, "3600000", started["charged_amount"], "an hour at the rate, charged since the host's start")
	require.Equal(t, "0", started["posted_amount"])
	require.Nil(t, started["stopped_at"])
	require.Equal(t, "gpu:a100", started["resource"])
	id, err := billing.ParseRunningChargeID(started["id"].(string))
	require.NoError(t, err)
	require.False(t, id.IsZero())

	status, again := w.merchantJSON(usage, http.MethodPost, "/v1/app/running-charges", start)
	require.Equal(t, http.StatusOK, status, "the same reference and terms: %v", again)
	require.Equal(t, started["id"], again["id"])
	changed := map[string]any{}
	for k, v := range start {
		changed[k] = v
	}
	changed["rate_per_hour"] = "3600001"
	status, body := w.merchantJSON(usage, http.MethodPost, "/v1/app/running-charges", changed)
	require.Equal(t, "rate_per_hour", refusal(status, body, http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused)["param"])
	status, body = w.merchantJSON(usage, http.MethodPost, "/v1/app/running-charges", map[string]any{
		"reference": "too-dear", "customer_id": c.id, "currency": "USD", "rate_per_hour": "1", "minimum_amount": "6400001",
	})
	refusal(status, body, http.StatusPaymentRequired, billing.CodeInsufficientCredits)
	status, body = w.merchantJSON(usage, http.MethodPost, "/v1/app/running-charges", map[string]any{
		"reference": "unknown-field", "customer_id": c.id, "currency": "USD", "rate_per_hour": "1", "amount": "5",
	})
	refusal(status, body, http.StatusBadRequest, billing.CodeUnknownField)
	status, body = w.merchantJSON(usage, http.MethodPost, "/v1/app/running-charges", map[string]any{
		"reference": "no-currency", "customer_id": c.id, "currency": "XXX", "rate_per_hour": "1",
	})
	refusal(status, body, http.StatusBadRequest, "currency_unsupported")

	status, read := w.merchantJSON(usage, http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, status, "%v", read)
	require.Equal(t, started["id"], read["id"])
	status, body = w.merchantJSON(usage, http.MethodGet, "/v1/app/running-charges/never-started", nil)
	refusal(status, body, http.StatusNotFound, "running_charge_not_found")

	status, list := w.merchantJSON(usage, http.MethodGet, "/v1/app/running-charges?state=running&customer_id="+c.id, nil)
	require.Equal(t, http.StatusOK, status, "%v", list)
	require.Len(t, list["data"], 1)
	require.Equal(t, started["id"], list["data"].([]any)[0].(map[string]any)["id"])
	require.Nil(t, list["next_cursor"])
	status, list = w.merchantJSON(usage, http.MethodGet, "/v1/app/running-charges?state=stopped", nil)
	require.Equal(t, http.StatusOK, status)
	require.Empty(t, list["data"])
	status, body = w.merchantJSON(usage, http.MethodGet, "/v1/app/running-charges?state=paused", nil)
	require.Equal(t, "state", refusal(status, body, http.StatusBadRequest, billing.CodeInvalidQuery)["param"])

	// The customer's own balance shows the accrual without a posting.
	status, me := c.call(http.MethodGet, "", "", nil)
	require.Equal(t, http.StatusOK, status, "%v", me)
	balance := me["balances"].([]any)[0].(map[string]any)
	require.Equal(t, "10000000", balance["balance_amount"])
	require.Equal(t, "3600000", balance["accrued_amount"])
	require.Equal(t, "6400000", balance["available_amount"])
	require.Equal(t, "3600000", balance["accrual_rate_per_hour"])
	require.NotNil(t, balance["depletes_at"])

	stoppedAt := startedAt.Add(30 * time.Minute)
	stop := map[string]any{"stopped_at": stoppedAt.Format(time.RFC3339)}
	status, stopped := w.merchantJSON(usage, http.MethodPost, path+"/stop", stop)
	require.Equal(t, http.StatusOK, status, "%v", stopped)
	require.Equal(t, "stopped", stopped["state"])
	require.Equal(t, "1800000", stopped["charged_amount"])
	require.Equal(t, "1800000", stopped["posted_amount"])
	require.Equal(t, stoppedAt.Format(time.RFC3339), stopped["stopped_at"])
	for _, repeat := range []any{stop, map[string]any{}} {
		status, replayed := w.merchantJSON(usage, http.MethodPost, path+"/stop", repeat)
		require.Equal(t, http.StatusOK, status, "%v", replayed)
		require.Equal(t, stopped, replayed)
	}
	status, body = w.merchantJSON(usage, http.MethodPost, path+"/stop", map[string]any{"stopped_at": stoppedAt.Add(time.Second).Format(time.RFC3339)})
	require.Equal(t, "stopped_at", refusal(status, body, http.StatusUnprocessableEntity, billing.CodeIdempotencyKeyReused)["param"])
	status, body = w.merchantJSON(usage, http.MethodPost, "/v1/app/running-charges/never-started/stop", map[string]any{})
	refusal(status, body, http.StatusNotFound, "running_charge_not_found")
	bal, err := w.client[embedded].GetBalance(t.Context(), c.cid(), "USD")
	require.NoError(t, err)
	require.EqualValues(t, 8_200_000, bal.BalanceAmount)
	require.Zero(t, bal.AccruedAmount)

	// Every route needs Usage, and an application.
	others := application("but:" + string(appUsage))
	person := w.auth.token(t, "staff")
	for _, call := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/app/running-charges", start},
		{http.MethodPost, path + "/stop", map[string]any{}},
		{http.MethodGet, path, nil},
		{http.MethodGet, "/v1/app/running-charges", nil},
	} {
		status, body := w.merchantJSON(others, call.method, call.path, call.body)
		refusal(status, body, http.StatusForbidden, billing.CodePermissionRequired)
		status, body = w.merchantJSON(person, call.method, call.path, call.body)
		refusal(status, body, http.StatusForbidden, billing.CodeApplicationRequired)
	}
}
