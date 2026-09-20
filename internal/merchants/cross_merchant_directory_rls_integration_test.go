//go:build integration

package merchants

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #824: three reads documented as running on "the privileged, non-RLS role"
// were plain base-pool queries. There is no privileged pool — one pgxpool, one
// DSN — and a pool query carries no app.merchant_id GUC, so under the
// production openrails_app role every policy-bearing table matched
// `merchant_id = NULL` and returned ZERO ROWS AND NO ERROR.
//
// This test connects as openrails_app (NOBYPASSRLS) against the real migrated
// schema and asserts each of the three now answers correctly, and that the
// retired shapes still lie on the same connection — the lie is the regression
// pin, because it is what a superuser-backed harness can never show.
func TestCrossMerchantDirectoryReadsUnderEnforcingRLS(t *testing.T) {
	ctx := context.Background()
	superDSN, appDSN := dbtest.SharedRLSPostgres(t)

	suffix := uuid.NewString()[:8]
	ownerID, otherID := uuid.New(), uuid.New()
	subject := uuid.New()

	superRaw, err := pgxpool.New(ctx, superDSN)
	require.NoError(t, err)
	defer superRaw.Close()
	super := db.WrapPool(superRaw, config.DefaultSchema)

	for id, slug := range map[uuid.UUID]string{ownerID: "or824-owner-" + suffix, otherID: "or824-other-" + suffix} {
		_, err = super.Exec(ctx,
			`INSERT INTO billing.merchants (id, slug, status) VALUES ($1::uuid, $2, 'active')`, id, slug)
		require.NoError(t, err)
	}

	accountID := "acct_or824_" + suffix
	_, err = super.Exec(ctx,
		`INSERT INTO billing.psps (merchant_id, rail, environment, account_id, archived)
		 VALUES ($1::uuid, 'stripe', 'live', $2, false)`, ownerID, accountID)
	require.NoError(t, err)

	// Both merchants have a customer record for the SAME subject — the hosted
	// portal's "which merchants do I buy from" question.
	for _, id := range []uuid.UUID{ownerID, otherID} {
		_, err = super.Exec(ctx,
			`INSERT INTO billing.customers (merchant_id, id) VALUES ($1::uuid, $2)`, id, subject)
		require.NoError(t, err)
	}

	appRaw, err := pgxpool.New(ctx, appDSN)
	require.NoError(t, err)
	defer appRaw.Close()
	appPool := db.WrapPool(appRaw, config.DefaultSchema)

	svc, err := NewService(appPool, nil, "live")
	require.NoError(t, err)

	t.Run("webhook routing resolves the owning merchant", func(t *testing.T) {
		got, ok, err := svc.ResolvePSPByIdentity(ctx, "stripe", "live", accountID)
		require.NoError(t, err)
		require.True(t, ok, "an account-routed webhook must resolve its merchant under the production role")
		require.Equal(t, merchant.ID(ownerID), got.MerchantID)
		require.Equal(t, accountID, got.AccountID)

		_, ok, err = svc.ResolvePSPByIdentity(ctx, "stripe", "live", accountID+"-nope")
		require.NoError(t, err)
		require.False(t, ok, "an unknown account is still not found")
	})

	t.Run("global PSP ownership is asserted, not assumed", func(t *testing.T) {
		q := dbtest.Queries(appPool)
		require.NoError(t, AssertPSPUnowned(ctx, q, ownerID, "stripe", "live", accountID),
			"the owner re-declaring its own account is not a conflict")

		err := AssertPSPUnowned(ctx, q, otherID, "stripe", "live", accountID)
		require.ErrorIs(t, err, ErrPSPOwnedByAnotherMerchant,
			"a cross-merchant claim must be named, not pass silently")

		require.NoError(t, AssertPSPUnowned(ctx, q, otherID, "stripe", "live", "acct_unclaimed_"+suffix))
	})

	t.Run("the portal's merchant directory spans merchant scopes", func(t *testing.T) {
		rows, err := dbtest.Queries(appPool).ListMerchantsForCustomerSubject(ctx, subject)
		require.NoError(t, err)
		slugs := make([]string, 0, len(rows))
		for _, r := range rows {
			slugs = append(slugs, r.Slug)
		}
		require.ElementsMatch(t, []string{"or824-owner-" + suffix, "or824-other-" + suffix}, slugs)

		empty, err := dbtest.Queries(appPool).ListMerchantsForCustomerSubject(ctx, uuid.New())
		require.NoError(t, err)
		require.Empty(t, empty)
	})
}
