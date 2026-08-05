//go:build integration

package service_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#900 item 1: the plan-migration facade under the or#885-mandated
// openrails_app role, called the way an embedding host calls it — a merchant on
// the context and NOTHING pinned upstream (no HTTP middleware, no
// embedded.RunInMerchant).
//
// Before the fix PreviewPlanMigration/PlanMigrate read on the base pool, where
// `app.merchant_id` is unset, so every RLS-forced read matched merchant_id =
// NULL. The price the SAME facade had just written was invisible and Preview
// answered "plan migration: source price: no rows in result set". This test
// fails that way without the RunInMerchantConn wrap in plan_migration.go.
func TestPlanMigrationFacade_RLS_Under_OpenRailsApp(t *testing.T) {
	ctx := context.Background()
	superDSN, appRoleDSN := dbtest.SharedRLSPostgres(t)

	merchantID := uuid.NewString()
	super, err := db.NewDB(&config.DBConfig{URL: superDSN})
	require.NoError(t, err)
	defer super.Close()
	_, err = super.Pool().Exec(ctx,
		`INSERT INTO openrails.merchants (id, slug) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`,
		merchantID, "merchant-pm-"+merchantID[:8])
	require.NoError(t, err)

	appDB, err := db.NewDB(&config.DBConfig{URL: appRoleDSN})
	require.NoError(t, err)
	defer appDB.Close()
	posture, err := appDB.CheckRLSPosture(ctx)
	require.NoError(t, err)
	require.True(t, posture.Enforcing, "the whole point is the role RLS constrains")

	clock := clockwork.NewRealClock()
	products := catalog.NewProductService(appDB)
	prices := catalog.NewPriceService(appDB)
	subs := subscriptions.NewSubscriptionService(appDB, prices, products, nil, clock)
	reprice := subscriptions.NewRepriceService(
		appDB,
		subscriptions.NewRepriceRepo(appDB),
		prices,
		subs,
		subscriptions.NewNotificationService(appDB, nil),
		merchantconfig.NewStore(appDB),
		clock,
	)
	rt := &app.Runtime{
		DB:                   appDB,
		MoneyService:         money.NewMoneyService(appDB),
		EntitlementService:   entitlements.NewEntitlementService(appDB),
		ProductService:       products,
		PriceService:         prices,
		SubscriptionService:  subs,
		PlanMigrationService: subscriptions.NewPlanMigrationService(reprice, nil, nil, nil),
		Clock:                clock,
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	// A merchant on the context and nothing else — exactly what a host has.
	mctx := merchant.WithID(ctx, mustMerchantID(t, merchantID))

	sourceProduct, err := svc.CreateProduct(mctx, billingservice.CreateProductRequest{
		Key: "or900-source", DisplayName: "or900 source",
	})
	require.NoError(t, err, "CreateProduct must work under openrails_app")
	targetProduct, err := svc.CreateProduct(mctx, billingservice.CreateProductRequest{
		Key: "or900-target", DisplayName: "or900 target",
	})
	require.NoError(t, err)

	hours := 720
	sourcePrice, err := svc.CreatePrice(mctx, billingservice.CreatePriceRequest{
		ProductID: sourceProduct.ID, UnitAmount: 10_000_000, Currency: "usd",
		AccessDurationHours: &hours, AutoRenew: true,
	})
	require.NoError(t, err, "CreatePrice must work under openrails_app")
	targetPrice, err := svc.CreatePrice(mctx, billingservice.CreatePriceRequest{
		ProductID: targetProduct.ID, UnitAmount: 12_000_000, Currency: "usd",
		AccessDurationHours: &hours, AutoRenew: true,
	})
	require.NoError(t, err)

	// The whole defect in one call: a preview of a price this facade wrote a
	// moment ago, on the same role, with no upstream pin.
	res, err := svc.PreviewPlanMigration(mctx, billingservice.PlanMigrationRequest{
		SourcePriceID: sourcePrice.ID,
		TargetPriceID: targetPrice.ID,
	})
	require.NoError(t, err, "PreviewPlanMigration must SEE the price it just wrote (or#900)")
	require.NotNil(t, res)
	require.Equal(t, sourcePrice.ID, res.SourcePriceID)
	require.Equal(t, targetPrice.ID, res.TargetPriceID)
	require.Equal(t, 0, res.Matched, "no subscriptions on the source price yet")

	// Commit the same migration: the write path resolves the same rows.
	archive := false
	committed, err := svc.PlanMigrate(mctx, billingservice.PlanMigrationRequest{
		SourcePriceID: sourcePrice.ID,
		TargetPriceID: targetPrice.ID,
		ArchiveSource: &archive,
	})
	require.NoError(t, err, "PlanMigrate must resolve the same prices under openrails_app")
	require.NotNil(t, committed)
	require.NotNil(t, committed.BatchID)
	require.NotEqual(t, uuid.Nil, *committed.BatchID)

	// And the batch is readable back through the facade, still unpinned.
	batch, rows, err := svc.GetPlanMigration(mctx, *committed.BatchID, 50, 0)
	require.NoError(t, err, "GetPlanMigration must find the batch it just wrote")
	require.NotNil(t, batch)
	require.Empty(t, rows, "empty cohort means no per-subscription ledger rows")
}

// TestFacadeRefusesWithoutAMerchant proves the OTHER half of the fix: with no
// merchant on the context the facade now FAILS instead of answering an empty
// result. Silence was the bug — a host could not tell "nothing matched" from
// "you read the wrong connection".
func TestFacadeRefusesWithoutAMerchant(t *testing.T) {
	ctx := context.Background()
	_, appRoleDSN := dbtest.SharedRLSPostgres(t)

	appDB, err := db.NewDB(&config.DBConfig{URL: appRoleDSN})
	require.NoError(t, err)
	defer appDB.Close()

	rt := &app.Runtime{
		DB:                 appDB,
		MoneyService:       money.NewMoneyService(appDB),
		EntitlementService: entitlements.NewEntitlementService(appDB),
		ProductService:     catalog.NewProductService(appDB),
		PriceService:       catalog.NewPriceService(appDB),
		Clock:              clockwork.NewRealClock(),
	}
	svc, err := billingservice.New(rt)
	require.NoError(t, err)

	_, err = svc.GetProduct(ctx, uuid.New())
	require.Error(t, err, "an unscoped facade read must fail loudly, not return nothing")
	require.Contains(t, err.Error(), "merchant")
}
