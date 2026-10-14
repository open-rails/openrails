//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/nmimock"
)

// A key identifies a product-local chain; equal money under another key is a
// separate price. Reusing financial terms preserves the original immutable ID/version.
func TestCatalogProductScopedPriceVersions(t *testing.T) {
	w := newWorld(t)
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			c := w.client[tp]
			products := make([]*billing.Product, 2)
			original := make([]*billing.Price, 2)
			create := func(product *billing.Product, key string, amount int64) *billing.Price {
				t.Helper()
				price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, Currency: "USD", UnitAmount: amount})
				require.NoError(t, err)
				return price
			}
			for i := range products {
				product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: fmt.Sprintf("local-%s-%d", tp, i), DisplayName: "Local keys"})
				require.NoError(t, err)
				products[i] = product
				original[i] = create(product, "monthly", 1_000_000)
				require.EqualValues(t, 1, original[i].Version)
				alias := create(product, "alternate-price", 1_000_000)
				require.NotEqual(t, original[i].ID, alias.ID, "equal financial terms cannot steal another key")
				require.EqualValues(t, 1, alias.Version)
				repeat := create(product, "monthly", 1_000_000)
				require.Equal(t, original[i].ID, repeat.ID)
				require.Equal(t, original[i].Version, repeat.Version)
			}
			next := create(products[0], "monthly", 2_000_000)
			require.EqualValues(t, 2, next.Version)
			require.NotEqual(t, original[0].ID, next.ID)
			prior, err := c.GetPrice(t.Context(), original[0].ID, billing.GetPriceParams{})
			require.NoError(t, err)
			require.True(t, prior.Archived)
			other, err := priceByKey(t.Context(), c, products[1].Key, "monthly")
			require.NoError(t, err)
			require.Equal(t, original[1].ID, other.ID)
			restored := create(products[0], "monthly", 1_000_000)
			require.Equal(t, original[0].ID, restored.ID)
			require.EqualValues(t, 1, restored.Version)
			require.False(t, restored.Archived)
			third := create(products[0], "monthly", 3_000_000)
			require.EqualValues(t, 3, third.Version, "reactivating v1 does not reset version allocation")
			for i, product := range products {
				history, err := priceKeyHistory(t.Context(), c, product.Key, "monthly", billing.PageRequest{})
				require.NoError(t, err)
				require.NotEmpty(t, history.Items)
				for _, movement := range history.Items {
					require.Equal(t, products[i].ID, movement.Price.ProductID)
				}
			}
			// Existing catalogs and imports can carry pre-revision UUIDs. The
			// natural-key lookup must preserve them rather than mint a duplicate.
			legacyID := uuid.New()
			err = w.catalogExec(t.Context(), w.sql(`INSERT INTO billing.prices
				(id, merchant_id, product_id, key, amount, currency, archived, billing_interval_hours)
				SELECT $1, merchant_id, id, 'imported', 4000000, 'USD', false, NULL
				FROM billing.products WHERE id=$2`), legacyID, products[0].ID.UUID())
			require.NoError(t, err)
			imported := create(products[0], "imported", 4_000_000)
			require.Equal(t, legacyID, w.priceStorageID(imported.ID))
			require.Equal(t, products[0].Key+":imported:v1", imported.ID.String())
			require.EqualValues(t, 1, imported.Version)
		})
	}
}

func TestCatalogProductRevisionTracksMutableFields(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "mutable", DisplayName: "Before", Entitlements: []string{"old-benefit"}})
	require.NoError(t, err)
	require.EqualValues(t, 1, product.Revision)
	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	patch := billing.UpdateProductParams{DisplayName: catalog.Value("After"), Entitlements: catalog.Value([]string{"new-benefit"})}
	updated, err := c.UpdateProduct(t.Context(), product.ID, patch)
	require.NoError(t, err)
	require.Equal(t, product.ID, updated.ID)
	require.EqualValues(t, 2, updated.Revision)
	repeated, err := c.UpdateProduct(t.Context(), product.ID, patch)
	require.NoError(t, err)
	require.Equal(t, updated.Revision, repeated.Revision, "an unchanged product patch does not manufacture a revision")
	current, err := priceByKey(t.Context(), c, product.Key, "purchase")
	require.NoError(t, err)
	require.Equal(t, price.ID, current.ID, "product metadata changes cannot replace its immutable prices")
	require.Equal(t, price.Revision, current.Revision, "a product change never moves its prices")
	archived, err := c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
	require.NoError(t, err)
	require.EqualValues(t, 3, archived.Revision)
	restored, err := c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(false)})
	require.NoError(t, err)
	require.Equal(t, product.ID, restored.ID)
	require.EqualValues(t, 4, restored.Revision)
}

func TestCatalogConcurrentPriceVersions(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "concurrent", DisplayName: "Concurrent"})
	require.NoError(t, err)
	// Each round races three writers of equal terms; each later round moves the
	// key to new terms once. Writers racing different terms keep moving the
	// catalog under each other's edits, which then answer 409.
	const rounds, writers = 3, 3
	for round := range rounds {
		results := make([]*billing.Price, writers)
		errors := make([]error, writers)
		start := make(chan struct{})
		var running sync.WaitGroup
		for i := range writers {
			running.Go(func() {
				<-start
				results[i], errors[i] = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", UnitAmount: int64(round+1) * 1_000_000, Currency: "USD"})
			})
		}
		close(start)
		running.Wait()
		for i, err := range errors {
			require.NoError(t, err, "round %d writer %d", round, i)
			require.Equal(t, results[0].ID, results[i].ID, "repeated terms keep one immutable ID")
			require.Equal(t, results[0].Version, results[i].Version)
		}
	}
	prices, err := c.ListPrices(t.Context(), billing.PriceListParams{ProductID: product.ID})
	require.NoError(t, err)
	require.Len(t, prices.Items, 3)
	revisions, live := map[int64]bool{}, 0
	for _, price := range prices.Items {
		revisions[price.Version] = true
		if !price.Archived {
			live++
		}
	}
	require.Equal(t, map[int64]bool{1: true, 2: true, 3: true}, revisions)
	require.Equal(t, 1, live)
}

func TestCatalogDocumentPartialArchiveAndRestoreVersions(t *testing.T) {
	w := newWorld(t)
	file := func(amount int64) *catalog.Application {
		price := catalog.ApplyPrice{Currency: catalog.Value("USD"), UnitAmount: catalog.Value(amount), Archived: catalog.Value(false), BillingIntervalHours: catalog.Null[int](), AccessDurationHours: catalog.Null[int](), TrialUnitAmount: catalog.Null[int64](), TrialDurationHours: catalog.Null[int]()}
		return &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
			"video":       {DisplayName: catalog.Value("Video"), Archived: catalog.Value(false), Entitlements: catalog.Value([]string{"video:one"}), Prices: map[string]catalog.ApplyPrice{"purchase": price}},
			"other-video": {DisplayName: catalog.Value("Other video"), Archived: catalog.Value(false), Prices: map[string]catalog.ApplyPrice{"purchase": price}},
		}}
	}
	apply := func(app *catalog.Application) {
		t.Helper()
		_, err := w.client[embedded].ApplyCatalog(t.Context(), app, billing.ApplyCatalogParams{})
		require.NoError(t, err)
	}
	apply(file(1_000_000))
	c := w.client[remote]
	first, err := priceByKey(t.Context(), c, "video", "purchase")
	require.NoError(t, err)
	apply(file(2_000_000))
	second, err := priceByKey(t.Context(), c, "video", "purchase")
	require.NoError(t, err)
	require.EqualValues(t, 2, second.Version)
	apply(&catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{"video": {DisplayName: catalog.Value("Updated display")}}})
	for _, key := range []string{"video", "other-video"} {
		product, err := productByKey(t.Context(), c, key)
		require.NoError(t, err)
		require.False(t, product.Archived, "omitted products remain available")
		current, err := priceByKey(t.Context(), c, key, "purchase")
		require.NoError(t, err)
		require.EqualValues(t, 2_000_000, current.UnitAmount, "omitted prices retain their financial terms")
	}
	apply(&catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
		"video":       {Archived: catalog.Value(true), Prices: map[string]catalog.ApplyPrice{"purchase": {Archived: catalog.Value(true)}}},
		"other-video": {Archived: catalog.Value(true), Prices: map[string]catalog.ApplyPrice{"purchase": {Archived: catalog.Value(true)}}},
	}})
	product, err := productByKey(t.Context(), c, "video")
	require.NoError(t, err)
	require.True(t, product.Archived)
	for _, id := range []billing.PriceID{first.ID, second.ID} {
		price, err := c.GetPrice(t.Context(), id, billing.GetPriceParams{})
		require.NoError(t, err)
		require.True(t, price.Archived)
	}
	restore := file(1_000_000)
	video := restore.Products["video"]
	video.DisplayName = catalog.Value("Video renamed")
	video.Entitlements = catalog.Value([]string{"video:two"})
	restore.Products["video"] = video
	apply(restore)
	restored, err := priceByKey(t.Context(), c, "video", "purchase")
	require.NoError(t, err)
	require.Equal(t, first.ID, restored.ID)
	require.Equal(t, first.Version, restored.Version)
	product, err = productByKey(t.Context(), c, "video")
	require.NoError(t, err)
	require.Equal(t, first.ProductID, product.ID)
	require.Equal(t, "Video renamed", product.DisplayName)
	require.Equal(t, []string{"video:two"}, product.Entitlements)
	revision := w.catalogRevision()
	apply(restore)
	require.Equal(t, revision, w.catalogRevision(), "the same content does not write again")
	_, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("API edit")})
	require.NoError(t, err)
	apply(restore)
	product, err = productByKey(t.Context(), c, "video")
	require.NoError(t, err)
	require.Equal(t, "API edit", product.DisplayName, "an already applied document cannot overwrite a later API edit")
	apply(file(2_000_000))
	still, err := priceByKey(t.Context(), c, "video", "purchase")
	require.NoError(t, err)
	require.Equal(t, first.ID, still.ID, "a previously applied batch cannot reactivate its former price")
	apply(&catalog.Application{SchemaVersion: 1, Prune: true})
	product, err = productByKey(t.Context(), c, "video")
	require.NoError(t, err)
	require.False(t, product.Archived, "prune keeps a product an edit touched")
	_, err = productByKey(t.Context(), c, "other-video")
	require.ErrorIs(t, err, billing.ErrNotFound, "explicit prune deletes an omitted product only documents set and nothing refers to")
	_, err = c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
	requireCode(t, err, http.StatusNotFound, "price_not_found")
}

// A provider price's terms are its provider's: when the provider changes them,
// the next declaration is a new version that takes the provider object, and
// the earlier one keeps its subscribers' history. A key of ours never becomes
// a provider price, nor the reverse.
func TestProviderPriceVersionsFollowTheProvider(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "bound", DisplayName: "Bound"})
	require.NoError(t, err)
	plan := "bound_plan_" + uuid.NewString()[:8]
	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Bound", Amount: "10.00", Days: 30})
	apply := func() (*billing.CatalogApplicationReceipt, error) {
		return c.ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{product.Key: {ProviderPrices: map[string]catalog.ApplyProviderPrice{
			"monthly": {PSP: "nmi", PlanID: plan}}}}}, billing.ApplyCatalogParams{})
	}
	_, err = apply()
	require.NoError(t, err)
	first, err := priceByKey(t.Context(), c, product.Key, "monthly")
	require.NoError(t, err)
	require.Equal(t, int64(10_000_000), first.UnitAmount)
	require.Equal(t, plan, first.Provider.IDs["plan_id"])

	w.nmi.AddPlan(nmimock.Plan{ID: plan, Name: "Bound", Amount: "12.00", Days: 30})
	_, err = apply()
	require.NoError(t, err)
	second, err := priceByKey(t.Context(), c, product.Key, "monthly")
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID, "new terms are a new version")
	require.Equal(t, int64(12_000_000), second.UnitAmount, "the provider's terms win")
	require.Equal(t, plan, second.Provider.IDs["plan_id"])
	earlier, err := c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.True(t, earlier.Archived)
	require.NotNil(t, earlier.Provider, "an earlier version stays the provider's")

	_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 5_000_000, Currency: "USD"})
	require.ErrorIs(t, err, billing.ErrInvalid, "a provider price's key takes none of ours")
	_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "ours", UnitAmount: 5_000_000, Currency: "USD"})
	require.NoError(t, err)
	_, err = c.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "ours", PSP: "nmi", PlanID: plan})
	require.ErrorIs(t, err, billing.ErrInvalid, "nor ours a provider's")
}

// These rows have never been purchased. Rejection must come from the database
// immutability rule, not an accidental child foreign key or an API-only guard.
func TestCatalogImmutableDatabaseRows(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "immutable", DisplayName: "Immutable"})
	require.NoError(t, err)
	other, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "other", DisplayName: "Other"})
	require.NoError(t, err)
	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	for name, statement := range map[string]string{
		"amount":           "UPDATE billing.prices SET amount=2000000 WHERE id=$1",
		"currency":         "UPDATE billing.prices SET currency='EUR' WHERE id=$1",
		"duration":         "UPDATE billing.prices SET access_duration_hours=720 WHERE id=$1",
		"billing_interval": "UPDATE billing.prices SET billing_interval_hours=720, access_duration_hours=720 WHERE id=$1",
		"trial":            "UPDATE billing.prices SET billing_interval_hours=720, access_duration_hours=720, trial_unit_amount=0, trial_duration_hours=24 WHERE id=$1",
		"key":              "UPDATE billing.prices SET key='renamed' WHERE id=$1",
		"revision":         "UPDATE billing.prices SET revision=revision+1 WHERE id=$1",
		"product":          fmt.Sprintf("UPDATE billing.prices SET product_id='%s' WHERE id=$1", other.ID.UUID()),
		"price delete":     "DELETE FROM billing.prices WHERE id=$1",
		"product key":      fmt.Sprintf("UPDATE billing.products SET key='renamed-product' WHERE id='%s' AND $1::uuid IS NOT NULL", other.ID.UUID()),
		"product delete":   fmt.Sprintf("DELETE FROM billing.products WHERE id='%s' AND $1::uuid IS NOT NULL", other.ID.UUID()),
	} {
		t.Run(name, func(t *testing.T) {
			err := w.catalogExec(t.Context(), w.sql(statement), w.priceStorageID(price.ID))
			var pgerr *pgconn.PgError
			require.ErrorAs(t, err, &pgerr)
			require.Equal(t, "23514", pgerr.Code, "immutable row must be rejected before referential checks")
		})
	}
	for _, table := range []string{"products", "prices"} {
		t.Run("truncate "+table, func(t *testing.T) {
			tx, err := w.pool.Begin(t.Context())
			require.NoError(t, err)
			defer tx.Rollback(t.Context())
			_, err = tx.Exec(t.Context(), w.sql("TRUNCATE billing."+table+" CASCADE"))
			var pgerr *pgconn.PgError
			require.ErrorAs(t, err, &pgerr)
			require.Equal(t, "23514", pgerr.Code)
		})
	}
	unchanged, err := c.GetPrice(t.Context(), price.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Equal(t, price.ID, unchanged.ID)
	require.Equal(t, price.ProductID, unchanged.ProductID)
	require.Equal(t, price.Key, unchanged.Key)
	require.Equal(t, price.UnitAmount, unchanged.UnitAmount)
	require.Equal(t, price.Revision, unchanged.Revision)
}

func TestCatalogRepriceStaysWithinProduct(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	products := make([]*billing.Product, 2)
	prices := make([]*billing.Price, 2)
	subs := make([]billing.SubscriptionID, 2)
	var current *billing.Price
	hours := monthHours
	for i := range products {
		entitlement := fmt.Sprintf("content:scoped-%d", i)
		product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: fmt.Sprintf("scoped-%d", i), DisplayName: "Scoped", Entitlements: []string{entitlement}})
		require.NoError(t, err)
		products[i] = product
		prices[i], err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 10_000_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		require.NoError(t, err)
		buyer := w.newCustomer()
		subs[i] = buyer.subscribe(embedded, "stripe", prices[i].ID.String(), entitlement, buyer.saveCard("stripe", visa))
		next, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 12_000_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		require.NoError(t, err)
		require.Equal(t, prices[i].ID, w.subscription(embedded, subs[i]).PriceID, "catalog replacement cannot reprice the existing subscription")
		if i == 0 {
			current = next
		}
	}
	preview, err := c.PreviewPriceMigration(t.Context(), billing.CreatePriceMigrationParams{ProductKey: products[0].Key, PriceKey: "monthly", ToPriceID: current.ID})
	require.NoError(t, err)
	require.Equal(t, 1, preview.Matched)
	migration, err := c.CreatePriceMigration(t.Context(), billing.CreatePriceMigrationParams{ProductKey: products[0].Key, PriceKey: "monthly", ToPriceID: current.ID, EffectiveAt: w.clock.Now().Add(45 * 24 * time.Hour)})
	require.NoError(t, err)
	require.Equal(t, 1, migration.Scheduled)
	require.NotNil(t, w.subscription(embedded, subs[0]).ScheduledChange)
	require.Nil(t, w.subscription(embedded, subs[1]).ScheduledChange, "a key names one product's prices")
	listed, err := c.ListPriceMigrations(t.Context(), billing.PriceMigrationListParams{ProductKey: products[1].Key, PriceKey: "monthly"})
	require.NoError(t, err)
	require.Empty(t, listed.Items)
}

func TestCatalogArchivePreservesAppliedHashesAndPriceRevisions(t *testing.T) {
	w := newWorld(t)
	declaration := func(amount int64) *catalog.Application {
		return &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
			"portable": {DisplayName: catalog.Value("Portable"), Prices: map[string]catalog.ApplyPrice{"purchase": {Currency: catalog.Value("USD"), UnitAmount: catalog.Value(amount)}}},
		}}
	}
	c := w.client[embedded]
	for _, amount := range []int64{4_000_000, 7_000_000} {
		_, err := c.ApplyCatalog(t.Context(), declaration(amount), billing.ApplyCatalogParams{})
		require.NoError(t, err)
	}
	product, err := productByKey(t.Context(), c, "portable")
	require.NoError(t, err)
	product, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("API edited")})
	require.NoError(t, err)
	current, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", Currency: "USD", UnitAmount: 9_000_000})
	require.NoError(t, err)
	require.EqualValues(t, 3, current.Version)
	versions, err := c.ListPrices(t.Context(), billing.PriceListParams{ProductID: current.ProductID})
	require.NoError(t, err)
	require.Len(t, versions.Items, 3)
	buyer := w.newCustomer()
	_, err = createCreditGrant(t.Context(), c, buyer.cid(), billing.CreateCreditGrantParams{Amount: 2_000_000, Currency: "USD", Source: "archive-catalog", SourceID: "retained-credit"})
	require.NoError(t, err)
	w.settle()
	events, err := c.ListHostEvents(t.Context(), billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err = c.AcknowledgeHostEvents(t.Context(), []billing.HostEventID{event.ID})
		require.NoError(t, err)
	}
	merchantID := c.MerchantID()
	w.stop()
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	var archive bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), source, merchantID, &archive))

	schema := "archive_catalog_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, engine.Migrate(t.Context(), w.pool, openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}}))
	name := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE") })
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+name+`.merchants (id, slug, status, permission_group_id) VALUES ($1, $2, 'active', $3)`, merchantID.UUID(), w.slug, uuid.New())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, schema)
	require.NoError(t, err)
	result, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	require.Positive(t, result.Rows)
	var receipts int
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+name+".catalog_applications").Scan(&receipts))
	require.Equal(t, 2, receipts)
	var productRevision int64
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT revision FROM "+name+".products WHERE id=$1", product.ID.UUID()).Scan(&productRevision))
	require.Equal(t, product.Revision, productRevision)
	for _, price := range versions.Items {
		var revision int64
		var publicID string
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT revision,public_id FROM "+name+".prices WHERE id=$1", w.priceStorageID(price.ID)).Scan(&revision, &publicID))
		require.Equal(t, price.Version, revision+1, "versions follow the accepted chronology")
		require.Equal(t, price.ID.String(), publicID, "public ID must be derived after the restored revision is known")
	}
	boot := func(app *catalog.Application) (*openrails.Engine, *openrails.Client) {
		t.Helper()
		bill, err := openrails.New(t.Context(), openrails.Config{Database: openrails.DatabaseConfig{Schema: schema, RiverSchema: schema}, ProviderWriteMode: openrails.ProviderWritesFull,
			Merchants: map[string]openrails.MerchantDeclaration{w.slug: {Sandbox: true, DisplayName: w.slug, PSPs: w.psps}},
		}, openrails.Deps{FXTransport: testFX.Transport(), Postgres: w.pool, StripeTransport: w.stripe, NMITransport: w.nmi, Clock: w.clock})
		require.NoError(t, err)
		t.Cleanup(func() { _ = bill.Close(context.Background()) })
		shop, err := bill.MerchantBySlug(w.slug)
		require.NoError(t, err)
		_, err = shop.ApplyCatalog(t.Context(), app, billing.ApplyCatalogParams{})
		require.NoError(t, err)
		return bill, shop
	}
	staleEngine, stale := boot(declaration(4_000_000))
	still, err := priceByKey(t.Context(), stale, "portable", "purchase")
	require.NoError(t, err)
	require.Equal(t, current.ID, still.ID, "an applied hash remains a replay after restore and later API edits")
	product, err = productByKey(t.Context(), stale, "portable")
	require.NoError(t, err)
	require.Equal(t, "API edited", product.DisplayName)
	balance, err := stale.GetBalance(t.Context(), buyer.cid(), "USD")
	require.NoError(t, err)
	require.EqualValues(t, 2_000_000, balance.BalanceAmount, "the retained money book is restored with the catalog")
	require.NoError(t, staleEngine.Close(t.Context()))
	// The restored owners still name the API edits: a new file skips them.
	nextEngine, next := boot(declaration(8_000_000))
	price, err := priceByKey(t.Context(), next, "portable", "purchase")
	require.NoError(t, err)
	require.Equal(t, current.ID, price.ID, "a restored edit is still the edit's")
	_, err = next.ApplyCatalog(t.Context(), declaration(8_000_000), billing.ApplyCatalogParams{Force: true})
	require.NoError(t, err)
	price, err = priceByKey(t.Context(), next, "portable", "purchase")
	require.NoError(t, err)
	require.EqualValues(t, 4, price.Version, "new terms allocate after all restored versions")
	require.NoError(t, nextEngine.Close(t.Context()))
}
