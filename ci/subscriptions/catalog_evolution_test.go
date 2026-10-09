//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"fmt"
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
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
)

// A key identifies a product-local chain; equal money under another key is a
// separate offer. Reusing financial terms preserves the original UUID/revision.
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
				require.Zero(t, original[i].Revision)
				alias := create(product, "other-offer", 1_000_000)
				require.NotEqual(t, original[i].ID, alias.ID, "equal financial terms cannot steal another key")
				require.Zero(t, alias.Revision)
				repeat := create(product, "monthly", 1_000_000)
				require.Equal(t, original[i].ID, repeat.ID)
				require.Equal(t, original[i].Revision, repeat.Revision)
			}
			next := create(products[0], "monthly", 2_000_000)
			require.EqualValues(t, 1, next.Revision)
			require.NotEqual(t, original[0].ID, next.ID)
			prior, err := c.GetPrice(t.Context(), original[0].ID, billing.GetPriceParams{})
			require.NoError(t, err)
			require.True(t, prior.Archived)
			other, err := c.GetPriceByKey(t.Context(), products[1].Key, "monthly")
			require.NoError(t, err)
			require.Equal(t, original[1].ID, other.ID)
			restored := create(products[0], "monthly", 1_000_000)
			require.Equal(t, original[0].ID, restored.ID)
			require.Zero(t, restored.Revision)
			require.False(t, restored.Archived)
			third := create(products[0], "monthly", 3_000_000)
			require.EqualValues(t, 2, third.Revision, "reactivating v0 does not reset revision allocation")
			for i, product := range products {
				history, err := c.ListPriceKeyHistory(t.Context(), product.Key, "monthly", billing.PageRequest{})
				require.NoError(t, err)
				require.NotEmpty(t, history.Items)
				for _, movement := range history.Items {
					require.Equal(t, products[i].ID, movement.Price.ProductID)
				}
			}
			// Existing catalogs and imports can carry pre-revision UUIDs. The
			// natural-key lookup must preserve them rather than mint a duplicate.
			legacyID := uuid.New()
			_, err = w.pool.Exec(t.Context(), w.sql(`INSERT INTO billing.prices
				(id, merchant_id, product_id, key, amount, currency, archived, billing_interval_hours)
				SELECT $1, merchant_id, id, 'imported', 4000000, 'USD', false, NULL
				FROM billing.products WHERE id=$2`), legacyID, products[0].ID.UUID())
			require.NoError(t, err)
			imported := create(products[0], "imported", 4_000_000)
			require.Equal(t, billing.PriceID(legacyID), imported.ID)
			require.Zero(t, imported.Revision)
		})
	}
}

func TestCatalogProductRevisionTracksMutableFields(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "mutable", DisplayName: "Before", Entitlements: []string{"old-benefit"}})
	require.NoError(t, err)
	require.Zero(t, product.Revision)
	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	patch := billing.UpdateProductParams{DisplayName: catalog.Value("After"), Entitlements: catalog.Value([]string{"new-benefit"})}
	updated, err := c.UpdateProduct(t.Context(), product.ID, patch)
	require.NoError(t, err)
	require.Equal(t, product.ID, updated.ID)
	require.EqualValues(t, 1, updated.Revision)
	repeated, err := c.UpdateProduct(t.Context(), product.ID, patch)
	require.NoError(t, err)
	require.Equal(t, updated.Revision, repeated.Revision, "an unchanged product patch does not manufacture a revision")
	current, err := c.GetPriceByKey(t.Context(), product.Key, "purchase")
	require.NoError(t, err)
	require.Equal(t, price.ID, current.ID, "product metadata changes cannot replace its immutable prices")
	require.Equal(t, price.Revision, current.Revision)
	archived, err := c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
	require.NoError(t, err)
	require.EqualValues(t, 2, archived.Revision)
	restored, err := c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(false)})
	require.NoError(t, err)
	require.Equal(t, product.ID, restored.ID)
	require.EqualValues(t, 3, restored.Revision)
}

func TestCatalogConcurrentPriceVersions(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "concurrent", DisplayName: "Concurrent"})
	require.NoError(t, err)
	const writers = 9
	results := make([]*billing.Price, writers)
	errors := make([]error, writers)
	start := make(chan struct{})
	var running sync.WaitGroup
	for i := range writers {
		running.Go(func() {
			<-start
			results[i], errors[i] = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", UnitAmount: int64(i%3+1) * 1_000_000, Currency: "USD"})
		})
	}
	close(start)
	running.Wait()
	for i, err := range errors {
		require.NoError(t, err, "concurrent writer %d", i)
		require.Equal(t, results[i%3].ID, results[i].ID, "repeated terms keep one immutable ID")
		require.Equal(t, results[i%3].Revision, results[i].Revision)
	}
	prices, err := c.ListPrices(t.Context(), billing.PriceListParams{ProductID: product.ID})
	require.NoError(t, err)
	require.Len(t, prices.Items, 3)
	revisions, live := map[int64]bool{}, 0
	for _, price := range prices.Items {
		revisions[price.Revision] = true
		if !price.Archived {
			live++
		}
	}
	require.Equal(t, map[int64]bool{0: true, 1: true, 2: true}, revisions)
	require.Equal(t, 1, live)
}

func TestDeclaredCatalogPartialArchiveAndRestoreVersions(t *testing.T) {
	w := newWorld(t)
	file := func(amount int64) *catalog.Application {
		price := catalog.ApplyPrice{Currency: catalog.Value("USD"), UnitAmount: catalog.Value(amount), Archived: catalog.Value(false), BillingIntervalHours: catalog.Null[int](), AccessDurationHours: catalog.Null[int](), TrialUnitAmount: catalog.Null[int64](), TrialDurationHours: catalog.Null[int]()}
		return &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
			"video":       {DisplayName: catalog.Value("Video"), Archived: catalog.Value(false), Entitlements: catalog.Value([]string{"video:one"}), Prices: map[string]catalog.ApplyPrice{"purchase": price}},
			"other-video": {DisplayName: catalog.Value("Other video"), Archived: catalog.Value(false), Prices: map[string]catalog.ApplyPrice{"purchase": price}},
		}}
	}
	boot := func(app *catalog.Application) {
		t.Helper()
		client, err := w.bootDeclared(t.Context(), app)
		require.NoError(t, err)
		require.NoError(t, client.Close(t.Context()))
	}
	boot(file(1_000_000))
	c := w.client[remote]
	first, err := c.GetPriceByKey(t.Context(), "video", "purchase")
	require.NoError(t, err)
	boot(file(2_000_000))
	second, err := c.GetPriceByKey(t.Context(), "video", "purchase")
	require.NoError(t, err)
	require.EqualValues(t, 1, second.Revision)
	boot(&catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{"video": {DisplayName: catalog.Value("Updated display")}}})
	for _, key := range []string{"video", "other-video"} {
		product, err := c.GetProductByKey(t.Context(), key)
		require.NoError(t, err)
		require.False(t, product.Archived, "omitted products remain available")
		current, err := c.GetPriceByKey(t.Context(), key, "purchase")
		require.NoError(t, err)
		require.EqualValues(t, 2_000_000, current.UnitAmount, "omitted prices retain their financial terms")
	}
	boot(&catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{
		"video":       {Archived: catalog.Value(true), Prices: map[string]catalog.ApplyPrice{"purchase": {Archived: catalog.Value(true)}}},
		"other-video": {Archived: catalog.Value(true), Prices: map[string]catalog.ApplyPrice{"purchase": {Archived: catalog.Value(true)}}},
	}})
	product, err := c.GetProductByKey(t.Context(), "video")
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
	boot(restore)
	restored, err := c.GetPriceByKey(t.Context(), "video", "purchase")
	require.NoError(t, err)
	require.Equal(t, first.ID, restored.ID)
	require.Equal(t, first.Revision, restored.Revision)
	product, err = c.GetProductByKey(t.Context(), "video")
	require.NoError(t, err)
	require.Equal(t, first.ProductID, product.ID)
	require.Equal(t, "Video renamed", product.DisplayName)
	require.Equal(t, []string{"video:two"}, product.Entitlements)
	revision := w.catalogRevision()
	boot(restore)
	require.Equal(t, revision, w.catalogRevision(), "the same content does not write again")
	_, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("API edit")})
	require.NoError(t, err)
	boot(restore)
	product, err = c.GetProductByKey(t.Context(), "video")
	require.NoError(t, err)
	require.Equal(t, "API edit", product.DisplayName, "an already applied startup batch cannot overwrite a later API edit")
	boot(file(2_000_000))
	still, err := c.GetPriceByKey(t.Context(), "video", "purchase")
	require.NoError(t, err)
	require.Equal(t, first.ID, still.ID, "a previously applied batch cannot reactivate its former price")
	boot(&catalog.Application{SchemaVersion: 1, Prune: true})
	product, err = c.GetProductByKey(t.Context(), "video")
	require.NoError(t, err)
	require.True(t, product.Archived, "explicit prune retires omitted products")
	retired, err := c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.True(t, retired.Archived, "explicit prune archives prices without deleting history")
	require.Equal(t, first.Revision, retired.Revision)
}

// Matching an archived version by its full terms must also recover its PSP
// bindings. Otherwise an omitted binding silently erases grandfathered routing.
func TestCatalogArchivedVersionKeepsProviderBindings(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: "bound", DisplayName: "Bound"})
	require.NoError(t, err)
	hours := monthHours
	create := func(amount int64, plan string) *billing.Price {
		t.Helper()
		price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: amount, Currency: "USD", AccessDurationHours: &hours, BillingIntervalHours: &hours, PSPLinks: map[string]map[string]string{"nmi": {"plan_id": plan}}})
		require.NoError(t, err)
		return price
	}
	first := create(1_000_000, "bound-first")
	second := create(2_000_000, "bound-second")
	_, err = c.UpdatePrice(t.Context(), second.ID, billing.UpdatePriceParams{Archived: catalog.Value(true)})
	require.NoError(t, err)
	_, err = c.ApplyCatalog(t.Context(), &catalog.Application{SchemaVersion: 1, Products: map[string]catalog.ApplyProduct{product.Key: {Prices: map[string]catalog.ApplyPrice{"monthly": {
		Currency: catalog.Value("USD"), UnitAmount: catalog.Value(int64(1_000_000)), AccessDurationHours: catalog.Value(hours), BillingIntervalHours: catalog.Value(hours), TrialUnitAmount: catalog.Null[int64](), TrialDurationHours: catalog.Null[int](), Archived: catalog.Value(true),
	}}}}})
	require.NoError(t, err)
	preserved, err := c.GetPrice(t.Context(), first.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.True(t, preserved.Archived)
	require.Equal(t, "bound-first", preserved.PSPs["nmi"].IDs["plan_id"])
	other, err := c.GetPrice(t.Context(), second.ID, billing.GetPriceParams{})
	require.NoError(t, err)
	require.Equal(t, "bound-second", other.PSPs["nmi"].IDs["plan_id"])
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
			_, err := w.pool.Exec(t.Context(), w.sql(statement), price.ID.UUID())
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
		_, err = c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "monthly", UnitAmount: 12_000_000, Currency: "USD", BillingIntervalHours: &hours, AccessDurationHours: &hours})
		require.NoError(t, err)
		require.Equal(t, prices[i].ID, w.subscription(embedded, subs[i]).PriceID, "catalog replacement cannot reprice the existing subscription")
	}
	preview, err := c.PreviewRepriceBatch(t.Context(), billing.PreviewRepriceBatchParams{ProductKey: products[0].Key, PriceKey: "monthly"})
	require.NoError(t, err)
	require.Equal(t, 1, preview.Matched)
	batch, err := c.CreateRepriceBatch(t.Context(), billing.CreateRepriceBatchParams{ProductKey: products[0].Key, PriceKey: "monthly", EffectiveAt: w.clock.Now().Add(45 * 24 * time.Hour)})
	require.NoError(t, err)
	require.Len(t, batch.Scheduled, 1)
	require.Equal(t, subs[0], batch.Scheduled[0].SubscriptionID)
	other, err := c.ListReprices(t.Context(), billing.RepriceListParams{SubscriptionID: subs[1]})
	require.NoError(t, err)
	require.Empty(t, other.Items)
	listed, err := c.ListRepriceBatches(t.Context(), billing.RepriceBatchListParams{ProductKey: products[1].Key, PriceKey: "monthly"})
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
	for _, amount := range []int64{4_000_000, 7_000_000} {
		client, err := w.bootDeclared(t.Context(), declaration(amount))
		require.NoError(t, err)
		require.NoError(t, client.Close(t.Context()))
	}
	c := w.client[embedded]
	product, err := c.GetProductByKey(t.Context(), "portable")
	require.NoError(t, err)
	product, err = c.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{DisplayName: catalog.Value("API edited")})
	require.NoError(t, err)
	current, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "purchase", Currency: "USD", UnitAmount: 9_000_000})
	require.NoError(t, err)
	require.EqualValues(t, 2, current.Revision)
	versions, err := c.ListPrices(t.Context(), billing.PriceListParams{ProductID: current.ProductID})
	require.NoError(t, err)
	require.Len(t, versions.Items, 3)
	buyer := w.newCustomer()
	_, err = c.CreateCreditGrant(t.Context(), buyer.cid(), billing.CreateCreditGrantParams{Amount: 2_000_000, Currency: "USD", Source: "archive-catalog", SourceID: "retained-credit"})
	require.NoError(t, err)
	w.settle()
	events, err := c.ListHostEvents(t.Context(), billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err = c.AcknowledgeHostEvent(t.Context(), event.ID)
		require.NoError(t, err)
	}
	merchantID := c.MerchantID()
	w.stop()
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	var archive bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), source, merchantID, &archive))

	// The pre-revision row format omitted product and price revisions. Rebuild
	// the footer so the old shape is an intact archive.
	var legacy bytes.Buffer
	var writer *archivewire.Writer
	table := ""
	_, err = archivewire.Read(bytes.NewReader(archive.Bytes()), func(header archivewire.Header) error {
		var err error
		writer, err = archivewire.NewWriter(&legacy, header.MerchantID, header.CatalogRevision)
		return err
	}, func(record archivewire.Record) error {
		if record.Kind == "table" {
			table = record.Table
			return writer.Table(table)
		}
		values := record.Values
		if table == "prices" || table == "products" {
			values = append(append([]*string{}, values[:1]...), values[2:]...)
		}
		return writer.Row(values)
	})
	require.NoError(t, err)
	require.NoError(t, writer.Close())

	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"current", archive.Bytes()},
		{"pre-revision", legacy.Bytes()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			schema := "archive_catalog_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
			require.NoError(t, openrails.Migrate(t.Context(), w.pool, openrails.Config{Schema: schema, RiverSchema: schema}))
			name := pgx.Identifier{schema}.Sanitize()
			t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE") })
			_, err := w.pool.Exec(t.Context(), "INSERT INTO "+name+`.merchants (id, slug, status, permission_group_id, display_name) VALUES ($1, $2, 'active', $3, 'Restored')`, merchantID.UUID(), w.slug, uuid.New())
			require.NoError(t, err)
			destination, err := db.NewWithPGXPool(w.pool, schema)
			require.NoError(t, err)
			result, err := merchantarchive.Restore(t.Context(), destination, merchantID, bytes.NewReader(tc.data))
			require.NoError(t, err)
			require.Positive(t, result.Rows)
			var receipts int
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+name+".catalog_applications").Scan(&receipts))
			require.Equal(t, 2, receipts)
			var productRevision int64
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT revision FROM "+name+".products WHERE id=$1", product.ID.UUID()).Scan(&productRevision))
			if tc.name == "current" {
				require.Equal(t, product.Revision, productRevision)
			}
			for _, price := range versions.Items {
				var revision int64
				require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT revision FROM "+name+".prices WHERE id=$1", price.ID.UUID()).Scan(&revision))
				if tc.name == "current" {
					require.Equal(t, price.Revision, revision, "current archives preserve the exact public revision")
				}
			}
			boot := func(app *catalog.Application) *openrails.Client {
				t.Helper()
				client, err := openrails.New(t.Context(), openrails.Config{Schema: schema, RiverSchema: schema, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
					Merchant: openrails.MerchantDeclaration{Slug: w.slug, DisplayName: w.slug, PSPs: w.psps}, Catalog: app,
				}, openrails.Deps{Postgres: w.pool, StripeTransport: w.stripe, NMITransport: w.nmi, Clock: w.clock})
				require.NoError(t, err)
				t.Cleanup(func() { _ = client.Close(context.Background()) })
				return client
			}
			stale := boot(declaration(4_000_000))
			still, err := stale.GetPriceByKey(t.Context(), "portable", "purchase")
			require.NoError(t, err)
			require.Equal(t, current.ID, still.ID, "an applied hash remains a replay after restore and later API edits")
			product, err := stale.GetProductByKey(t.Context(), "portable")
			require.NoError(t, err)
			require.Equal(t, "API edited", product.DisplayName)
			balance, err := stale.GetBalance(t.Context(), buyer.cid(), "USD")
			require.NoError(t, err)
			require.EqualValues(t, 2_000_000, balance.BalanceAmount, "the retained money book is restored with the catalog")
			require.NoError(t, stale.Close(t.Context()))
			next := boot(declaration(8_000_000))
			price, err := next.GetPriceByKey(t.Context(), "portable", "purchase")
			require.NoError(t, err)
			require.EqualValues(t, 3, price.Revision, "new terms allocate after all restored revisions")
			require.NoError(t, next.Close(t.Context()))
		})
	}
}
