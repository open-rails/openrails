//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/modules/grants"
)

func TestEntitlementsHistoricalArchiveRetainsTimedPurchase(t *testing.T) {
	w := newWorld(t)
	client, buyer := w.client[embedded], w.newCustomer()
	product, err := client.CreateProduct(t.Context(), billing.CreateProductParams{
		Key: "historical-features", DisplayName: "Historical features", Entitlements: []string{"asset:timed", "service:permanent"},
	})
	require.NoError(t, err)
	price, err := client.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: "buy", UnitAmount: 1_000_000, Currency: "USD"})
	require.NoError(t, err)
	w.settle()
	w.stop()
	mid, payer, payment, pid := client.MerchantID(), buyer.customerID().UUID(), uuid.New(), product.ID.UUID()
	start := w.clock.Now().UTC().Truncate(time.Microsecond)
	end := start.Add(12 * time.Hour)
	// A retained manually recorded purchase already had these private historical
	// terms after migration. Its derived projections were never materialized.
	_, err = w.pool.Exec(t.Context(), w.q(`INSERT INTO billing.payments
		(merchant_id,id,customer_id,price_id,channel,transaction_id,amount,list_amount,currency,status,purchased_at,entitlements_snapshot,legacy_entitlement_hours,metadata)
		VALUES($1,$2,$3,$4,'manual','retained-one-time',1000000,1000000,'USD','completed',$5,
		'["asset:timed","service:permanent"]','{"asset:timed":12}','{"legacy_entitlement_hours":{"application-value":360}}')`), mid.UUID(), payment, payer, price.ID.UUID(), start)
	require.NoError(t, err)
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), mid)
	ledger := grants.New(source.Gen(ctx), mid.UUID())
	ledger.SetClock(w.clock.Now)
	for _, feature := range []struct {
		name string
		end  *time.Time
	}{{"asset:timed", &end}, {"service:permanent", nil}} {
		_, err := ledger.Grant(ctx, grants.GrantInput{Customer: payer, Product: &pid, Payment: &payment,
			Kind: grants.Entitlement, Source: grants.Purchase, SourceID: payment.String(),
			Spec: &grants.Spec{Entitlements: []string{feature.name}}, StartsAt: start, EndsAt: feature.end})
		require.NoError(t, err)
	}
	_, err = ledger.Grant(ctx, grants.GrantInput{Customer: payer, Product: &pid, Payment: &payment,
		Kind: grants.Ownership, Source: grants.Purchase, SourceID: payment.String(), StartsAt: start})
	require.NoError(t, err)
	var current bytes.Buffer
	require.NoError(t, merchantarchive.Export(ctx, source, mid, &current))

	// Emit the preceding positional archive representation, retaining every
	// original financial fact and the immutable 12-hour/permanent grant windows.
	profiles := map[string]contract.Profile{}
	for _, profile := range contract.Profiles {
		profiles[profile.Name] = profile
	}
	var old bytes.Buffer
	var writer *archivewire.Writer
	var table string
	_, err = archivewire.Read(bytes.NewReader(current.Bytes()), func(h archivewire.Header) error {
		var err error
		writer, err = archivewire.NewWriter(&old, h.MerchantID, h.CatalogRevision)
		return err
	}, func(record archivewire.Record) error {
		if record.Kind == "table" {
			table = record.Table
			return writer.Table(table)
		}
		values := append([]*string(nil), record.Values...)
		for i, field := range profiles[table].Columns {
			if table == "products" && field.Name == "entitlements" || table == "payments" && field.Name == "entitlements_snapshot" {
				values[i] = new(`{"asset:timed":12,"service:permanent":null}`)
			}
			if table == "payments" && field.Name == "legacy_entitlement_hours" {
				values = values[:i]
				break
			}
		}
		return writer.Row(values)
	})
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	original, err := archivewire.Read(bytes.NewReader(old.Bytes()), nil, nil)
	require.NoError(t, err)

	schema := "entitlement_archive_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	require.NoError(t, openrails.Migrate(t.Context(), w.pool, openrails.Config{Schema: schema, River: openrails.RiverHostOwned}))
	quoted := pgx.Identifier{schema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	_, err = w.pool.Exec(t.Context(), "INSERT INTO "+quoted+".merchants(id,slug,status) VALUES($1,'historical-entitlements','active')", mid.UUID())
	require.NoError(t, err)
	target, err := db.NewWithPGXPool(w.pool, schema)
	require.NoError(t, err)
	result, err := merchantarchive.Restore(ctx, target, mid, bytes.NewReader(old.Bytes()))
	require.NoError(t, err)
	require.Equal(t, original.Digest, result.Digest)
	var names, historical, metadata string
	require.NoError(t, w.pool.QueryRow(t.Context(), "SELECT entitlements_snapshot::text,legacy_entitlement_hours::text,metadata::text FROM "+quoted+".payments WHERE id=$1", payment).Scan(&names, &historical, &metadata))
	require.JSONEq(t, `["asset:timed","service:permanent"]`, names)
	require.JSONEq(t, `{"asset:timed":12}`, historical)
	require.JSONEq(t, `{"legacy_entitlement_hours":{"application-value":360}}`, metadata)
	rows, err := target.Gen(ctx).ListOriginalPurchaseGrants(ctx, gen.ListOriginalPurchaseGrantsParams{MerchantID: mid.UUID(), PaymentID: payment, RowLimit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 3)
	for range 2 {
		for _, grant := range rows {
			require.NoError(t, grants.New(target.Gen(ctx), mid.UUID()).MaterializeGrant(ctx, grant))
		}
	}
	for _, check := range []struct {
		at    time.Time
		timed bool
	}{{start.Add(time.Hour), true}, {end, false}, {start.Add(1000 * time.Hour), false}} {
		access, err := target.Gen(ctx).CheckResourceEntitlements(ctx, gen.CheckResourceEntitlementsParams{
			MerchantID: mid.UUID(), CustomerID: payer, AtTime: check.at, Entitlements: []string{"asset:timed", "service:permanent"},
		})
		require.NoError(t, err)
		require.Len(t, access, 2)
		require.Equal(t, check.timed, access[0].HasAccess, "restoring or repairing cannot make the historical rental permanent")
		require.True(t, access[1].HasAccess)
	}
	replayed, err := merchantarchive.Restore(ctx, target, mid, bytes.NewReader(old.Bytes()))
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, original.Digest, replayed.Digest)
}
