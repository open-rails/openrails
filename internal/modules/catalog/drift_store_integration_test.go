//go:build integration

package catalog

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestStandingCatalogFindingsAreAccountScopedAndCoverageBound(t *testing.T) {
	ctx := context.Background()
	admin := dbtest.SharedSuperuserPGXPool(t)
	database, err := db.NewDB(ctx, &config.DBConfig{URL: dbtest.SharedPostgresDSN(t)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	a, b := uuid.New(), uuid.New()
	stripe, nmiA, nmiB := uuid.New(), uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{a, b} {
		_, err := admin.Exec(ctx, `INSERT INTO billing.merchants(id,slug) VALUES($1,$2)`, id, "drift-"+id.String())
		require.NoError(t, err)
	}
	for id, rail := range map[uuid.UUID]string{stripe: "stripe", nmiA: "nmi", nmiB: "nmi"} {
		_, err := admin.Exec(ctx, `INSERT INTO billing.psps(id,merchant_id,rail,environment,account_id) VALUES($1,$2,$3,'test',$4)`, id, a, rail, "acct-"+id.String())
		require.NoError(t, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, `DELETE FROM billing.reconciliation_findings WHERE merchant_id = ANY($1)`, []uuid.UUID{a, b})
		_, _ = admin.Exec(ctx, `DELETE FROM billing.psps WHERE merchant_id = ANY($1)`, []uuid.UUID{a, b})
		_, _ = admin.Exec(ctx, `DELETE FROM billing.merchants WHERE id = ANY($1)`, []uuid.UUID{a, b})
	})
	mctx := merchant.WithID(ctx, merchant.ID(a))
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	priceOne, priceTwo := uuid.NewString(), uuid.NewString()
	stripeDrift := models.CatalogDriftEvent{PSPID: stripe, Provider: models.CatalogDriftProviderStripe, Kind: models.CatalogDriftFieldDrift,
		OpenRailsResourceType: models.CatalogDriftResourcePrice, OpenRailsResourceID: priceOne, ExternalResourceID: "price_1", Field: "active", OpenRailsValue: "true", ExternalValue: "false"}
	missing := func(psp uuid.UUID, priceID, plan string) models.CatalogDriftEvent {
		return models.CatalogDriftEvent{PSPID: psp, Provider: models.CatalogDriftProviderNMI, Kind: models.CatalogDriftMissingInNMI,
			OpenRailsResourceType: models.CatalogDriftResourcePrice, OpenRailsResourceID: priceID, ExternalResourceID: plan}
	}
	// The same local price and plan id on two NMI accounts are distinct identities.
	desired := []models.CatalogDriftEvent{stripeDrift, missing(nmiA, priceOne, "plan"), missing(nmiB, priceOne, "plan"), missing(nmiB, priceTwo, "plan-2")}
	added, resolved, err := PersistDrift(mctx, database, desired, nil, t0)
	require.NoError(t, err)
	require.Equal(t, 4, added)
	require.Zero(t, resolved, "no read means no absence proof")
	added, _, err = PersistDrift(mctx, database, desired, nil, t0)
	require.NoError(t, err)
	require.Zero(t, added, "re-observation is idempotent")

	open := func() map[uuid.UUID][]gen.OpenrailsCatalogDriftEvent {
		out := map[uuid.UUID][]gen.OpenrailsCatalogDriftEvent{}
		require.NoError(t, database.RunInMerchantConn(mctx, func(ctx context.Context) error {
			rows, err := database.Gen(ctx).ListOpenCatalogDriftEvents(ctx)
			for _, r := range rows {
				out[*r.PspID] = append(out[*r.PspID], r)
			}
			return err
		}))
		return out
	}
	require.Len(t, open()[nmiB], 2)
	stripeID := open()[stripe][0].ID

	// A complete read of NMI account A proves nothing about account B or Stripe.
	_, resolved, err = PersistDrift(mctx, database, nil, []DriftCoverage{{PSPID: nmiA}}, t0.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, resolved)
	require.Empty(t, open()[nmiA])
	require.Len(t, open()[nmiB], 2)
	require.Len(t, open()[stripe], 1)

	// A per-resource read proves only that resource.
	_, resolved, err = PersistDrift(mctx, database, nil, []DriftCoverage{{PSPID: nmiB, Resource: &DriftResource{Type: models.CatalogDriftResourcePrice, ID: priceTwo}}}, t0.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, resolved)
	require.Len(t, open()[nmiB], 1)

	// Newer evidence wins over a slower, older snapshot in both directions.
	stripeDrift.ExternalValue = "unknown"
	_, _, err = PersistDrift(mctx, database, []models.CatalogDriftEvent{stripeDrift}, nil, t0.Add(3*time.Second))
	require.NoError(t, err)
	stale := stripeDrift
	stale.ExternalValue = "stale"
	_, resolved, err = PersistDrift(mctx, database, []models.CatalogDriftEvent{stale}, nil, t0.Add(2*time.Second))
	require.NoError(t, err)
	require.Zero(t, resolved)
	_, resolved, err = PersistDrift(mctx, database, nil, []DriftCoverage{{PSPID: stripe}}, t0.Add(2*time.Second))
	require.NoError(t, err)
	require.Zero(t, resolved)
	require.Equal(t, "unknown", *open()[stripe][0].ExternalValue)

	// An operator-ignored identity stays ignored and is not reported as new drift.
	_, err = admin.Exec(ctx, `UPDATE billing.reconciliation_findings SET status='ignored', resolution='ignored', resolved_at=now() WHERE id=$1`, stripeID)
	require.NoError(t, err)
	stripeDrift.ExternalValue = "changed"
	added, resolved, err = PersistDrift(mctx, database, []models.CatalogDriftEvent{stripeDrift}, []DriftCoverage{{PSPID: stripe}}, t0.Add(4*time.Second))
	require.NoError(t, err)
	require.Zero(t, added)
	require.Zero(t, resolved)
	var status, external string
	require.NoError(t, admin.QueryRow(ctx, `SELECT status, external_value FROM billing.reconciliation_findings WHERE id=$1`, stripeID).Scan(&status, &external))
	require.Equal(t, "ignored", status)
	require.Equal(t, "unknown", external)

	// A resolved identity that reappears reopens the same standing finding and counts once.
	added, _, err = PersistDrift(mctx, database, []models.CatalogDriftEvent{missing(nmiA, priceOne, "plan")}, []DriftCoverage{{PSPID: nmiA}}, t0.Add(5*time.Second))
	require.NoError(t, err)
	require.Equal(t, 1, added)
	require.Len(t, open()[nmiA], 1)
	var findings int
	require.NoError(t, admin.QueryRow(ctx, `SELECT count(*) FROM billing.reconciliation_findings WHERE psp_id=$1`, nmiA).Scan(&findings))
	require.Equal(t, 1, findings)

	// Kind, rail and account identity must agree.
	wrongRail := missing(stripe, priceOne, "plan")
	_, _, err = PersistDrift(mctx, database, []models.CatalogDriftEvent{wrongRail}, nil, t0.Add(6*time.Second))
	require.Error(t, err, "an NMI finding cannot name a Stripe account")
	wrongRail.Provider = models.CatalogDriftProviderStripe
	_, _, err = PersistDrift(mctx, database, []models.CatalogDriftEvent{wrongRail}, nil, t0.Add(6*time.Second))
	require.Error(t, err, "missing_in_nmi cannot be recorded on the stripe rail")
	_, _, err = PersistDrift(mctx, database, []models.CatalogDriftEvent{{Provider: models.CatalogDriftProviderNMI, Kind: models.CatalogDriftOrphanInNMI,
		OpenRailsResourceType: models.CatalogDriftResourcePrice, ExternalResourceID: "x"}}, nil, t0)
	require.Error(t, err, "a finding without account identity is refused")

	// RLS: another merchant sees and resolves nothing.
	bctx := merchant.WithID(ctx, merchant.ID(b))
	_, resolved, err = PersistDrift(bctx, database, nil, []DriftCoverage{{PSPID: nmiB}, {PSPID: stripe}}, t0.Add(time.Hour))
	require.NoError(t, err)
	require.Zero(t, resolved)
	require.NoError(t, database.RunInMerchantConn(bctx, func(ctx context.Context) error {
		rows, err := database.Gen(ctx).ListOpenCatalogDriftEvents(ctx)
		require.Empty(t, rows)
		return err
	}))
	require.Len(t, open()[nmiB], 1)
}
