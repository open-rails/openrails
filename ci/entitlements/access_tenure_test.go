//go:build e2e && integration

package entitlements_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func TestAccessTenureUsesHistoricalKeysAndElapsedWindowUnion(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	customer := billing.CustomerID(uuid.New())
	start := f.clock.Now()
	day := 24 * time.Hour
	product := func(key string) *billing.Product {
		t.Helper()
		p, err := f.client.CreateProduct(ctx, billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"premium"}})
		require.NoError(t, err)
		return p
	}
	grant := func(p *billing.Product, end *time.Time) billing.ProductAccessID {
		t.Helper()
		out, err := f.client.CreateProductAccess(ctx, billing.CreateProductAccessBatchParams{Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: p.ID, EndsAt: end}}})
		require.NoError(t, err)
		return out[0].ID
	}
	first := grant(product("first"), nil)
	f.clock.Advance(5 * day)
	second := product("second")
	end := start.Add(15 * day)
	grant(second, &end)
	f.clock.Advance(5 * day)
	require.NoError(t, revokeAccess(f.client, ctx, first))
	f.clock.Advance(10 * day)
	third := product("third")
	grant(third, nil)
	f.clock.Advance(5 * day)
	_, err := f.client.UpdateProduct(ctx, third.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{})})
	require.NoError(t, err)
	f.clock.Advance(5 * day)
	_, err = f.client.UpdateProduct(ctx, third.ID, billing.UpdateProductParams{Entitlements: catalog.Value([]string{"premium"})})
	require.NoError(t, err)
	f.clock.Advance(5 * day)
	_, err = f.client.UpdateProduct(ctx, second.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
	require.NoError(t, err)

	f.executed.ran()
	got, err := f.client.GetAccessTenure(ctx, billing.AccessTenureParams{CustomerID: customer, Entitlements: []string{"premium", "never-held"}})
	require.NoError(t, err)
	require.Equal(t, f.clock.Now(), got.AsOf)
	require.Equal(t, map[string]int64{"premium": int64(25 * day / time.Second), "never-held": 0}, got.Seconds)
	require.Equal(t, 1, f.executed.ran()["GetAccessTenure"], "the authoritative aggregate is one database query")

	past := start.Add(24 * day)
	got, err = f.client.GetAccessTenure(ctx, billing.AccessTenureParams{CustomerID: customer, Entitlements: []string{"premium"}, At: past})
	require.NoError(t, err)
	require.Equal(t, int64(19*day/time.Second), got.Seconds["premium"])

	got, err = f.client.GetAccessTenure(ctx, billing.AccessTenureParams{CustomerID: billing.CustomerID(uuid.New()), Entitlements: []string{"premium"}})
	require.NoError(t, err)
	require.Zero(t, got.Seconds["premium"], "another customer has no access from this customer")
	other := newFixture(t)
	got, err = other.client.GetAccessTenure(ctx, billing.AccessTenureParams{CustomerID: customer, Entitlements: []string{"premium"}})
	require.NoError(t, err)
	require.Zero(t, got.Seconds["premium"], "another merchant has none of this merchant's history")

	_, err = f.client.GetAccessTenure(ctx, billing.AccessTenureParams{CustomerID: customer, Entitlements: []string{"premium"}, At: f.clock.Now().Add(time.Second)})
	invalidParam(t, err, "at")
}
