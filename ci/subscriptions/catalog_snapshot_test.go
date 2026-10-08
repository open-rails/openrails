//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/configdocument"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

func TestCatalogSnapshotPreservesPurchasedArchivedIdentities(t *testing.T) {
	w := newWorld(t)
	c := w.client[embedded]
	mid := c.MerchantID()
	buyer := w.newCustomer()
	method := buyer.saveCard("nmi", visa)
	declaration := func(amount int64) *catalog.Application {
		return &catalog.Application{SchemaVersion: 1, Products: []catalog.ApplyProduct{{Key: "retained-video", DisplayName: catalog.Value("Video"), EntitlementsSpec: catalog.Value(map[string]*int{"video:101": nil}), Prices: []catalog.ApplyPrice{{Key: "buy", Currency: catalog.Value("USD"), UnitAmount: catalog.Value(amount)}}}}}
	}
	_, err := c.ApplyCatalog(t.Context(), declaration(4000000))
	require.NoError(t, err)
	first, err := c.GetPriceByKey(t.Context(), "retained-video", "buy")
	require.NoError(t, err)
	paid, err := c.CreateCheckoutAttempt(t.Context(), billing.CreateCheckoutAttemptParams{Customer: buyer.identity(), PriceID: first.ID, IdempotencyKey: "snapshot-video", PaymentOptions: billing.CheckoutPaymentOptions{PSP: "nmi", PaymentMethodID: pmid(method)}})
	require.NoError(t, err)
	require.Equal(t, billing.CheckoutAttemptSucceeded, paid.Status)
	_, err = c.ApplyCatalog(t.Context(), declaration(7000000))
	require.NoError(t, err)
	_, err = c.ArchiveProduct(t.Context(), billing.ArchiveProductParams{ProductID: first.ProductID, IdempotencyKey: "snapshot-archive", Reason: "Catalog retirement"})
	require.NoError(t, err)
	// Default and customer-specific rate cards belong to the catalog snapshot.
	rates, err := catalog.ParseApplicationYAML([]byte(`schema_version: 1
meters:
- key: requests
  event_type: api.request
  aggregation: count
products:
- key: api-balance
  display_name: Prepaid API balance
  credit_grant: {currency: USD, from_payment: true, expires_after_days: 365}
  prices:
  - key: deposit
    currency: USD
    unit_amount: 0
    customer_amount: {min_amount: 1000000, max_amount: 1000000000}
  rate_cards:
  - meter: requests
    price: {model: per_unit, currency: USD, per_unit: {unit_amount: "1000"}}
`))
	require.NoError(t, err)
	_, err = c.ApplyCatalog(t.Context(), rates)
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.catalog_rate_cards(merchant_id,customer_id,ordinal,meter_key,payment_term,filter,price) VALUES($1,$2,1,'requests','in_arrears','null','{"model":"per_unit","currency":"USD","per_unit":{"unit_amount":"500"}}')`), mid.UUID(), uuid.MustParse(buyer.id))
	require.NoError(t, err)
	// A retained binding references an already provisioned account; no secret is copied.
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.price_psp_bindings(merchant_id,price_id,psp_id,configuration) VALUES($1,$2,$3,'{"provider":"nmi"}')`), mid.UUID(), first.ID.UUID(), w.psp["nmi"].UUID())
	require.NoError(t, err)
	w.settle()
	w.stop()
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	var artifact bytes.Buffer
	err = merchantarchive.ExportCatalog(t.Context(), source, mid, &artifact)
	require.NoError(t, err, "cause: %v", errors.Unwrap(err))
	require.Contains(t, artifact.String(), "kind: catalog_snapshot")
	require.NotContains(t, artifact.String(), "e2e-nmi-key")
	var document merchantarchive.CatalogSnapshot
	raw, err := configdocument.YAMLToJSON(artifact.Bytes(), merchantarchive.CatalogSnapshotMaxBytes)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &document))
	require.Len(t, document.Tables, 8)
	require.Len(t, document.Tables["prices"], 3)
	require.Len(t, document.Tables["product_archive_operations"], 1)
	require.Len(t, document.Dependencies.PSPs, 1)
	require.Equal(t, []string{buyer.id}, document.Dependencies.Customers)

	schema := "catalog_snapshot_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, openrails.Migrate(t.Context(), w.pool, openrails.Config{Schema: schema, River: openrails.RiverHostOwned}))
	dst := pgx.Identifier{schema}.Sanitize()
	src := pgx.Identifier{w.schema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+dst+" CASCADE") })
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+dst+`.merchants(id,slug,status,permission_group_id,display_name) VALUES($1,'catalog-snapshot-target','active',$2,'Target')`, mid.UUID(), uuid.New())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, schema)
	require.NoError(t, err)
	restore := func(data []byte) (merchantarchive.Result, error) {
		return merchantarchive.RestoreCatalog(t.Context(), destination, mid, bytes.NewReader(data))
	}
	refusal := func(err error, code string) {
		t.Helper()
		var typed *merchantarchive.Error
		require.ErrorAs(t, err, &typed)
		require.Equal(t, code, typed.Code, "cause: %v", errors.Unwrap(err))
	}
	empty := func() {
		t.Helper()
		for _, table := range []string{"products", "prices", "catalog_restore_receipts"} {
			var count int
			require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+dst+"."+table).Scan(&count))
			require.Zero(t, count, table)
		}
		var revision int64
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT catalog_revision FROM "+dst+".merchants WHERE id=$1", mid.UUID()).Scan(&revision))
		require.Zero(t, revision)
	}
	_, err = merchantarchive.RestoreCatalog(t.Context(), destination, billing.MerchantID(uuid.New()), bytes.NewReader(artifact.Bytes()))
	refusal(err, "merchant_mismatch")
	empty()
	_, err = restore(bytes.Replace(artifact.Bytes(), []byte("Prepaid API balance"), []byte("Tampered balance"), 1))
	refusal(err, "invalid_artifact")
	empty()
	_, err = restore(artifact.Bytes())
	refusal(err, "dependency_mismatch")
	empty()
	// Provision existing identities separately, first deliberately with a wrong PSP account.
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+dst+`.psps(merchant_id,id,key,rail,environment,account_id,settings) SELECT merchant_id,id,key,rail,environment,'wrong-account','{}' FROM `+src+`.psps WHERE id=$1`, w.psp["nmi"].UUID())
	require.NoError(t, err)
	_, err = restore(artifact.Bytes())
	refusal(err, "dependency_mismatch")
	empty()
	_, err = w.pool.Exec(t.Context(), "UPDATE "+dst+`.psps SET account_id=(SELECT account_id FROM `+src+`.psps WHERE id=$1) WHERE id=$1`, w.psp["nmi"].UUID())
	require.NoError(t, err)
	_, err = restore(artifact.Bytes())
	refusal(err, "dependency_mismatch")
	empty()
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+dst+`.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), uuid.MustParse(buyer.id))
	require.NoError(t, err)
	// A sealed artifact with a broken later-table FK proves all earlier inserts roll back.
	originalProduct := document.Tables["product_archive_operations"][0]["product_id"]
	document.Tables["product_archive_operations"][0]["product_id"], err = json.Marshal(uuid.NewString())
	require.NoError(t, err)
	var broken bytes.Buffer
	require.NoError(t, merchantarchive.WriteCatalogSnapshot(&broken, document))
	_, err = restore(broken.Bytes())
	refusal(err, "integrity")
	empty()
	document.Tables["product_archive_operations"][0]["product_id"] = originalProduct

	type outcome struct {
		result merchantarchive.Result
		err    error
	}
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() { result, err := restore(artifact.Bytes()); outcomes <- outcome{result, err} }()
	}
	a, b := <-outcomes, <-outcomes
	require.NoError(t, a.err, "cause: %v", errors.Unwrap(a.err))
	require.NoError(t, b.err, "cause: %v", errors.Unwrap(b.err))
	require.NotEqual(t, a.result.Replayed, b.result.Replayed, "one import wins; its concurrent retry replays")
	require.Equal(t, a.result.Digest, b.result.Digest)
	result := a.result
	// Compare every stored column, including all original UUIDs, revisions,
	// timestamps, archival flags, movement records and application hashes.
	for table := range document.Tables {
		read := func(name string) string {
			var rows string
			query := fmt.Sprintf("SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]')::text FROM %s.%s t WHERE merchant_id=$1", name, table)
			require.NoError(t, w.pool.QueryRow(t.Context(), query, mid.UUID()).Scan(&rows))
			return rows
		}
		require.Equal(t, read(src), read(dst), table)
	}
	var restored bytes.Buffer
	require.NoError(t, merchantarchive.ExportCatalog(t.Context(), destination, mid, &restored))
	require.Equal(t, artifact.String(), restored.String())
	// The real source payment still joins the restored archived price/product,
	// and target-side foreign keys accept that exact original price identity.
	var preserved bool
	require.NoError(t, w.pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM `+src+`.payments pay JOIN `+dst+`.prices price ON price.merchant_id=pay.merchant_id AND price.id=pay.price_id JOIN `+dst+`.products product ON product.merchant_id=price.merchant_id AND product.id=price.product_id WHERE pay.id=$1 AND price.archived AND product.archived)`, paid.PaymentID.UUID()).Scan(&preserved))
	require.True(t, preserved)
	_, err = w.pool.Exec(t.Context(), "CREATE TABLE "+dst+`.purchase_reference_probe(merchant_id uuid,price_id uuid,FOREIGN KEY(merchant_id,price_id) REFERENCES `+dst+`.prices(merchant_id,id))`)
	require.NoError(t, err)
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+dst+`.purchase_reference_probe VALUES($1,$2)`, mid.UUID(), first.ID.UUID())
	require.NoError(t, err)
	for _, table := range []string{"payments", "subscriptions", "grants", "ledger_transfers", "provider_intents"} {
		var count int
		require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT count(*) FROM "+dst+"."+table).Scan(&count))
		require.Zero(t, count, "catalog restore cannot create billing effects: %s", table)
	}
	_, err = merchantarchive.RestoreCatalog(t.Context(), source, mid, bytes.NewReader(artifact.Bytes()))
	refusal(err, "not_empty")
	_, err = w.pool.Exec(t.Context(), "UPDATE "+dst+`.products SET display_name='Later edit' WHERE id=$1`, first.ProductID.UUID())
	require.NoError(t, err)
	result, err = restore(artifact.Bytes())
	require.NoError(t, err)
	require.True(t, result.Replayed)
	var title string
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT display_name FROM "+dst+".products WHERE id=$1", first.ProductID.UUID()).Scan(&title))
	require.Equal(t, "Later edit", title)
	// Normal host configuration recreates natural PSP IDs; customer provisioning
	// uses the public Client with the original host UUID. No raw PSP inserts are
	// required for this ordinary destination path.
	provisionedSchema := "catalog_provisioned_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, openrails.Migrate(t.Context(), w.pool, openrails.Config{Schema: provisionedSchema, River: openrails.RiverHostOwned}))
	provisionedName := pgx.Identifier{provisionedSchema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+provisionedName+" CASCADE") })
	provisioned, err := db.NewWithPGXPool(w.pool, provisionedSchema)
	require.NoError(t, err)
	directory, err := merchants.NewDirectoryService(provisioned.DataPool())
	require.NoError(t, err)
	_, _, err = directory.RegisterForRestore(t.Context(), mid, "catalog-provisioned")
	require.NoError(t, err)
	client, err := openrails.New(t.Context(), openrails.Config{
		Schema: provisionedSchema, River: openrails.RiverHostOwned, TestMode: openrails.Sandbox, ProviderWriteMode: openrails.ProviderWritesFull,
		Merchant: openrails.MerchantDeclaration{Slug: "catalog-provisioned", DisplayName: "Catalog destination", PSPs: w.psps},
	}, openrails.Deps{Postgres: w.pool, StripeTransport: w.stripe, NMITransport: w.nmi, Clock: w.clock})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close(context.Background()) })
	_, err = client.EnsureCustomer(t.Context(), buyer.cid(), billing.EnsureCustomerParams{})
	require.NoError(t, err)
	var provisionedPSP uuid.UUID
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT id FROM "+provisionedName+".psps WHERE merchant_id=$1 AND rail='nmi'", mid.UUID()).Scan(&provisionedPSP))
	require.Equal(t, w.psp["nmi"].UUID(), provisionedPSP)
	require.NoError(t, client.Close(t.Context()))
	_, err = merchantarchive.RestoreCatalog(t.Context(), provisioned, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err, "publicly provisioned prerequisites must satisfy restore")

	document.Tables["products"][0]["display_name"] = json.RawMessage(`"Different snapshot"`)
	var different bytes.Buffer
	require.NoError(t, merchantarchive.WriteCatalogSnapshot(&different, document))
	_, err = restore(different.Bytes())
	refusal(err, "not_empty")
}
