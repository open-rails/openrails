//go:build integration

package integrationharness

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
)

// #535: the SDK Client.ListCustomersWithEntitlement (remote transport) walks the
// keyset-paginated reverse route and returns the active set, excluding
// expired/other-entitlement rows.
func TestEntitlementReverseLookupSDKClient(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	surface := h.StartStandalone("usd")
	now := time.Now().UTC()
	premium := "premium-sdk-" + uuid.NewString()[:8]

	client := openrails.NewRemote(surface.BaseURL,
		openrails.WithTokenProvider(func(context.Context) (string, error) { return surface.Token, nil }))

	mkCustomer := func() uuid.UUID {
		return dbtest.EnsureCustomerIDPgx(ctx, t, h.Pool(), uuid.New().String())
	}
	insert := func(customer uuid.UUID, ent string, start time.Time, end *time.Time) {
		_, err := h.Pool().Exec(ctx, `
			INSERT INTO openrails.entitlements (
				id, merchant_id, customer_id, entitlement, start_at, end_at,
				source_id, source_type, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, 'admin', $8, $8)`,
			uuid.New(), dbtest.TestMerchantID.UUID(), customer, ent, start, end, uuid.New(), now)
		require.NoError(t, err)
	}

	future := now.Add(24 * time.Hour)
	expired := now.Add(-time.Hour)
	a, b := mkCustomer(), mkCustomer()
	insert(a, premium, now.Add(-time.Hour), &future)
	insert(b, premium, now.Add(-time.Hour), nil)
	insert(mkCustomer(), premium, now.Add(-48*time.Hour), &expired) // expired -> excluded
	insert(mkCustomer(), "other-sdk", now.Add(-time.Hour), &future) // other entitlement -> excluded

	got, err := client.ListCustomersWithEntitlement(ctx, premium, now)
	require.NoError(t, err)
	sort.Strings(got)
	want := []string{a.String(), b.String()}
	sort.Strings(want)
	require.Equal(t, want, got)
}
