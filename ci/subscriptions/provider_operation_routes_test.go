//go:build e2e && integration

package subscriptions_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// A provider operation's lifecycle runs over its seven routes: the host opens
// (201, then 200 on replay), increments, observes or refuses and releases;
// staff list, read and close stuck holds, and a refused hold's finding clears
// on close. The retired routes are gone.
func TestProviderOperationRoutes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	_, err := w.client[embedded].CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{
		{CustomerID: c.cid(), Currency: "USD", Amount: 10_000_000, Source: "support", SourceID: "seed"},
	})
	require.NoError(t, err)
	app, base := "/v1/app/provider-operations", "/v1/admin/provider-operations"
	open := func(id string) (int, map[string]any) {
		t.Helper()
		body := []byte(`{"rental":"` + id + `"}`)
		digest := sha256.Sum256(body)
		return w.hostJSON(http.MethodPost, app, map[string]any{
			"operation_id": id, "customer_id": c.id, "record_owner": "user:1", "currency": "USD", "amount": "1000000",
			"claim_reference": "claim:" + id, "authorization_body": base64.StdEncoding.EncodeToString(body),
			"authorization_body_sha256": hex.EncodeToString(digest[:]),
		})
	}
	ok := func(status int, body map[string]any) map[string]any {
		t.Helper()
		require.Equal(t, http.StatusOK, status, "%v", body)
		return body
	}
	end := w.clock.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	start := end.Add(-time.Hour)
	stamp := func(at time.Time) string { return at.Format(time.RFC3339Nano) }
	evidence := map[string]any{
		"observation_id": "obs-1",
		"lifecycle": map[string]any{
			"provider": "runpod", "provider_resource_id": "pod-1",
			"provider_lifetime_starts_at": stamp(start), "provider_lifetime_ends_at": stamp(end), "provider_absent_at": stamp(end),
			"provider_absence_reference": "absence:1", "billing_stop_reference": "stop:1",
			"windows_closed_at": stamp(end), "windows_closed_reference": "windows:1",
			"lifecycle_evidence_body": base64.StdEncoding.EncodeToString([]byte(`{}`)),
		},
		"normalized_query": "pod=pod-1", "query_starts_at": stamp(start), "query_ends_at": stamp(end),
		"raw_body": base64.StdEncoding.EncodeToString([]byte(`[{"amount":0.4}]`)),
		"records":  []any{map[string]any{"provider_resource_id": "pod-1", "bucket_start": stamp(start), "amount": "400000", "time_billed_ms": "3600000"}},
	}

	status, created := open("rental-1")
	require.Equal(t, http.StatusCreated, status, "%v", created)
	require.Equal(t, false, created["replayed"])
	require.Nil(t, created["qualification"])
	require.Nil(t, created["last_increment"])
	replay := ok(open("rental-1"))
	require.Equal(t, true, replay["replayed"])

	grown := ok(w.hostJSON(http.MethodPost, app+"/rental-1/increment", map[string]any{"ordinal": 1, "amount": "500000", "minimum_amount": "100000"}))
	require.Equal(t, "1500000", grown["authorized_amount"])
	require.Equal(t, "500000", grown["last_increment"].(map[string]any)["granted_amount"])

	observed := ok(w.hostJSON(http.MethodPost, app+"/rental-1/observations", evidence))
	require.Equal(t, "pending", observed["qualification"].(map[string]any)["state"])
	read := ok(w.staffJSON(http.MethodGet, base+"/rental-1", nil))
	require.Equal(t, observed["qualification"], read["qualification"], "the read carries the qualification")
	require.Equal(t, grown["last_increment"], read["last_increment"])

	status, created = open("rental-2")
	require.Equal(t, http.StatusCreated, status, "%v", created)
	refused := ok(w.hostJSON(http.MethodPost, app+"/rental-2/observations", map[string]any{
		"observation_id": "host-1", "refusal": map[string]any{"kind": "provider_billing_unavailable", "detail": "vast has no billing reader"},
	}))
	require.Equal(t, "provider_billing_unavailable", refused["refusal"].(map[string]any)["reason"])
	require.Equal(t, "observation host-1: vast has no billing reader", refused["refusal"].(map[string]any)["detail"])
	status, body := w.hostJSON(http.MethodPost, app+"/rental-2/increment", map[string]any{"ordinal": 1, "amount": "1", "minimum_amount": "1"})
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "provider_operation_refused", body["error"].(map[string]any)["code"])

	stuck := ok(w.staffJSON(http.MethodGet, base+"?refused=true&state=open", nil))
	require.Len(t, stuck["data"], 1)
	require.Equal(t, "rental-2", stuck["data"].([]any)[0].(map[string]any)["operation_id"])

	findings := func() []any {
		t.Helper()
		return ok(w.staffJSON(http.MethodGet, "/v1/admin/findings?type=life.provider_operation.refused", nil))["data"].([]any)
	}
	w.converge()
	raised := findings()
	require.Len(t, raised, 1)
	finding := raised[0].(map[string]any)
	require.Equal(t, "provider_operation:rental-2", finding["subject_key"])
	require.Equal(t, "requires_review", finding["status"])
	require.Contains(t, finding["recommended_action"], "/close")
	status, metrics := w.staffJSON(http.MethodPost, "/v1/admin/metrics/query", map[string]any{
		"measures": []string{"open_findings"}, "range": nowRange(), "filters": map[string][]string{"finding_type": {"life.provider_operation.refused"}},
	})
	require.Equal(t, http.StatusOK, status, "%v", metrics)
	row := metrics["rows"].([]any)[0].([]any)
	require.EqualValues(t, 1, number(t, row[len(row)-1]))

	closed := ok(w.staffJSON(http.MethodPost, base+"/rental-2/close", map[string]any{
		"kind": "written_off", "attested_by": "operator:paul", "reference": "ticket:1",
	}))
	require.Equal(t, "released", closed["state"])
	require.Equal(t, "written_off", closed["resolution"].(map[string]any)["kind"])
	w.converge()
	require.Empty(t, findings(), "the finding clears with the close")

	status, created = open("rental-3")
	require.Equal(t, http.StatusCreated, status, "%v", created)
	released := ok(w.hostJSON(http.MethodPost, app+"/rental-3/release", map[string]any{"release_reference": "never-created"}))
	require.Equal(t, "released", released["state"])

	for _, gone := range []struct{ method, path string }{
		{http.MethodGet, base + "/rental-1/qualification"},
		{http.MethodPost, base + "/rental-1/resolution"},
		{http.MethodPost, base + "/rental-1/refusal"},
		{http.MethodPost, base + "/rental-1/extend"},
		{http.MethodPost, base},
		{http.MethodPost, base + "/rental-1/increment"},
		{http.MethodGet, "/v1/admin/provider-qualifications"},
		{http.MethodPost, "/v1/admin/wasted-spend"},
		{http.MethodGet, "/v1/admin/customers/" + c.id + "/usage"},
	} {
		status, raw := w.staff(gone.method, gone.path)
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, status, "%s %s: %s", gone.method, gone.path, raw)
	}
}
