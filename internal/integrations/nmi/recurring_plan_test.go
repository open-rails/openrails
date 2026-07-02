package nmi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient spins a client pointed at the given test server for the
// direct-post, query, and v5 endpoints.
func newTestClient(t *testing.T, serverURL string) *NMIClient {
	t.Helper()
	client, err := NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test-security-key",
	}, false)
	require.NoError(t, err)
	client.DirectPostURL = serverURL
	client.QueryURL = serverURL
	client.V5BaseURL = serverURL
	return client
}

func TestAddRecurringPlan_RequestShapeAndConversion(t *testing.T) {
	var method, path, auth, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		_, _ = w.Write([]byte(`{"object":"plan","id":"openrails-abc"}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)

	err := client.AddRecurringPlan("openrails-abc", "Premium Monthly", 999, 30, 0)
	require.NoError(t, err)

	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/plans", path)
	// Bare key, no Bearer/scheme prefix.
	assert.Equal(t, "test-security-key", auth)
	// Wire-pinned money boundary (#671): 999 cents -> JSON number 9.99.
	assert.JSONEq(t, `{"id":"openrails-abc","plan_name":"Premium Monthly","plan_amount":9.99,"plan_payments":0,"day_frequency":30}`, body)
}

func TestAddRecurringPlan_ValidatesInput(t *testing.T) {
	client := newTestClient(t, "http://unused.example.com")

	require.Error(t, client.AddRecurringPlan("", "name", 100, 30, 0))
	require.Error(t, client.AddRecurringPlan("id", "", 100, 30, 0))
	require.Error(t, client.AddRecurringPlan("id", "name", 100, 0, 0))
}

func TestAddRecurringPlan_SurfacesDeclineError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"validationError","error_code":"E_VALIDATION","message":"Plan already exists"}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	err := client.AddRecurringPlan("dup", "Dup", 500, 30, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Plan already exists")
}

func TestEditRecurringPlan_OnlyMutableFields(t *testing.T) {
	// Classic direct post: PATCH /v5/plans/{id} does not exist on the live
	// gateway (E_ROUTE_NOT_FOUND, verified 2026-07-01).
	var seen url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		seen = r.Form
		_, _ = w.Write([]byte("response=1&responsetext=SUCCESS"))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	err := client.EditRecurringPlan("openrails-abc", "New Name", 1999)
	require.NoError(t, err)

	assert.Equal(t, "edit_plan", seen.Get("recurring"))
	// current_plan_id, NOT plan_id — the live gateway rejects the latter.
	assert.Equal(t, "openrails-abc", seen.Get("current_plan_id"))
	assert.Empty(t, seen.Get("plan_id"))
	assert.Equal(t, "New Name", seen.Get("plan_name"))
	assert.Equal(t, "19.99", seen.Get("plan_amount"))
	// Frequency/payments are immutable and must not be sent.
	assert.Empty(t, seen.Get("day_frequency"))
	assert.Empty(t, seen.Get("plan_payments"))
}

func TestEditRecurringPlan_RequiresPlanID(t *testing.T) {
	client := newTestClient(t, "http://unused.example.com")
	require.Error(t, client.EditRecurringPlan("", "name", 100))
}

func TestGetRecurringPlanByID_FoundParsesAmount(t *testing.T) {
	var method, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_, _ = w.Write([]byte(`{"object":"plan","id":"openrails-abc","plan_name":"Premium Monthly","plan_amount":"9.99","plan_payments":"0","day_frequency":"30"}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	found, name, cents, err := client.GetRecurringPlanByID("openrails-abc")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "Premium Monthly", name)
	assert.Equal(t, int64(999), cents)
	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, "/plans/openrails-abc", path)
}

func TestGetRecurringPlanDetailByID_ParsesAmountAndFrequency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"plan","id":"premium-usd-999-30","plan_name":"Premium Monthly","plan_amount":"9.99","day_frequency":"30"}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	detail, err := client.GetRecurringPlanDetailByID("premium-usd-999-30")
	require.NoError(t, err)
	assert.True(t, detail.Found)
	assert.Equal(t, "Premium Monthly", detail.Name)
	assert.Equal(t, int64(999), detail.AmountCents)
	assert.Equal(t, 30, detail.DayFrequency)
}

func TestGetRecurringPlanDetailByID_MonthPlanHasZeroDayFrequency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"plan","id":"monthly","plan_name":"Monthly","plan_amount":"5.00","day_frequency":"0","month_frequency":"1","day_of_month":"15"}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	detail, err := client.GetRecurringPlanDetailByID("monthly")
	require.NoError(t, err)
	assert.True(t, detail.Found)
	assert.Equal(t, 0, detail.DayFrequency)
}

func TestGetRecurringPlanByID_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"type":"notFound","error_code":"E_NOT_FOUND","message":"plan not found"}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	found, _, _, err := client.GetRecurringPlanByID("missing")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestListRecurringPlans_FollowsCursor(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"plans":[{"object":"plan","id":"a","plan_amount":"1.00"}],"next_cursor":"7","has_more":true,"per_page":1}`))
			return
		}
		assert.Equal(t, "7", r.URL.Query().Get("cursor"))
		_, _ = w.Write([]byte(`{"plans":[{"object":"plan","id":"b","plan_amount":"2.00"}],"next_cursor":null,"has_more":false,"per_page":1}`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	plans, err := client.ListRecurringPlans()
	require.NoError(t, err)
	require.Len(t, plans, 2)
	assert.Equal(t, "a", plans[0].ID)
	assert.Equal(t, "b", plans[1].ID)
	assert.Equal(t, 2, calls)
}

// TestCentsAmountWirePinning pins the cents -> JSON-number rendering at the
// money wire boundary (#671): known cents must produce these exact bytes.
func TestCentsAmountWirePinning(t *testing.T) {
	cases := map[int64]string{
		0:      "0.00",
		1:      "0.01",
		99:     "0.99",
		100:    "1.00",
		999:    "9.99",
		1999:   "19.99",
		123456: "1234.56",
		-500:   "-5.00",
	}
	for cents, wire := range cases {
		assert.Equal(t, wire, string(centsJSONAmount(moneyutil.Cents(cents))), "wire for %d cents", cents)
	}
	// And the reverse: v5 decimal strings parse back to exact cents.
	for cents, wire := range cases {
		got, err := v5AmountToCents(wire)
		require.NoError(t, err)
		assert.Equal(t, cents, got, "parse of %q", wire)
	}
}
