//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
)

func TestEntitlementReverseLookupStandaloneHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	now := time.Now().UTC()
	premium := "premium-http-" + uuid.NewString()[:8]

	mkCustomer := func() uuid.UUID {
		id := uuid.New()
		return dbtest.EnsureCustomerIDPgx(ctx, t, h.Pool(), id.String())
	}
	insertEntitlement := func(customer uuid.UUID, entitlement string, start time.Time, end *time.Time) {
		_, err := h.Pool().Exec(ctx, `
			INSERT INTO openrails.entitlements (
				id, merchant_id, customer_id, entitlement, start_at, end_at,
				source_id, source_type, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 'admin', $8, $8)
		`, uuid.New(), dbtest.TestMerchantID.UUID(), customer, entitlement, start, end, uuid.New(), now)
		require.NoError(t, err)
	}

	future := now.Add(24 * time.Hour)
	expired := now.Add(-time.Hour)
	activeA := mkCustomer()
	activeB := mkCustomer()
	expiredCustomer := mkCustomer()
	otherEntitlementCustomer := mkCustomer()

	insertEntitlement(activeA, premium, now.Add(-time.Hour), &future)
	insertEntitlement(activeB, premium, now.Add(-time.Hour), nil)
	insertEntitlement(expiredCustomer, premium, now.Add(-48*time.Hour), &expired)
	insertEntitlement(otherEntitlementCustomer, "basic-http-"+uuid.NewString()[:8], now.Add(-time.Hour), &future)

	want := []string{activeA.String(), activeB.String()}
	sort.Strings(want)

	page1 := getEntitlementCustomers(t, surface, premium, now, "", 1)
	require.Equal(t, []string{want[0]}, page1.Customers)
	require.True(t, page1.HasMore)
	require.Equal(t, want[0], page1.NextCursor)

	page2 := getEntitlementCustomers(t, surface, premium, now, page1.NextCursor, 1)
	require.Equal(t, []string{want[1]}, page2.Customers)
	require.True(t, page2.HasMore)
	require.Equal(t, want[1], page2.NextCursor)

	page3 := getEntitlementCustomers(t, surface, premium, now, page2.NextCursor, 1)
	require.Empty(t, page3.Customers)
	require.False(t, page3.HasMore)
	require.Empty(t, page3.NextCursor)
}

type entitlementCustomersResponse struct {
	Customers  []string `json:"customers"`
	NextCursor string   `json:"next_cursor"`
	HasMore    bool     `json:"has_more"`
}

func getEntitlementCustomers(t *testing.T, surface *Surface, entitlement string, at time.Time, cursor string, limit int) entitlementCustomersResponse {
	t.Helper()
	q := url.Values{}
	q.Set("at", at.Format(time.RFC3339Nano))
	q.Set("limit", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	status, body := requestJSON(t, http.MethodGet,
		surface.BaseURL+"/v1/merchant/entitlements/"+url.PathEscape(entitlement)+"/customers?"+q.Encode(),
		surface.Token, nil)
	require.Equalf(t, http.StatusOK, status, "reverse entitlement lookup: %s", string(body))
	var out entitlementCustomersResponse
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}
