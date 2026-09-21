//go:build integration

package money_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/open-rails/openrails/internal/archivewire"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
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
	for _, resolution := range []string{"verifier", "operator", "customer", "admin_retry"} {
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
	if resolution == "customer" {
		result, err := e.svc.PayInvoiceNow(e.ctx, e.runner, e.payer, money.InvoiceCollectionRetryRequest{InvoiceID: e.invoice, PaymentMethodID: e.method, IdempotencyKey: "archive-key-1461"})
		require.NoError(t, err)
		require.Equal(t, intents.StatusUnknownNeedsVerify, result.Operation.Status)
		e.op = result.Operation.ID
	} else if resolution == "admin_retry" {
		_, err := e.svc.MarkInvoiceUncollectible(e.ctx, e.payer, e.invoice)
		require.NoError(t, err)
		result, err := e.svc.RetryInvoiceCollection(e.ctx, e.runner, e.payer, money.InvoiceCollectionRetryRequest{InvoiceID: e.invoice, PaymentMethodID: e.method, IdempotencyKey: "admin-archive-3817"})
		require.NoError(t, err)
		require.Equal(t, intents.StatusUnknownNeedsVerify, result.Operation.Status)
		e.op = result.Operation.ID
	} else {
		e.collectUncertain(t)
	}
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
	if resolution == "customer" {
		accepted, err := intents.DecodeInvoiceCollectionPayload(original)
		require.NoError(t, err)
		for _, tc := range []struct {
			name, column, wire string
			changed, original  any
		}{
			{"orphan key", "idempotency_key", charge.CustomerPaymentKey("invoice_collection", payer.UUID(), "orphan"), charge.CustomerPaymentKey("invoice_collection", payer.UUID(), "orphan"), original.IdempotencyKey},
			{"wrong amount", "amount", "50001", int64(50001), int64(50000)},
			{"caller PAN text", "failure_message", "4111111111111111", "4111111111111111", nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				// The source mutation and altered artifact represent the same bad
				// row. Both export and restore must refuse it; recompute the footer
				// so the restore proof reaches semantic identity validation.
				query := fmt.Sprintf("UPDATE billing.invoice_payments SET %s=$2 WHERE id=$1", tc.column)
				_, err := database.Pool().Exec(e.ctx, query, accepted.AttemptID, tc.changed)
				require.NoError(t, err)
				defer func() {
					_, err := database.Pool().Exec(e.ctx, query, accepted.AttemptID, tc.original)
					require.NoError(t, err)
				}()
				var refused bytes.Buffer
				require.Error(t, merchantarchive.Export(t.Context(), database, mid, &refused))
				_, err = merchantarchive.Restore(t.Context(), target, mid, bytes.NewReader(rewriteCollectionArchive(t, archive.Bytes(), "invoice_payments", tc.column, tc.wire)))
				require.Error(t, err)
				var count int
				require.NoError(t, target.Qx(t.Context()).QueryRow(t.Context(), `SELECT count(*) FROM openrails.customers WHERE merchant_id=$1`, mid.UUID()).Scan(&count))
				require.Zero(t, count, "refused restore rolls back every row")
			})
		}
		t.Run("malformed accepted amount", func(t *testing.T) {
			const canary = "invalid-amount-4111111111111111"
			var payload map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(original.Payload, &payload))
			payload["amount"], err = json.Marshal(canary)
			require.NoError(t, err)
			malformed, err := json.Marshal(payload)
			require.NoError(t, err)
			_, err = database.Pool().Exec(e.ctx, `UPDATE billing.rail_intents SET payload=$2 WHERE id=$1`, original.ID, malformed)
			require.NoError(t, err)
			defer func() {
				_, err := database.Pool().Exec(e.ctx, `UPDATE billing.rail_intents SET payload=$2 WHERE id=$1`, original.ID, original.Payload)
				require.NoError(t, err)
			}()
			var refused bytes.Buffer
			exportErr := merchantarchive.Export(t.Context(), database, mid, &refused)
			require.Error(t, exportErr)
			var classified *merchantarchive.Error
			require.True(t, errors.As(exportErr, &classified))
			require.Equal(t, "unsupported_state", classified.Code, "the canonical decoder must refuse before any SQL numeric cast")
			require.NotContains(t, exportErr.Error(), canary)
			_, restoreErr := merchantarchive.Restore(t.Context(), target, mid, bytes.NewReader(rewriteCollectionArchive(t, archive.Bytes(), "rail_intents", "payload", string(malformed))))
			require.Error(t, restoreErr)
			require.NotContains(t, restoreErr.Error(), canary)
		})

	}
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

func rewriteCollectionArchive(t *testing.T, raw []byte, table, column, value string) []byte {
	t.Helper()
	var result bytes.Buffer
	var writer *archivewire.Writer
	current, changed := "", false
	_, err := archivewire.Read(bytes.NewReader(raw), func(header archivewire.Header) error {
		var err error
		writer, err = archivewire.NewWriter(&result, header.MerchantID)
		return err
	}, func(record archivewire.Record) error {
		if record.Kind == "table" {
			current = record.Table
			return writer.Table(current)
		}
		if current == table {
			for _, profile := range contract.Profiles {
				if profile.Name != table {
					continue
				}
				for i, field := range profile.Columns {
					if field.Name == column {
						record.Values[i] = &value
						changed = true
					}
				}
			}
		}
		return writer.Row(record.Values)
	})
	require.NoError(t, err)
	require.True(t, changed)
	require.NoError(t, writer.Close())
	return result.Bytes()
}
