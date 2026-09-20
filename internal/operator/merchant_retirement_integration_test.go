//go:build integration

package operator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestMerchantRetirementBoundary proves the core half of hosted dormancy with
// no host state: activity facts, namespace refusals, the irreversible tombstone
// and slug release through the real AuthKit core.
func TestMerchantRetirementBoundary(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	cfg := hostedTestConfig(t, dsn, "https://retire1009.openrails.test")
	e := newHostApp(t, cfg)
	sfx := strings.ToLower(uuid.NewString()[:8])
	hostReserved := "retire-vip-" + sfx
	require.NoError(t, embcp.AttachWithOptions(ctx, e.App(), cfg, nil, embcp.AttachOptions{
		HostedPosture:    true,
		EmailSender:      &captureEmailSender{},
		MerchantCreation: &embcp.MerchantCreationConfig{ReservedSlugs: []string{hostReserved}},
	}))
	core := embcp.Get(e.App()).Core()
	pool, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	provision := func(slug string, age time.Duration) embcp.ProvisionMerchantResult {
		t.Helper()
		res, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: slug})
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE billing.merchants SET created_at=now()-make_interval(secs => $2) WHERE id=$1`,
			res.MerchantID.UUID(), age.Seconds())
		require.NoError(t, err)
		return *res
	}
	addCustomer := func(id merchant.ID) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, `SELECT set_config('app.merchant_id', $1, true)`, id.String())
		require.NoError(t, err)
		_, err = tx.Exec(ctx, `INSERT INTO billing.customers (merchant_id, issuer, id) VALUES ($1, 'test', $2)`, id.UUID(), uuid.NewString())
		require.NoError(t, err)
		require.NoError(t, tx.Commit(ctx))
	}

	unused := provision("retire-unused-"+sfx, 72*time.Hour)
	active := provision("retire-active-"+sfx, 72*time.Hour)
	young := provision("retire-young-"+sfx, 0)
	reserved := provision(hostReserved, 72*time.Hour)
	addCustomer(active.MerchantID)

	t.Run("candidates carry activity facts and exclude young and reserved merchants", func(t *testing.T) {
		seen := map[merchant.ID]embcp.MerchantRetirementCandidate{}
		var after *embcp.MerchantRetirementCursor
		for {
			page, err := embcp.ListMerchantRetirementCandidates(ctx, e.App(), embcp.MerchantRetirementCandidatesRequest{
				CreatedBefore: time.Now().Add(-24 * time.Hour), After: after, Limit: 2,
			})
			require.NoError(t, err)
			for _, c := range page.Candidates {
				_, dup := seen[c.MerchantID]
				require.False(t, dup, "keyset pages must not repeat %s", c.MerchantID)
				seen[c.MerchantID] = c
			}
			if page.Next == nil {
				break
			}
			after = page.Next
		}
		require.Contains(t, seen, unused.MerchantID)
		require.False(t, seen[unused.MerchantID].Used)
		require.Equal(t, unused.GroupID, seen[unused.MerchantID].GroupID)
		require.Contains(t, seen, active.MerchantID)
		require.True(t, seen[active.MerchantID].Used)
		require.NotContains(t, seen, young.MerchantID)
		require.NotContains(t, seen, reserved.MerchantID)

		_, err := embcp.ListMerchantRetirementCandidates(ctx, e.App(), embcp.MerchantRetirementCandidatesRequest{
			CreatedBefore: time.Now(), Limit: 0,
		})
		require.Error(t, err, "an unbounded page is refused")
	})

	t.Run("unsafe retirements are refused without side effects", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			id      merchant.ID
			group   string
			refusal embcp.MerchantRetirementRefusal
		}{
			{"activity", active.MerchantID, active.GroupID, embcp.MerchantRetirementRefusedActive},
			{"different group UUID", unused.MerchantID, uuid.NewString(), embcp.MerchantRetirementRefusedGroupMismatch},
			{"reserved namespace", reserved.MerchantID, reserved.GroupID, embcp.MerchantRetirementRefusedReserved},
			{"unknown merchant", merchant.ID(uuid.New()), uuid.NewString(), embcp.MerchantRetirementRefusedNotLive},
		} {
			res, err := embcp.RetireUnusedMerchant(ctx, e.App(), tc.id, tc.group)
			require.NoError(t, err, tc.name)
			require.False(t, res.Retired, tc.name)
			require.Equal(t, tc.refusal, res.Refusal, tc.name)
		}
		for _, m := range []embcp.ProvisionMerchantResult{active, unused, reserved} {
			_, err := core.GroupInstanceByID(ctx, m.GroupID)
			require.NoError(t, err, "refused retirement kept group %s", m.GroupID)
		}
		_, err := embcp.RetireUnusedMerchant(ctx, e.App(), unused.MerchantID, "")
		require.Error(t, err, "the expected group UUID is mandatory")
	})

	t.Run("unused merchant retires irreversibly and releases its name", func(t *testing.T) {
		res, err := embcp.RetireUnusedMerchant(ctx, e.App(), unused.MerchantID, unused.GroupID)
		require.NoError(t, err)
		require.True(t, res.Retired)
		_, err = core.GroupInstanceByID(ctx, unused.GroupID)
		require.ErrorIs(t, err, authkit.ErrGroupNotFound)
		_, err = core.GroupInstanceForSlug(ctx, embcp.MerchantGroup("retire-unused-"+sfx))
		require.ErrorIs(t, err, authkit.ErrGroupNotFound, "released, not tombstoned")

		var status string
		var released bool
		require.NoError(t, pool.QueryRow(ctx, `SELECT status, group_release_completed_at IS NOT NULL
			FROM billing.merchants WHERE id=$1`, unused.MerchantID.UUID()).Scan(&status, &released))
		require.Equal(t, "deleted", status)
		require.True(t, released)
		_, err = pool.Exec(ctx, `UPDATE billing.merchants SET deleted_at=NULL,status='active' WHERE id=$1`, unused.MerchantID.UUID())
		require.ErrorContains(t, err, "cannot be restored")

		again, err := embcp.RetireUnusedMerchant(ctx, e.App(), unused.MerchantID, unused.GroupID)
		require.NoError(t, err)
		require.Equal(t, embcp.MerchantRetirementRefusedNotLive, again.Refusal)
		completed, err := embcp.CompletePendingMerchantRetirements(ctx, e.App(), 500)
		require.NoError(t, err)
		require.Zero(t, completed)

		reclaimed, err := embcp.ProvisionMerchant(ctx, e.App(), embcp.ProvisionMerchantRequest{Slug: "retire-unused-" + sfx})
		require.NoError(t, err)
		require.True(t, reclaimed.Created)
		require.NotEqual(t, unused.MerchantID, reclaimed.MerchantID)
		require.NotEqual(t, unused.GroupID, reclaimed.GroupID)
	})
}
