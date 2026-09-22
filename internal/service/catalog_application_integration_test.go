//go:build integration

package service

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/catalogpolicy"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	catalogdecl "github.com/open-rails/openrails/pkg/catalog"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/pricing"
	"github.com/stretchr/testify/require"
)

func applicationService(t *testing.T) (*Service, context.Context) {
	t.Helper()
	pool := dbtest.SharedSuperuserPGXPool(t)
	mid := merchant.ID(uuid.New())
	ctx := merchant.WithID(t.Context(), mid)
	_, err := pool.Exec(ctx, "INSERT INTO billing.merchants(id,slug) VALUES($1,$2)", mid.UUID(), "apply-"+mid.String())
	require.NoError(t, err)
	database := dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t))
	return &Service{rt: &app.Runtime{DB: database, Config: &config.Config{AllowCatalogUpdates: true, NewSubscriptionCollectionPolicy: "engine"}, ProductService: catalog.NewProductService(database), PriceService: catalog.NewPriceService(database), MoneyService: money.NewMoneyService(database)}}, ctx
}
func applicationParams(t *testing.T, s *Service, ctx context.Context) openrails.CatalogApplyParams {
	t.Helper()
	revision, err := s.CatalogRevision(ctx)
	require.NoError(t, err)
	return openrails.CatalogApplyParams{SchemaVersion: 1, ApplicationID: uuid.NewString(), ExpectedRevision: &revision}
}
func applicationProduct(key string) openrails.CatalogApplyProduct {
	return openrails.CatalogApplyProduct{Key: key, DisplayName: openrails.CatalogValue(key), Prices: []openrails.CatalogApplyPrice{{Key: key + "-price", Currency: openrails.CatalogValue("USD"), UnitAmount: openrails.CatalogValue(int64(100))}}}
}

func TestCatalogApplicationReplayAndCAS(t *testing.T) {
	s, ctx := applicationService(t)
	a := applicationParams(t, s, ctx)
	a.Products = []openrails.CatalogApplyProduct{applicationProduct("first")}
	first, err := s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	require.EqualValues(t, 1, first.AppliedRevision)
	p, err := s.GetProductByKey(ctx, "first")
	require.NoError(t, err)
	edited := "API edit"
	_, err = s.UpdateProduct(ctx, p.ID, UpdateProductRequest{DisplayName: &edited})
	require.NoError(t, err)
	rev, err := s.CatalogRevision(ctx)
	require.NoError(t, err)
	require.Greater(t, rev, first.AppliedRevision)
	replay, err := s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, first.AppliedRevision, replay.AppliedRevision)
	p, err = s.GetProductByKey(ctx, "first")
	require.NoError(t, err)
	require.Equal(t, edited, p.DisplayName)
	after, err := s.CatalogRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, rev, after)
	changed := a
	changed.Prune = true
	_, err = s.ApplyCatalog(ctx, changed)
	require.ErrorContains(t, err, "different content")
	stale := a
	stale.ApplicationID = uuid.NewString()
	_, err = s.ApplyCatalog(ctx, stale)
	require.ErrorContains(t, err, "revision")
	reapplied := a
	reapplied.ApplicationID = uuid.NewString()
	reapplied.ExpectedRevision = &rev
	_, err = s.ApplyCatalog(ctx, reapplied)
	require.NoError(t, err)
	p, err = s.GetProductByKey(ctx, "first")
	require.NoError(t, err)
	require.Equal(t, "first", p.DisplayName)
	// A replay is still subject to the current ordinary-write policy.
	s.rt.Config.AllowCatalogUpdates = false
	_, err = s.ApplyCatalog(ctx, a)
	require.ErrorIs(t, err, catalogpolicy.ErrUpdatesDisabled)
	replay, err = s.ApplyCatalog(catalogpolicy.OperatorContext(ctx), a)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
}

func TestCatalogApplicationConcurrentIdentityAndRevision(t *testing.T) {
	s, ctx := applicationService(t)
	a := applicationParams(t, s, ctx)
	a.Products = []openrails.CatalogApplyProduct{applicationProduct("concurrent")}
	var wg sync.WaitGroup
	receipts := make([]*openrails.CatalogApplicationReceipt, 8)
	errs := make([]error, 8)
	for i := range receipts {
		wg.Add(1)
		go func(i int) { defer wg.Done(); receipts[i], errs[i] = s.ApplyCatalog(ctx, a) }(i)
	}
	wg.Wait()
	committed := 0
	for i, r := range receipts {
		require.NoError(t, errs[i])
		if !r.Replayed {
			committed++
		}
	}
	require.Equal(t, 1, committed)
	rev, err := s.CatalogRevision(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, rev)
	a = applicationParams(t, s, ctx)
	b := a
	b.ApplicationID = uuid.NewString()
	wg.Add(2)
	go func() { defer wg.Done(); _, errs[0] = s.ApplyCatalog(ctx, a) }()
	go func() { defer wg.Done(); _, errs[1] = s.ApplyCatalog(ctx, b) }()
	wg.Wait()
	require.True(t, (errs[0] == nil) != (errs[1] == nil), "exactly one new identity commits the pinned revision")
}

func TestCatalogApplicationRollbackAndPriceHistory(t *testing.T) {
	s, ctx := applicationService(t)
	a := applicationParams(t, s, ctx)
	good := applicationProduct("retained")
	bad := applicationProduct("bad")
	bad.Prices[0].AutoRenew = openrails.CatalogValue(true)
	a.Products = []openrails.CatalogApplyProduct{good, bad}
	_, err := s.ApplyCatalog(ctx, a)
	require.Error(t, err)
	_, err = s.GetProductByKey(ctx, "retained")
	require.True(t, errors.Is(err, openrails.ErrNotFound))
	rev, err := s.CatalogRevision(ctx)
	require.NoError(t, err)
	require.Zero(t, rev)
	// Failed attempts consume neither ID nor revision.
	a.Products = []openrails.CatalogApplyProduct{good}
	_, err = s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	old, err := s.GetPriceByKey(ctx, "retained-price")
	require.NoError(t, err)
	update := applicationParams(t, s, ctx)
	update.Products = []openrails.CatalogApplyProduct{{Key: "retained", Prices: []openrails.CatalogApplyPrice{{Key: "retained-price", UnitAmount: openrails.CatalogValue(int64(200))}}}}
	_, err = s.ApplyCatalog(ctx, update)
	require.NoError(t, err)
	next, err := s.GetPriceByKey(ctx, "retained-price")
	require.NoError(t, err)
	require.NotEqual(t, old.ID, next.ID)
	require.Equal(t, "USD", next.Currency)
	archive := applicationParams(t, s, ctx)
	archive.Products = []openrails.CatalogApplyProduct{{Key: "retained", Prices: []openrails.CatalogApplyPrice{{Key: "retained-price", Archived: openrails.CatalogValue(true)}}}}
	_, err = s.ApplyCatalog(ctx, archive)
	require.NoError(t, err)
	_, err = s.GetPriceByKey(ctx, "retained-price")
	require.ErrorIs(t, err, openrails.ErrNotFound)
	history, err := s.GetPriceKeyHistory(ctx, "retained-price")
	require.NoError(t, err)
	require.True(t, history[0].Archived)
	revive := applicationParams(t, s, ctx)
	revive.Products = []openrails.CatalogApplyProduct{{Key: "retained", Prices: []openrails.CatalogApplyPrice{{Key: "retained-price", ID: old.ID.String(), Archived: openrails.CatalogValue(false)}}}}
	_, err = s.ApplyCatalog(ctx, revive)
	require.NoError(t, err)
	now, err := s.GetPriceByKey(ctx, "retained-price")
	require.NoError(t, err)
	require.Equal(t, old.ID, now.ID)
	history, err = s.GetPriceKeyHistory(ctx, "retained-price")
	require.NoError(t, err)
	require.False(t, history[0].Archived)
	require.Len(t, history, 5)
}

func TestCatalogApplicationOmissionAndScopedPrune(t *testing.T) {
	s, ctx := applicationService(t)
	a := applicationParams(t, s, ctx)
	a.Products = []openrails.CatalogApplyProduct{applicationProduct("keep"), applicationProduct("omit")}
	_, err := s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	subject := "other-owner"
	other, err := catalog.NewCatalogRepo(s.rt.DB).Ensure(ctx, &subject)
	require.NoError(t, err)
	foreign, err := s.CreateProduct(ctx, CreateProductRequest{CatalogID: openrails.CatalogID(other.ID), Key: "foreign", DisplayName: "foreign"})
	require.NoError(t, err)
	patch := applicationParams(t, s, ctx)
	patch.Products = []openrails.CatalogApplyProduct{{Key: "keep", Description: openrails.CatalogValue("changed")}}
	_, err = s.ApplyCatalog(ctx, patch)
	require.NoError(t, err)
	_, err = s.GetPriceByKey(ctx, "keep-price")
	require.NoError(t, err)
	omitted, err := s.GetProductByKey(ctx, "omit")
	require.NoError(t, err)
	require.False(t, omitted.Archived)
	prune := applicationParams(t, s, ctx)
	prune.Prune = true
	prune.Products = []openrails.CatalogApplyProduct{{Key: "keep"}}
	_, err = s.ApplyCatalog(ctx, prune)
	require.NoError(t, err)
	_, err = s.GetPriceByKey(ctx, "keep-price")
	require.ErrorIs(t, err, openrails.ErrNotFound)
	omitted, err = s.GetProductByKey(ctx, "omit")
	require.NoError(t, err)
	require.True(t, omitted.Archived)
	preserved, err := s.GetProduct(ctx, foreign.ID)
	require.NoError(t, err)
	require.False(t, preserved.Archived)
	// The archive state is not reset by an omitted lifecycle field.
	noReactivate := applicationParams(t, s, ctx)
	noReactivate.Products = []openrails.CatalogApplyProduct{{Key: "omit", Description: openrails.CatalogValue("")}}
	_, err = s.ApplyCatalog(ctx, noReactivate)
	require.NoError(t, err)
	omitted, err = s.GetProductByKey(ctx, "omit")
	require.NoError(t, err)
	require.True(t, omitted.Archived)
}

func TestCatalogApplicationRetiredCreationAndLastWriteRollback(t *testing.T) {
	s, ctx := applicationService(t)
	unknown := applicationParams(t, s, ctx)
	unknown.Products = []openrails.CatalogApplyProduct{{Key: "missing", Archived: openrails.CatalogValue(true)}}
	_, err := s.ApplyCatalog(ctx, unknown)
	require.ErrorContains(t, err, "unknown product")
	historical := applicationParams(t, s, ctx)
	p := applicationProduct("historical")
	p.Archived = openrails.CatalogValue(true)
	p.Prices[0].Archived = openrails.CatalogValue(true)
	historical.Products = []openrails.CatalogApplyProduct{p}
	_, err = s.ApplyCatalog(ctx, historical)
	require.NoError(t, err)
	product, err := s.GetProductByKey(ctx, "historical")
	require.NoError(t, err)
	require.True(t, product.Archived)
	prices, err := s.ListPricesByProduct(ctx, product.ID, false)
	require.NoError(t, err)
	require.Len(t, prices, 1)
	require.True(t, prices[0].Archived)
	// Fail the LAST durable receipt write after product, price and history writes.
	// Scope the injected trigger to this merchant so concurrent fixtures are safe.
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	owner := dbtest.SharedSuperuserPGXPool(t)
	_, err = owner.Exec(ctx, `CREATE OR REPLACE FUNCTION billing.reject_application_fixture() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.application_id LIKE 'reject-fixture-%' THEN RAISE EXCEPTION 'receipt fixture rejection'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER reject_application_fixture BEFORE INSERT ON billing.catalog_applications FOR EACH ROW EXECUTE FUNCTION billing.reject_application_fixture();`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = owner.Exec(context.Background(), "DROP TRIGGER reject_application_fixture ON billing.catalog_applications; DROP FUNCTION billing.reject_application_fixture();")
	})
	rejected := applicationParams(t, s, ctx)
	before := *rejected.ExpectedRevision
	rejected.ApplicationID = "reject-fixture-" + mid.String()
	rejected.Products = []openrails.CatalogApplyProduct{applicationProduct("must-rollback")}
	_, err = s.ApplyCatalog(ctx, rejected)
	require.ErrorContains(t, err, "receipt fixture rejection")
	_, err = s.GetProductByKey(ctx, "must-rollback")
	require.ErrorIs(t, err, openrails.ErrNotFound)
	revision, err := s.CatalogRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, before, revision)
	var movements int
	err = owner.QueryRow(ctx, "SELECT count(*) FROM billing.price_key_movements WHERE merchant_id=$1 AND key='must-rollback-price'", mid.UUID()).Scan(&movements)
	require.NoError(t, err)
	require.Zero(t, movements)
}

func TestCatalogApplicationMeterPresenceAndDependencyRollback(t *testing.T) {
	s, ctx := applicationService(t)
	a := applicationParams(t, s, ctx)
	a.Meters = []openrails.CatalogApplyMeter{{Key: "requests", EventType: openrails.CatalogValue("request"), Aggregation: openrails.CatalogValue("count"), Unit: openrails.CatalogValue("call")}}
	_, err := s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	patch := applicationParams(t, s, ctx)
	patch.Meters = []openrails.CatalogApplyMeter{{Key: "requests", Unit: openrails.CatalogValue("invocation")}}
	_, err = s.ApplyCatalog(ctx, patch)
	require.NoError(t, err)
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	owner := dbtest.SharedSuperuserPGXPool(t)
	var event, aggregation, unit string
	err = owner.QueryRow(ctx, "SELECT event_type,aggregation,unit FROM billing.catalog_meters WHERE merchant_id=$1 AND key='requests'", mid.UUID()).Scan(&event, &aggregation, &unit)
	require.NoError(t, err)
	require.Equal(t, "request", event)
	require.Equal(t, "count", aggregation)
	require.Equal(t, "invocation", unit)
	bad := applicationParams(t, s, ctx)
	bad.Products = []openrails.CatalogApplyProduct{applicationProduct("rolled-back-meter")}
	bad.Meters = []openrails.CatalogApplyMeter{{Key: "invalid", Aggregation: openrails.CatalogValue("not-an-aggregation")}}
	_, err = s.ApplyCatalog(ctx, bad)
	require.Error(t, err)
	_, err = s.GetProductByKey(ctx, "rolled-back-meter")
	require.ErrorIs(t, err, openrails.ErrNotFound)
}

func TestCatalogApplicationPruneEnumeratesEveryPage(t *testing.T) {
	s, ctx := applicationService(t)
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	// Seed through the runtime role, exercising the raw writer revision backstop.
	_, err = s.rt.DB.Qx(ctx).Exec(ctx, "INSERT INTO billing.products(merchant_id,key,display_name) SELECT $1,'page-'||n,'Product '||n FROM generate_series(1,1005) n", mid.UUID())
	require.NoError(t, err)
	a := applicationParams(t, s, ctx)
	a.Prune = true
	receipt, err := s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	require.Equal(t, 1005, receipt.ProductsChanged)
	page, err := s.ListProducts(ctx, ListProductsOptions{Archived: func() *bool { v := false; return &v }()})
	require.NoError(t, err)
	require.Zero(t, page.Total)
}

func TestCatalogApplicationRejectsProviderIOAndPreservesVerifiedLinks(t *testing.T) {
	s, ctx := applicationService(t)
	unsupported := applicationParams(t, s, ctx)
	p := applicationProduct("external")
	p.Prices[0].PSPs = openrails.CatalogValue([]string{"solana"})
	unsupported.Products = []openrails.CatalogApplyProduct{p}
	_, err := s.ApplyCatalog(ctx, unsupported)
	require.ErrorContains(t, err, "matching merchant account")
	_, err = s.GetProductByKey(ctx, "external")
	require.ErrorIs(t, err, openrails.ErrNotFound)
	native := applicationParams(t, s, ctx)
	p = applicationProduct("native")
	p.Prices[0].PSPs = openrails.CatalogValue([]string{"stripe"})
	native.Products = []openrails.CatalogApplyProduct{p}
	_, err = s.ApplyCatalog(ctx, native)
	require.NoError(t, err)
	again := native
	again.ApplicationID = uuid.NewString()
	revision, err := s.CatalogRevision(ctx)
	require.NoError(t, err)
	again.ExpectedRevision = &revision
	_, err = s.ApplyCatalog(ctx, again)
	require.NoError(t, err, "native PSP selection has no external binding to require on reapplication")
	// A historically linked price can restate its verified binding without network
	// validation or recreation; changing the link is refused transactionally.
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	owner := dbtest.SharedSuperuserPGXPool(t)
	psp := uuid.New()
	_, err = owner.Exec(ctx, "INSERT INTO billing.psps(merchant_id,id,key,rail,environment,account_id) VALUES($1,$2,'historical-nmi','nmi','live','fixture-account')", mid.UUID(), psp)
	require.NoError(t, err)
	price, err := s.GetPriceByKey(ctx, "native-price")
	require.NoError(t, err)
	_, err = owner.Exec(ctx, "INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,plan_id,configuration) VALUES($1,$2,$3,'verified-plan','{}')", mid.UUID(), price.ID.UUID(), psp)
	require.NoError(t, err)
	retain := applicationParams(t, s, ctx)
	retain.Products = []openrails.CatalogApplyProduct{{Key: "native", Prices: []openrails.CatalogApplyPrice{{Key: "native-price", PSPs: openrails.CatalogValue([]string{"historical-nmi"}), PSPLinks: openrails.CatalogValue(map[string]map[string]string{"historical-nmi": {"plan_id": "verified-plan"}})}}}}
	_, err = s.ApplyCatalog(ctx, retain)
	require.NoError(t, err)
	rotated := applicationParams(t, s, ctx)
	rotated.Products = []openrails.CatalogApplyProduct{{Key: "native", Description: openrails.CatalogValue("must roll back"), Prices: []openrails.CatalogApplyPrice{{Key: "native-price", PSPLinks: openrails.CatalogValue(map[string]map[string]string{"historical-nmi": {"plan_id": "unverified"}})}}}}
	_, err = s.ApplyCatalog(ctx, rotated)
	require.Error(t, err, "an unverified rotated provider reference cannot commit")
	product, err := s.GetProductByKey(ctx, "native")
	require.NoError(t, err)
	require.Empty(t, product.Description)
	empty := applicationParams(t, s, ctx)
	empty.Products = []openrails.CatalogApplyProduct{{Key: "native", Description: openrails.CatalogValue("must also roll back"), Prices: []openrails.CatalogApplyPrice{{Key: "native-price", PSPLinks: openrails.CatalogValue(map[string]map[string]string{"historical-nmi": {}})}}}}
	_, err = s.ApplyCatalog(ctx, empty)
	require.ErrorContains(t, err, "removal")
	product, err = s.GetProductByKey(ctx, "native")
	require.NoError(t, err)
	require.Empty(t, product.Description)
}

func TestCatalogApplicationRateCardOrdinalsRemainStable(t *testing.T) {
	s, ctx := applicationService(t)
	a := applicationParams(t, s, ctx)
	cards := []catalogdecl.RateCard{{Ordinal: 1, Price: pricing.RatePrice{Model: "flat", Currency: "USD", Flat: &pricing.FlatPrice{Amount: 100}}}, {Ordinal: 3, Price: pricing.RatePrice{Model: "flat", Currency: "USD", Flat: &pricing.FlatPrice{Amount: 300}}}}
	a.Products = []openrails.CatalogApplyProduct{{Key: "ordinal", DisplayName: openrails.CatalogValue("Ordinal"), RateCards: openrails.CatalogValue(cards)}}
	_, err := s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	mid, err := merchant.Require(ctx)
	require.NoError(t, err)
	owner := dbtest.SharedSuperuserPGXPool(t)
	read := func() []string {
		rows, e := owner.Query(ctx, "SELECT id::text||':'||ordinal::text||':'||created_at::text FROM billing.catalog_rate_cards WHERE merchant_id=$1 ORDER BY ordinal", mid.UUID())
		require.NoError(t, e)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var value string
			require.NoError(t, rows.Scan(&value))
			out = append(out, value)
		}
		require.NoError(t, rows.Err())
		return out
	}
	before := read()
	require.Len(t, before, 2)
	revision, err := s.CatalogRevision(ctx)
	require.NoError(t, err)
	a.ApplicationID = uuid.NewString()
	a.ExpectedRevision = &revision
	_, err = s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	require.Equal(t, before, read(), "unchanged declared ordinals cannot churn row identity or creation time")
	bad := applicationParams(t, s, ctx)
	duplicate := append([]catalogdecl.RateCard(nil), cards...)
	duplicate[1].Ordinal = 1
	bad.Products = []openrails.CatalogApplyProduct{{Key: "ordinal", RateCards: openrails.CatalogValue(duplicate)}}
	_, err = s.ApplyCatalog(ctx, bad)
	require.Error(t, err)
	require.Equal(t, before, read())
}

func TestCatalogApplicationNormalizationDoesNotChangeRetryPayload(t *testing.T) {
	s, ctx := applicationService(t)
	a := applicationParams(t, s, ctx)
	a.Meters = []openrails.CatalogApplyMeter{{Key: "filter-meter", EventType: openrails.CatalogValue("request"), Aggregation: openrails.CatalogValue("count"), GroupBy: openrails.CatalogValue(map[string]string{"region": "$.region"})}}
	filter := map[string][]string{"region": {"west", "east"}}
	cards := []catalogdecl.RateCard{{Meter: "filter-meter", PaymentTerm: "in_arrears", Filter: filter, Price: pricing.RatePrice{Model: "per_unit", Currency: "USD", PerUnit: &pricing.PerUnitPrice{UnitAmount: 100}}}}
	a.Products = []openrails.CatalogApplyProduct{{Key: "filter-product", DisplayName: openrails.CatalogValue("Filter product"), RateCards: openrails.CatalogValue(cards)}}
	beforeJSON, err := json.Marshal(a)
	require.NoError(t, err)
	digest, err := a.CanonicalDigest()
	require.NoError(t, err)
	_, err = s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	require.Equal(t, []string{"west", "east"}, filter["region"])
	after, err := a.CanonicalDigest()
	require.NoError(t, err)
	require.Equal(t, digest, after)
	afterJSON, err := json.Marshal(a)
	require.NoError(t, err)
	require.Equal(t, string(beforeJSON), string(afterJSON))
	require.Zero(t, cards[0].Price.PerUnit.DivideBy)
	require.Empty(t, cards[0].Price.PerUnit.Round)
	replay, err := s.ApplyCatalog(ctx, a)
	require.NoError(t, err)
	require.True(t, replay.Replayed)
}
