//go:build integration

package money_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
)

func TestInvoiceCollectionRoundingConservesCustomerFunds(t *testing.T) {
	for _, tc := range []struct {
		name, currency, providerAmount string
		due, charged, excess           int64
		failSettlement                 bool
	}{
		{"exact", "USD", "3.00", 3_000_000, 3_000_000, 0, false},
		{"fractional yen", "JPY", "4.00", 30_001, 40_000, 9_999, false},
		{"fractional dollar", "USD", "3.01", 3_000_001, 3_010_000, 9_999, false},
		{"fractional amount with rollback", "USD", "4.01", 4_004_999, 4_010_000, 5_001, true},
		{"rounding overflow", "USD", "", math.MaxInt64, 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mid := merchant.ID(uuid.New())
			database := dbtest.OpenMerchantDB(t, mid.UUID())
			pool, ctx := database.Pool(), merchant.WithID(t.Context(), mid)
			_, err := pool.Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, mid.UUID(), "rounding-"+mid.String())
			require.NoError(t, err)
			svc := money.NewMoneyService(database)
			payer := identity.CustomerIDFromString(uuid.NewString())
			msvc := merchantsServiceForTest(t, database)
			psp, method := uuid.New(), uuid.New()
			account := "gw-rounding-" + psp.String()
			_, err = pool.Exec(ctx, `INSERT INTO openrails.customers(merchant_id,id) VALUES($1,$2)`, mid.UUID(), payer.UUID())
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO openrails.psps(merchant_id,id,rail,environment,account_id) VALUES($1,$2,'nmi','test',$3)`, mid.UUID(), psp, account)
			require.NoError(t, err)
			_, err = pool.Exec(ctx, `INSERT INTO openrails.payment_methods(merchant_id,id,customer_id,psp_id,rail,rail_customer_ref,rail_method_ref,initial_transaction_id,stored_credential_unscheduled_ref) VALUES($1,$2,$3,$4,'nmi',$5,'billing','initial','approved-unscheduled')`, mid.UUID(), method, payer.UUID(), psp, "vault_"+method.String())
			require.NoError(t, err)
			secret, err := merchants.PSPSecretName("nmi", "test", account, "security_key")
			require.NoError(t, err)
			_, err = msvc.Secrets().Put(ctx, mid, secret, "synthetic-rounding-key")
			require.NoError(t, err)
			_, err = svc.UpsertAccountSettings(ctx, payer, tc.currency, money.AccountSettingsInput{BillingMode: strptr(money.BillingModeArrears)})
			require.NoError(t, err)
			require.NoError(t, svc.SetInvoiceCollectionPaymentMethod(ctx, payer, tc.currency, method))
			_, err = svc.AccrueOwed(ctx, payer, tc.currency, "usage", uuid.NewString(), tc.due)
			require.NoError(t, err)
			invoice, err := svc.FinalizeInvoice(ctx, payer, tc.currency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
			require.NoError(t, err)
			t.Cleanup(func() {
				// Fleet-worker tests share this package database. The deliberate overflow
				// refusal is not scheduled work for a later, unrelated test's worker.
				_, err := pool.Exec(context.WithoutCancel(ctx), `UPDATE openrails.invoices SET collection_method='send_invoice' WHERE merchant_id=$1 AND id=$2`, mid.UUID(), invoice.ID)
				require.NoError(t, err)
			})
			gateway, server := newFakeNMIReceiptGateway(t)
			plane := &money.MerchantCollectionAdapterBuilder{Config: storeCollectionTestConfig(), DB: database, MerchantsFn: func() *merchants.Service { return msvc },
				Endpoints: money.CollectionEndpoints{NMIDirectPostURL: server.URL, NMIQueryURL: server.URL, NMIV5BaseURL: server.URL}}
			charger := money.NewScopedCharger(database, nil)
			charger.SetAdapterResolver(plane)
			runner := collectionRunner(database, charger, plane)
			trigger := "rounding_" + uuid.NewString()[:8]
			if tc.failSettlement {
				// Fail after invoice/ledger/credit writes, while settling the attempt.
				admin := dbtest.SharedSuperuserPGXPool(t)
				_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION openrails.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
				 IF NEW.invoice_id = '%s'::uuid AND NEW.status='settled' THEN RAISE EXCEPTION 'injected rounding settlement failure'; END IF;
				 RETURN NEW; END $$; CREATE TRIGGER %s BEFORE UPDATE ON openrails.invoice_payments FOR EACH ROW EXECUTE FUNCTION openrails.%s()`, trigger, invoice.ID, trigger, trigger))
				require.NoError(t, err)
				t.Cleanup(func() {
					_, _ = admin.Exec(context.WithoutCancel(ctx), "DROP FUNCTION IF EXISTS openrails."+trigger+"() CASCADE")
				})
			}
			n, err := svc.ChargeOutstanding(ctx, runner, 0)
			if tc.charged != 0 {
				require.Equal(t, []string{tc.providerAmount}, gateway.saleAmounts)
				require.Equal(t, []string{tc.currency}, gateway.saleCurrencies)
			}

			if tc.charged == 0 {
				require.ErrorContains(t, err, "rounded charge is not representable")
				require.Zero(t, n)
				require.Zero(t, gateway.sends, "unrepresentable native receipt refuses before submission")
				attempts, total, err := svc.ListInvoicePaymentAttempts(ctx, payer, invoice.ID, 10, 0)
				require.NoError(t, err)
				require.Zero(t, total)
				require.Empty(t, attempts)
				return
			}
			require.NoError(t, err)
			op := latestCollectionIntent(t, pool, ctx, invoice.ID)
			require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
			gateway.orderSale(op.ID.String(), "txn_rounding")
			gateway.payment("txn_rounding", "vault_"+method.String(), tc.providerAmount, tc.currency)
			dueNow(t, pool, ctx, op.ID)
			_, err = runner.RunVerifyOnce(ctx)
			require.NoError(t, err)
			if tc.failSettlement {
				require.Zero(t, n)
				require.Equal(t, intents.StatusUnknownNeedsVerify, op.Status)
				balance, err := svc.GetBalanceForCustomer(ctx, payer, tc.currency)
				require.NoError(t, err)
				require.Zero(t, balance.Balance, "rounding credit rolls back with settlement")
				current, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
				require.NoError(t, err)
				require.Equal(t, tc.due, current.AmountDue)
				var transfers int
				require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM openrails.ledger_transfers WHERE customer_id=$1 AND transfer_type IN ('owed_payment','deposit')`, payer.UUID()).Scan(&transfers))
				require.Zero(t, transfers)
				admin := dbtest.SharedSuperuserPGXPool(t)
				_, err = admin.Exec(ctx, "DROP TRIGGER "+trigger+" ON openrails.invoice_payments")
				require.NoError(t, err)
				dueNow(t, pool, ctx, op.ID)
				_, err = collectionRunner(database, charger, plane).RunVerifyOnce(ctx)
				require.NoError(t, err)
			}
			current, err := svc.GetInvoiceByID(ctx, payer, invoice.ID)
			require.NoError(t, err)
			require.Equal(t, "paid", current.Status)
			require.Equal(t, tc.due, current.AmountPaid)
			require.Zero(t, current.AmountDue)
			attempts, total, err := svc.ListInvoicePaymentAttempts(ctx, payer, invoice.ID, 10, 0)
			require.NoError(t, err)
			require.Equal(t, 1, total)
			require.Equal(t, tc.charged, attempts[0].Amount, "history records the actual provider charge")
			balance, err := svc.GetBalanceForCustomer(ctx, payer, tc.currency)
			require.NoError(t, err)
			require.Equal(t, tc.excess, balance.Balance)
			var settled int64
			require.NoError(t, pool.QueryRow(ctx, `SELECT sum(amount)::bigint FROM openrails.ledger_transfers WHERE customer_id=$1 AND transfer_type IN ('owed_payment','deposit')`, payer.UUID()).Scan(&settled))
			require.Equal(t, tc.charged, settled, "actual charge = debt paid + customer credit")
			_, err = runner.ExecuteByID(ctx, op.ID)
			require.NoError(t, err)
			require.Equal(t, 1, gateway.sends)
			balance, err = svc.GetBalanceForCustomer(ctx, payer, tc.currency)
			require.NoError(t, err)
			require.Equal(t, tc.excess, balance.Balance, "replay never credits twice")
			if tc.excess > 0 {
				key := uuid.New()
				_, err = svc.Withdraw(ctx, money.WithdrawParams{CustomerID: &payer, Currency: tc.currency, Amount: tc.excess, Source: "rounding-proof", SourceID: &key})
				require.NoError(t, err, "rounding surplus is a spendable lot, not only a balance counter")
				balance, err = svc.GetBalanceForCustomer(ctx, payer, tc.currency)
				require.NoError(t, err)
				require.Zero(t, balance.Balance)
			}
		})
	}
}
