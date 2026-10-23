package eventlog

import (
	"os"
	"strings"
	"testing"

	sharedpg "github.com/open-rails/helpers/eventlog/postgres"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestSharedSchemaIsCanonical(t *testing.T) {
	b, err := os.ReadFile("../migrate/postgres/0113_shared_eventlog_contract.up.sql")
	require.NoError(t, err)
	_, tail, ok := strings.Cut(string(b), "-- BEGIN helpers/eventlog/postgres schema v1 (verbatim; checked by test)\n")
	require.True(t, ok)
	actual, _, ok := strings.Cut(tail, "-- END helpers/eventlog/postgres schema v1")
	require.True(t, ok)
	want, err := sharedpg.MigrationSQL("billing", 1)
	require.NoError(t, err)
	require.Equal(t, want, actual)
}

func TestSharedStorageMigrationRollbackAndDeletion(t *testing.T) {
	f := newFixture(t)
	mid, customer := f.merchant(t)
	f.add(t, mid, customer, "before")
	before := f.list(t, mid, billing.EventQuery{})
	b, err := os.ReadFile("../migrate/postgres/0113_shared_eventlog_contract.up.sql")
	require.NoError(t, err)
	tx, err := f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), f.sql(string(b)))
	require.NoError(t, err)
	require.NoError(t, tx.Rollback(t.Context()))
	require.Equal(t, before, f.list(t, mid, billing.EventQuery{}))
	tx, err = f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), f.sql(string(b)))
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
	f.shared = true
	f.add(t, mid, customer, "after")
	after := f.list(t, mid, billing.EventQuery{After: before.NextCursor})
	require.Len(t, after.Events, 1)
	require.Equal(t, "after", after.Events[0].Type)
	tx, err = f.pool.Begin(t.Context())
	require.NoError(t, err)
	_, err = f.insert(t.Context(), tx, mid, customer, "deleted-in-same-tx")
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), f.sql(`DELETE FROM billing.customers WHERE merchant_id=$1;`), mid.UUID())
	require.NoError(t, err)
	_, err = tx.Exec(t.Context(), f.sql(`DELETE FROM billing.merchants WHERE id=$1`), mid.UUID())
	require.NoError(t, err)
	require.NoError(t, tx.Commit(t.Context()))
	var count int
	require.NoError(t, f.pool.QueryRow(t.Context(), f.sql(`SELECT count(*) FROM billing.eventlog_events WHERE merchant_id=$1`), mid.UUID()).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, f.pool.QueryRow(t.Context(), f.sql(`SELECT count(*) FROM billing.eventlog_streams WHERE merchant_id=$1`), mid.UUID()).Scan(&count))
	require.Zero(t, count)
	f.pendingEmpty(t)
}
