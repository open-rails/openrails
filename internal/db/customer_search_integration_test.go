//go:build integration

package db_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
)

// TestSearchCustomersScopedToMerchant is the regression test for the console
// customer list leaking across merchants on a BYPASSRLS connection: before
// SearchCustomers/CountSearchCustomers carried an explicit merchant predicate,
// this test's cross-merchant assertions failed (every merchant's customers came
// back). It therefore needs a privileged handle ON PURPOSE — with RLS enforcing,
// the policy would hold the line and the query's own predicate would go untested.
func TestSearchCustomersScopedToMerchant(t *testing.T) {
	ctx := context.Background()
	pool := dbtest.SharedSuperuserPGXPool(t)
	dbtest.EnsureTestMerchant(ctx, t, pool)

	otherMerchantID := uuid.New()
	otherSlug := fmt.Sprintf("othr%x", time.Now().UnixNano())
	_, err := pool.Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug, status) VALUES ($1, $2, 'active')`,
		otherMerchantID, otherSlug)
	require.NoError(t, err)

	mine := uuid.New()
	theirs := uuid.New()
	for _, row := range []struct {
		id       uuid.UUID
		merchant uuid.UUID
	}{
		{mine, dbtest.TestMerchantID.UUID()},
		{theirs, otherMerchantID},
	} {
		_, err := pool.Exec(ctx,
			`INSERT INTO openrails.customers (id, merchant_id, subject) VALUES ($1, $2, $3)`,
			row.id, row.merchant, row.id.String())
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.customers WHERE id IN ($1, $2)`, mine, theirs)
		_, _ = pool.Exec(ctx, `DELETE FROM openrails.merchants WHERE id = $1`, otherMerchantID)
	})

	queries := gen.New(pool)

	// Unfiltered list: only the scoped merchant's customer comes back.
	rows, err := queries.SearchCustomers(ctx, gen.SearchCustomersParams{
		MerchantID: dbtest.TestMerchantID.UUID(),
		Q:          "",
		PageLimit:  200,
		PageOffset: 0,
	})
	require.NoError(t, err)
	seen := map[uuid.UUID]bool{}
	for _, row := range rows {
		seen[row.ID] = true
	}
	require.True(t, seen[mine], "scoped merchant's customer missing from its own list")
	require.False(t, seen[theirs], "another merchant's customer leaked into the list")

	// Searching by the other merchant's exact customer id must not cross scopes.
	rows, err = queries.SearchCustomers(ctx, gen.SearchCustomersParams{
		MerchantID: dbtest.TestMerchantID.UUID(),
		Q:          theirs.String(),
		PageLimit:  200,
		PageOffset: 0,
	})
	require.NoError(t, err)
	require.Empty(t, rows, "searching a foreign customer id must return nothing")

	// The count obeys the same scope.
	count, err := queries.CountSearchCustomers(ctx, gen.CountSearchCustomersParams{
		MerchantID: otherMerchantID,
		Q:          "",
	})
	require.NoError(t, err)
	require.EqualValues(t, 1, count, "other merchant's count must see exactly its own customer")
}
