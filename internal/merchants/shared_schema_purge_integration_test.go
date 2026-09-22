//go:build integration

package merchants

import (
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestSharedSchemaPurgePreservesHostRecordsAndRiverJobs(t *testing.T) {
	super, app := dbtest.SharedRLSPostgres(t)
	dbtest.ApplyPostgresMigrations(t, super, app, "public")
	pool := dbtest.SharedPGXPool(t)
	owner := dbtest.SharedSuperuserPGXPool(t)
	mid := merchant.ID(uuid.New())
	slug := "shared-purge-" + mid.String()
	_, err := owner.Exec(t.Context(), `CREATE TABLE IF NOT EXISTS public.host_purge_records(merchant_id uuid PRIMARY KEY,value text)`)
	require.NoError(t, err)
	_, err = owner.Exec(t.Context(), `INSERT INTO public.host_purge_records VALUES($1,'host-only')`, mid.UUID())
	require.NoError(t, err)
	_, err = owner.Exec(t.Context(), `INSERT INTO public.merchants(id,slug) VALUES($1,$2)`, mid.UUID(), slug)
	require.NoError(t, err)
	var jobID int64
	require.NoError(t, owner.QueryRow(t.Context(), `INSERT INTO public.river_job(kind,args,queue,state,scheduled_at) VALUES('host-only','{}','host','scheduled',now()+interval '1 day') RETURNING id`).Scan(&jobID))
	_, err = owner.Exec(t.Context(), `INSERT INTO public.products(merchant_id,id,key,display_name) VALUES($1,$2,'purge-product','Purge product')`, mid.UUID(), uuid.New())
	require.NoError(t, err)
	service, err := NewService(db.WrapPool(pool, "public"), nil, "test")
	require.NoError(t, err)
	service.WithDestructivePolicy(allowAllDestructive{})
	inventory, err := service.TakePurgeInventory(t.Context(), mid)
	require.NoError(t, err)
	require.NoError(t, service.Delete(t.Context(), mid, DeleteOptions{ConfirmPhrase: PurgeConfirmPhrase(slug), ExpectRows: &inventory.TotalRows, InventoryID: inventory.ID, Actor: "isolated-test"}))
	var value string
	require.NoError(t, owner.QueryRow(t.Context(), `SELECT value FROM public.host_purge_records WHERE merchant_id=$1`, mid.UUID()).Scan(&value))
	require.Equal(t, "host-only", value)
	require.NoError(t, owner.QueryRow(t.Context(), `SELECT kind FROM public.river_job WHERE id=$1`, jobID).Scan(&value))
	require.Equal(t, "host-only", value)
	require.NoError(t, owner.QueryRow(t.Context(), `SELECT status FROM public.merchants WHERE id=$1`, mid.UUID()).Scan(&value))
	require.Equal(t, "deleted", value)
}
