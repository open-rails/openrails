//go:build integration

package tests

import (
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/stretchr/testify/require"
)

// Directory filtering is a distinct reverse-query boundary: active finite and
// standing subjects appear once; expired, revoked and deleted projections do not.
func TestListCustomersWithEntitlement_Reverse(t *testing.T) {
	d := dbtest.OpenMerchantDB(t, dbtest.TestMerchantID.UUID())
	ctx := dbtest.WithTestMerchant(t.Context())
	pool := d.Pool()
	svc := entitlements.NewEntitlementService(d)
	now := time.Now().UTC().Truncate(time.Microsecond)
	name := "reverse-" + uuid.NewString()
	var want []uuid.UUID
	for _, state := range []string{"finite", "standing", "expired", "revoked", "other", "deleted"} {
		id := dbtest.EnsureCustomerIDPgx(ctx, t, pool, uuid.NewString())
		end := now.Add(time.Hour)
		var until, revoked, deleted *time.Time
		if state != "standing" {
			until = &end
		}
		if state == "expired" {
			past := now.Add(-time.Second)
			until = &past
		}
		if state == "revoked" {
			revoked = &now
		}
		if state == "deleted" {
			deleted = &now
		}
		entitlement := name
		if state == "other" {
			entitlement += "-other"
		}
		var reason *string
		if revoked != nil {
			v := "admin"
			reason = &v
		}
		_, err := pool.Exec(ctx, `INSERT INTO billing.entitlements(id,merchant_id,customer_id,entitlement,start_at,end_at,source_type,source_id,revoked_at,revoke_reason,deleted_at) VALUES($1,$2,$3,$4,$5,$6,'admin',$7,$8,$9,$10)`, uuid.New(), dbtest.TestMerchantID.UUID(), id, entitlement, now.Add(-time.Hour), until, uuid.New(), revoked, reason, deleted)
		require.NoError(t, err)
		if state == "finite" || state == "standing" {
			want = append(want, id)
		}
	}
	sort.Slice(want, func(i, j int) bool { return want[i].String() < want[j].String() })
	got, err := svc.ListCustomersWithEntitlement(ctx, name, now, uuid.Nil, 100)
	require.NoError(t, err)
	require.Equal(t, want, got)
	after := uuid.Nil
	for _, id := range want {
		got, err = svc.ListCustomersWithEntitlement(ctx, name, now, after, 1)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{id}, got)
		after = id
	}
	got, err = svc.ListCustomersWithEntitlement(ctx, name, now, after, 1)
	require.NoError(t, err)
	require.Empty(t, got)
	_, err = svc.ListCustomersWithEntitlement(ctx, " ", now, uuid.Nil, 100)
	require.Error(t, err)
}
