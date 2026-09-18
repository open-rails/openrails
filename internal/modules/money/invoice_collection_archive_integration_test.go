//go:build integration

package money_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestInvoiceCollectionArchivePreservesTerminalReplay(t *testing.T) {
	e := nmiReceiptScenario(t)
	var archive bytes.Buffer
	// The live invoice pointer is not portable while a provider answer is unknown.
	err := merchantarchive.Export(t.Context(), e.db, dbtest.TestMerchantID, &archive)
	require.Error(t, err)
	require.Empty(t, archive.Bytes())
	e.gateway.orderSale(e.op.String(), "txn_archive_collected")
	e.gateway.payment("txn_archive_collected", e.vault, "0.05", "USD")
	require.Equal(t, intents.StatusSucceeded, e.verify(t))
	e.requireSettledOnce(t)
	original, err := intents.NewStore(e.db).Get(e.ctx, e.op)
	require.NoError(t, err)
	require.NoError(t, merchantarchive.Export(t.Context(), e.db, dbtest.TestMerchantID, &archive))

	adminDSN, appDSN := dbtest.SharedRLSPostgres(t)
	const schema = "collected_invoice_archive"
	require.NoError(t, migrate.RunPostgres(t.Context(), &config.Config{DB: &config.DBConfig{URL: adminDSN, Schema: schema}}))
	target, err := db.NewDB(t.Context(), &config.DBConfig{URL: appDSN, Schema: schema})
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	_, err = target.Qx(t.Context()).Exec(t.Context(), `INSERT INTO openrails.merchants(id,slug) VALUES($1,'archive-invoice-destination')`, dbtest.TestMerchantID.UUID())
	require.NoError(t, err)
	_, err = merchantarchive.Restore(t.Context(), target, dbtest.TestMerchantID, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	ctx, release, err := target.WithMerchantConn(merchant.WithID(context.Background(), dbtest.TestMerchantID))
	require.NoError(t, err)
	defer release()
	restored := money.NewMoneyService(target)
	invoice, err := restored.GetInvoiceByID(ctx, e.payer, e.invoice)
	require.NoError(t, err)
	require.Equal(t, "paid", invoice.Status)
	require.Zero(t, invoice.AmountDue)
	// No provider adapter is installed: a terminal operation must replay locally.
	operation, err := (&intents.Runner{Store: intents.NewStore(target)}).ExecuteByID(ctx, e.op)
	require.NoError(t, err)
	require.Equal(t, original.Status, operation.Status)
	require.JSONEq(t, string(original.Payload), string(operation.Payload))
	require.JSONEq(t, string(original.ResultEvidence), string(operation.ResultEvidence))
	var settled, transfers int
	require.NoError(t, target.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.invoice_payments WHERE invoice_id=$1 AND status='settled'`, e.invoice).Scan(&settled))
	require.NoError(t, target.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, e.payer.UUID()).Scan(&transfers))
	require.Equal(t, 1, settled)
	require.Equal(t, 1, transfers)
	require.Equal(t, 1, e.gateway.sends)
}
