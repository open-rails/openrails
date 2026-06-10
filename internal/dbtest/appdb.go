//go:build integration

package dbtest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
)

// OpenAppDB returns a pool-backed *db.DB on the given DSN, closed on test
// cleanup.
func OpenAppDB(t *testing.T, dsn string) *db.DB {
	t.Helper()
	d, err := db.NewDB(&config.DBConfig{URL: dsn})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}
