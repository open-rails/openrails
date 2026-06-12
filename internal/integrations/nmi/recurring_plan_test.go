package nmi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient spins a client pointed at the given test server for both the
// direct-post and query endpoints.
func newTestClient(t *testing.T, serverURL string) *NMIClient {
	t.Helper()
	client, err := NewClient("mobius", &config.NMIProviderSettings{
		SecurityKey: "test-security-key",
	}, false)
	require.NoError(t, err)
	client.DirectPostURL = serverURL
	client.QueryURL = serverURL
	return client
}

func TestAddRecurringPlan_RequestShapeAndConversion(t *testing.T) {
	var seen url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		seen = r.Form
		_, _ = w.Write([]byte("response=1&responsetext=SUCCESS"))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)

	err := client.AddRecurringPlan("openrails-abc", "Premium Monthly", 999, 30, 0)
	require.NoError(t, err)

	assert.Equal(t, "add_plan", seen.Get("recurring"))
	assert.Equal(t, "openrails-abc", seen.Get("plan_id"))
	assert.Equal(t, "Premium Monthly", seen.Get("plan_name"))
	// 999 cents -> "9.99" dollars
	assert.Equal(t, "9.99", seen.Get("plan_amount"))
	assert.Equal(t, "30", seen.Get("day_frequency"))
	assert.Equal(t, "0", seen.Get("plan_payments"))
}

func TestAddRecurringPlan_ValidatesInput(t *testing.T) {
	client := newTestClient(t, "http://unused.example.com")

	require.Error(t, client.AddRecurringPlan("", "name", 100, 30, 0))
	require.Error(t, client.AddRecurringPlan("id", "", 100, 30, 0))
	require.Error(t, client.AddRecurringPlan("id", "name", 100, 0, 0))
}

func TestAddRecurringPlan_SurfacesDeclineError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("response=3&responsetext=Plan already exists"))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	err := client.AddRecurringPlan("dup", "Dup", 500, 30, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Plan already exists")
}

func TestEditRecurringPlan_OnlyMutableFields(t *testing.T) {
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
	assert.Equal(t, "openrails-abc", seen.Get("plan_id"))
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
	var seen url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		seen = r.Form
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<nm_response>
  <plan>
    <plan_id>openrails-abc</plan_id>
    <plan_name>Premium Monthly</plan_name>
    <plan_amount>9.99</plan_amount>
  </plan>
</nm_response>`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	found, name, cents, err := client.GetRecurringPlanByID("openrails-abc")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "Premium Monthly", name)
	assert.Equal(t, int64(999), cents)
	assert.Equal(t, "recurring_plans", seen.Get("report_type"))
	assert.Equal(t, "openrails-abc", seen.Get("plan_id"))
}

func TestGetRecurringPlanDetailByID_ParsesAmountAndFrequency(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?>
<nm_response>
  <plan>
    <plan_id>premium-usd-999-30</plan_id>
    <plan_name>Premium Monthly</plan_name>
    <plan_amount>9.99</plan_amount>
    <day_frequency>30</day_frequency>
  </plan>
</nm_response>`))
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

func TestGetRecurringPlanByID_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<nm_response></nm_response>`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	found, _, _, err := client.GetRecurringPlanByID("missing")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestGetRecurringPlanByID_FiltersByID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<nm_response>
  <plan><plan_id>other</plan_id><plan_name>Other</plan_name><plan_amount>1.00</plan_amount></plan>
  <plan><plan_id>wanted</plan_id><plan_name>Wanted</plan_name><plan_amount>5.00</plan_amount></plan>
</nm_response>`))
	}))
	t.Cleanup(server.Close)

	client := newTestClient(t, server.URL)
	found, name, cents, err := client.GetRecurringPlanByID("wanted")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "Wanted", name)
	assert.Equal(t, int64(500), cents)
}

func TestCentsDollarRoundTrip(t *testing.T) {
	cases := []int64{0, 1, 99, 100, 999, 1999, 123456}
	for _, c := range cases {
		got, err := dollarStringToCents(centsToDollarString(c))
		require.NoError(t, err)
		assert.Equal(t, c, got, "round trip for %d cents", c)
	}
}
