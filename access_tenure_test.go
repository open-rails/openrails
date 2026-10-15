package openrails

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestGetAccessTenureRequest(t *testing.T) {
	customer := billing.CustomerID(uuid.New())
	path := "/v1/admin/customers/" + customer.String() + "/access-tenure"
	client, seen := recordingRemote(t, map[string]string{path: `{"as_of":"2026-10-14T12:00:00Z","seconds":{"premium":86400}}`})
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	got, err := client.GetAccessTenure(t.Context(), billing.AccessTenureParams{CustomerID: customer, Entitlements: []string{"supporter", "premium"}, At: at})
	require.NoError(t, err)
	require.Equal(t, at, got.AsOf)
	require.Equal(t, int64(86400), got.Seconds["premium"])
	req := <-seen
	require.Equal(t, "GET", req.method)
	require.Equal(t, path, req.path)
	require.Equal(t, "at=2026-10-14T12%3A00%3A00Z&entitlement=premium&entitlement=supporter", req.query)
}

func TestGetAccessTenureValidatesBeforeSending(t *testing.T) {
	client, seen := recordingRemote(t, nil)
	customer := billing.CustomerID(uuid.New())
	tooMany := make([]string, billing.MaxEntitlementChecks+1)
	for i := range tooMany {
		tooMany[i] = strings.Repeat("x", i+1)
	}
	for _, keys := range [][]string{nil, {}, {""}, {"premium", "premium"}, {strings.Repeat("x", 257)}, tooMany} {
		_, err := client.GetAccessTenure(t.Context(), billing.AccessTenureParams{CustomerID: customer, Entitlements: keys})
		require.Error(t, err)
	}
	_, err := client.GetAccessTenure(t.Context(), billing.AccessTenureParams{Entitlements: []string{"premium"}})
	require.Error(t, err)
	require.Empty(t, seen)
}
