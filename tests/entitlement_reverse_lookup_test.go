//go:build integration

package tests

import (
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
)

// TestListCustomersWithEntitlement_Reverse validates the #535 reverse lookup
// against real Postgres: only customers holding an ACTIVE, non-revoked,
// non-deleted window of the entitlement are returned, ordered by customer_id, and
// keyset pagination walks the set. This is what backs AuthKit's directory
// filter-by-entitlement (#91).
func TestListCustomersWithEntitlement_Reverse(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := suite.MerchantCtx()
	svc := suite.App.Runtime.EntitlementService
	now := time.Now().UTC()

	mkCustomer := func() (string, uuid.UUID) {
		userID := uuid.New().String()
		cid := suite.ensureCustomer(ctx, userID)
		return userID, cid
	}
	ent := func(cid uuid.UUID, name string, start time.Time, end *time.Time, revokedAt *time.Time) {
		sourceID := uuid.New()
		e := &models.Entitlement{
			ID:          uuid.New(),
			CustomerID:  cid,
			Entitlement: name,
			StartAt:     start,
			EndAt:       end,
			SourceID:    &sourceID,
			SourceType:  models.EntitlementSourceAdmin,
			RevokedAt:   revokedAt,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if revokedAt != nil {
			reason := models.EntitlementRevokeAdmin // revoke fields must be set together (chk_revoke_fields_together)
			e.RevokeReason = &reason
		}
		suite.InsertEntitlement(ctx, e)
	}
	future := now.Add(30 * 24 * time.Hour)
	pastEnd := now.Add(-time.Hour)
	revoked := now.Add(-30 * time.Minute)

	// Use a UNIQUE entitlement name so this test is isolated from other suite data.
	premium := "premium-rev-" + uuid.NewString()[:8]

	_, idA := mkCustomer() // active, finite window -> INCLUDED
	ent(idA, premium, now.Add(-time.Hour), &future, nil)
	_, idB := mkCustomer() // active, indefinite -> INCLUDED
	ent(idB, premium, now.Add(-time.Hour), nil, nil)
	_, idC := mkCustomer() // expired -> excluded
	ent(idC, premium, now.Add(-48*time.Hour), &pastEnd, nil)
	_, idD := mkCustomer() // revoked -> excluded
	ent(idD, premium, now.Add(-time.Hour), &future, &revoked)
	_, idE := mkCustomer() // different entitlement -> excluded
	ent(idE, "basic-rev-"+uuid.NewString()[:8], now.Add(-time.Hour), &future, nil)
	_, idF := mkCustomer() // active then soft-deleted -> excluded
	ent(idF, premium, now.Add(-time.Hour), &future, nil)
	_, err := suite.Pool.Exec(ctx, `UPDATE openrails.entitlements SET deleted_at = now() WHERE customer_id = $1`, idF)
	require.NoError(t, err)

	want := []uuid.UUID{idA, idB}
	sort.Slice(want, func(i, j int) bool { return want[i].String() < want[j].String() })

	t.Run("returns only active, non-revoked, non-deleted matches, ordered by customer_id", func(t *testing.T) {
		got, err := svc.ListCustomersWithEntitlement(ctx, premium, now, uuid.Nil, 100)
		require.NoError(t, err)
		require.Equal(t, want, got)
	})

	t.Run("keyset pagination walks the set", func(t *testing.T) {
		page1, err := svc.ListCustomersWithEntitlement(ctx, premium, now, uuid.Nil, 1)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{want[0]}, page1)

		page2, err := svc.ListCustomersWithEntitlement(ctx, premium, now, page1[0], 1)
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{want[1]}, page2)

		page3, err := svc.ListCustomersWithEntitlement(ctx, premium, now, page2[0], 1)
		require.NoError(t, err)
		require.Empty(t, page3)
	})

	t.Run("empty entitlement is rejected", func(t *testing.T) {
		_, err := svc.ListCustomersWithEntitlement(ctx, "  ", now, uuid.Nil, 100)
		require.Error(t, err)
	})

	_ = idC
	_ = idD
	_ = idE
}
