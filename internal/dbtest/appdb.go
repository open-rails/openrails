//go:build integration

package dbtest

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// OpenAppDB returns a pool-backed *db.DB on the given DSN, closed on test
// cleanup.
func OpenAppDB(t *testing.T, dsn string) *db.DB {
	t.Helper()
	d, err := db.NewDB(t.Context(), &config.DBConfig{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// OpenMerchantDB returns a *db.DB on the RLS-enforcing default role with
// app.merchant_id pinned on every connection.
//
// Use it when the test drives a MODULE SERVICE directly — below the layer that
// opens the merchant connection in production (the HTTP router / River worker).
// The test stands in for that layer, so it must supply what that layer supplies.
// Tests that drive the full entry point must NOT use this: proving the code pins
// the merchant itself is their whole point.
func OpenMerchantDB(t *testing.T, merchantID uuid.UUID) *db.DB {
	t.Helper()
	d, err := db.NewWithPGXPool(SharedMerchantPool(t, merchantID), config.DefaultSchema)
	require.NoError(t, err)
	return d
}
