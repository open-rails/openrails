//go:build integration

package money_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestInvoiceCollectionArchivePreservesTerminalReplay(t *testing.T) {
	for _, resolution := range []string{"verifier", "operator"} {
		t.Run(resolution, func(t *testing.T) { testInvoiceCollectionArchive(t, resolution) })
	}
}

func testInvoiceCollectionArchive(t *testing.T, resolution string) {
	// A whole-book export must not inherit the package's shared merchant: other
	// tests legitimately leave unsupported operation evidence on that merchant.
	mid := merchant.ID(uuid.New())
	database := dbtest.OpenMerchantDB(t, mid.UUID())
	ctx := merchant.WithID(t.Context(), mid)
	_, err := database.Pool().Exec(ctx, `INSERT INTO billing.merchants(id,slug) VALUES($1,$2)`, mid.UUID(), "archive-"+mid.String())
	require.NoError(t, err)
	payer, method, psp := identity.CustomerIDFromString(uuid.NewString()), uuid.New(), uuid.New()
	account := "archive-" + uuid.NewString()
	_, err = database.Pool().Exec(ctx, `INSERT INTO billing.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), payer.UUID())
	require.NoError(t, err)
	_, err = database.Pool().Exec(ctx, `INSERT INTO billing.psps(merchant_id,id,rail,environment,account_id) VALUES($1,$2,'nmi','test',$3)`, mid.UUID(), psp, account)
	require.NoError(t, err)
	vault := "archive-original-vault"
	_, err = database.Pool().Exec(ctx, `INSERT INTO billing.payment_methods(merchant_id,id,customer_id,psp_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id,stored_credential_unscheduled_ref) VALUES($1,$2,$3,$4,'nmi',$5,'billing-original','initial-original','approved-unscheduled')`, mid.UUID(), method, payer.UUID(), psp, vault)
	require.NoError(t, err)
	msvc := merchantsServiceForTest(t, database)
	secret, err := merchants.PSPSecretName("nmi", "test", account, "security_key")
	require.NoError(t, err)
	_, err = msvc.Secrets().Put(ctx, mid, secret, "synthetic-key")
	require.NoError(t, err)
	svc := money.NewMoneyService(database)
	invoiceID := seedArrearsInvoice(t, svc, ctx, payer, method)
	gateway, server := newFakeNMIReceiptGateway(t)
	plane := &money.MerchantCollectionAdapterBuilder{Config: storeCollectionTestConfig(), DB: database, MerchantsFn: func() *merchants.Service { return msvc },
		Endpoints: money.CollectionEndpoints{NMIDirectPostURL: server.URL, NMIQueryURL: server.URL, NMIV5BaseURL: server.URL}}
	charger := money.NewScopedCharger(database, nil)
	charger.SetAdapterResolver(plane)
	e := nmiReceiptEnv{collectionEnv: collectionEnv{svc: svc, db: database, pool: database.Pool(), payer: payer, currency: "USD", method: method, invoice: invoiceID, ctx: ctx},
		gateway: gateway, plane: plane, runner: collectionRunner(database, charger, plane), merchants: msvc, vault: vault}
	e.collectUncertain(t)
	var archive bytes.Buffer
	// The live invoice pointer is not portable while a provider answer is unknown.
	err = merchantarchive.Export(t.Context(), e.db, mid, &archive)
	require.Error(t, err)
	require.Empty(t, archive.Bytes())
	e.gateway.orderSale(e.op.String(), "txn_archive_collected")
	e.gateway.payment("txn_archive_collected", e.vault, "0.05", "USD")
	if resolution == "operator" {
		require.NoError(t, e.resolve(t, "txn_archive_collected"))
	} else {
		require.Equal(t, intents.StatusSucceeded, e.verify(t))
	}
	e.requireSettledOnce(t)
	original, err := intents.NewStore(e.db).Get(e.ctx, e.op)
	require.NoError(t, err)
	require.NoError(t, merchantarchive.Export(t.Context(), e.db, mid, &archive))

	adminDSN, appDSN := dbtest.SharedRLSPostgres(t)
	schema := "collected_invoice_archive_" + resolution
	require.NoError(t, migrate.RunPostgres(t.Context(), &config.Config{DB: &config.DBConfig{URL: adminDSN, Schema: schema}}))
	target, err := db.NewDB(t.Context(), &config.DBConfig{URL: appDSN, Schema: schema})
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	_, err = target.Qx(t.Context()).Exec(t.Context(), `INSERT INTO openrails.merchants(id,slug) VALUES($1,'archive-invoice-destination')`, mid.UUID())
	require.NoError(t, err)
	_, err = merchantarchive.Restore(t.Context(), target, mid, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	ctx, release, err := target.WithMerchantConn(merchant.WithID(context.Background(), mid))
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
	receipt, found, err := intents.LoadCollectedReceipt(operation)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, receipt.Validate(operation))

	var settled, transfers int
	require.NoError(t, target.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.invoice_payments WHERE invoice_id=$1 AND status='settled'`, e.invoice).Scan(&settled))
	require.NoError(t, target.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.ledger_transfers WHERE customer_id=$1 AND transfer_type='owed_payment'`, e.payer.UUID()).Scan(&transfers))
	require.Equal(t, 1, settled)
	require.Equal(t, 1, transfers)
	require.Equal(t, 1, e.gateway.sends)
}
