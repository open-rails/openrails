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
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/engine"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/modules/orders/observed"
)

// Restoring the local book preserves observed facts and replay coordinates. No
// provider adapter is installed on the destination, and replay moves no money.
func TestProviderBillArchiveRestoresIdentityAndMultiplePayments(t *testing.T) {
	w := newWorld(t)
	w.waive("evidenced", "normalized provider money fixtures qualify offline archive and replay, not a gateway sale")
	c := w.newCustomer()
	price := w.lifetime("archive-provider-goods", 100)
	priceID, productID := w.priceStorageID(price.ID), price.ProductID.UUID()
	mid := w.client[embedded].MerchantID()
	ctx := merchant.WithID(t.Context(), mid)
	source, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	bill := observed.Bill{PSPID: w.psp["nmi"].UUID(), CustomerID: c.cid().UUID(), Owner: observed.OwnerProvider,
		Kind: "purchase", SourceKind: "verified_invoice", SourceKey: "archive-invoice", ProviderReference: "archive-invoice",
		Currency: "USD", Total: 100, Lines: []observed.Line{{PriceID: &priceID, ProductID: &productID, Description: "Accepted goods", Quantity: new(1), UnitAmount: 100, Amount: 100}},
		Settled: true, SettlementSource: "cash"}
	bill.Payment = &models.Payment{ID: uuid.New(), CustomerID: bill.CustomerID, Rail: models.RailNMI, PspID: &bill.PSPID,
		TransactionID: "archive-capture", Amount: 100, ListAmount: 100, Currency: "USD", Status: "succeeded", MoneyMovement: models.MoneyMovementRail}
	record := func(database *db.DB, input observed.Bill) (out observed.Result, err error) {
		err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			out, e = observed.Record(ctx, database.NewWithPgxTx(tx), input, w.clock.Now())
			return e
		})
		return
	}
	original, err := record(source, bill)
	require.NoError(t, err)
	require.True(t, original.Completed)
	extra := bill
	extra.Payment = &models.Payment{ID: uuid.New(), CustomerID: bill.CustomerID, Rail: models.RailNMI, PspID: &bill.PSPID,
		TransactionID: "archive-extra-capture", Amount: 25, ListAmount: 100, Currency: "USD", Status: "succeeded", MoneyMovement: models.MoneyMovementRail}
	second, err := record(source, extra)
	require.NoError(t, err)
	require.True(t, second.PaymentCreated)
	require.False(t, second.Completed)
	w.settle()
	events, err := w.client[embedded].ListHostEvents(ctx, billing.HostEventListParams{})
	require.NoError(t, err)
	for _, event := range events.Items {
		_, err := w.client[embedded].AcknowledgeHostEvents(ctx, []billing.HostEventID{event.ID})
		require.NoError(t, err)
	}
	w.stop()
	charges, setups := w.providerWrites("nmi")
	var artifact bytes.Buffer
	require.NoError(t, merchantarchive.Export(ctx, source, mid, &artifact))
	destinationSchema := "provider_archive_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	require.NoError(t, engine.Migrate(ctx, w.pool, openrails.Config{Database: openrails.DatabaseConfig{Schema: destinationSchema, RiverSchema: destinationSchema}}))
	quoted := pgx.Identifier{destinationSchema}.Sanitize()
	t.Cleanup(func() { _, _ = w.pool.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE") })
	_, err = w.pool.Exec(ctx, "INSERT INTO "+quoted+`.merchants (id,slug,status,permission_group_id) VALUES ($1,'archive-destination','active',$2)`, mid.UUID(), uuid.New())
	require.NoError(t, err)
	destination, err := db.NewWithPGXPool(w.pool, destinationSchema)
	require.NoError(t, err)

	// A correct digest does not make a different accepted customer/price safe.
	for _, field := range []string{"CustomerID", "PriceID"} {
		tampered := rewriteProviderArchive(t, artifact.Bytes(), func(snapshot map[string]any) {
			if field == "CustomerID" {
				snapshot[field] = uuid.NewString()
			} else {
				snapshot["Lines"].([]any)[0].(map[string]any)[field] = uuid.NewString()
			}
		})
		_, err = merchantarchive.Restore(ctx, destination, mid, bytes.NewReader(tampered))
		require.Error(t, err, field)
		var count int
		require.NoError(t, w.pool.QueryRow(ctx, "SELECT count(*) FROM "+quoted+".orders").Scan(&count))
		require.Zero(t, count, "tampered reference must roll the entire restore back")
	}
	result, err := merchantarchive.Restore(ctx, destination, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err, "archive restore: %v", errors.Unwrap(err))
	require.False(t, result.Replayed)
	for _, table := range []string{"orders", "order_lines", "payments", "provider_bills"} {
		read := func(schema string) string {
			var value string
			require.NoError(t, w.pool.QueryRow(ctx, fmt.Sprintf("SELECT jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text)::text FROM %s.%s t WHERE merchant_id=$1", pgx.Identifier{schema}.Sanitize(), table), mid.UUID()).Scan(&value))
			return value
		}
		require.Equal(t, read(w.schema), read(destinationSchema), table)
	}
	replayed, err := record(destination, bill)
	require.NoError(t, err)
	require.Equal(t, original.Order.ID, replayed.Order.ID)
	require.False(t, replayed.Created || replayed.PaymentCreated || replayed.Completed)
	replayed, err = record(destination, extra)
	require.NoError(t, err)
	require.False(t, replayed.Created || replayed.PaymentCreated || replayed.Completed)
	var payments int
	require.NoError(t, w.pool.QueryRow(ctx, "SELECT count(*) FROM "+quoted+".payments WHERE order_id=$1", original.Order.ID).Scan(&payments))
	require.Equal(t, 2, payments)
	retry, err := merchantarchive.Restore(ctx, destination, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.True(t, retry.Replayed)
	gotCharges, gotSetups := w.providerWrites("nmi")
	require.Equal(t, charges, gotCharges)
	require.Equal(t, setups, gotSetups)
}

func rewriteProviderArchive(t *testing.T, input []byte, edit func(map[string]any)) []byte {
	t.Helper()
	var output bytes.Buffer
	var writer *archivewire.Writer
	var table string
	_, err := archivewire.Read(bytes.NewReader(input), func(h archivewire.Header) error {
		var e error
		writer, e = archivewire.NewWriter(&output, h.MerchantID, h.CatalogRevision)
		return e
	}, func(record archivewire.Record) error {
		if record.Kind == "table" {
			table = record.Table
			return writer.Table(table)
		}
		if table == "provider_bills" {
			for _, profile := range contract.Profiles {
				if profile.Name != table {
					continue
				}
				for i, col := range profile.Columns {
					if col.Name != "snapshot" {
						continue
					}
					var snapshot map[string]any
					d := json.NewDecoder(strings.NewReader(*record.Values[i]))
					d.UseNumber()
					require.NoError(t, d.Decode(&snapshot))
					edit(snapshot)
					raw, err := json.Marshal(snapshot)
					require.NoError(t, err)
					record.Values[i] = new(string(raw))
				}
			}
		}
		return writer.Row(record.Values)
	})
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return output.Bytes()
}
