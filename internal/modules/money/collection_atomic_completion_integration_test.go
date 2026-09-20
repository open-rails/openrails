//go:build integration

package money_test

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/modules/money"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/stretchr/testify/require"
)

func TestCollectionSettlementAndTerminalCommitTogether(t *testing.T) {
	e := nmiReceiptScenario(t)
	e.gateway.orderSale(e.op.String(), "atomic-charge")
	e.gateway.payment("atomic-charge", e.vault, "0.05", e.currency)
	admin := dbtest.SharedSuperuserPGXPool(t)
	trigger := "atomic_terminal_" + uuid.NewString()[:8]
	_, err := admin.Exec(e.ctx, fmt.Sprintf(`CREATE FUNCTION openrails.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF NEW.id='%s'::uuid AND NEW.status='succeeded' THEN RAISE EXCEPTION 'injected terminal commit failure'; END IF;
 RETURN NEW; END $$; CREATE TRIGGER %s BEFORE UPDATE ON openrails.rail_intents FOR EACH ROW EXECUTE FUNCTION openrails.%s()`, trigger, e.op, trigger, trigger))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(context.WithoutCancel(e.ctx), "DROP FUNCTION IF EXISTS openrails."+trigger+"() CASCADE")
	})
	dueNow(t, e.pool, e.ctx, e.op)
	stats, err := e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Zero(t, stats.Succeeded)
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	require.Equal(t, []string{"attempted"}, e.attemptStatuses(t))
	require.Zero(t, e.owedPaymentTransfers(t))
	invoice := e.invoiceRow(t)
	require.EqualValues(t, 50000, invoice.AmountDue)
	require.NotNil(t, invoice.CollectionIntentID)
	_, found, err := intents.LoadCollectedReceipt(latestCollectionIntent(t, e.pool, e.ctx, e.invoice))
	require.NoError(t, err)
	require.True(t, found)
	_, err = admin.Exec(e.ctx, "DROP FUNCTION openrails."+trigger+"() CASCADE")
	require.NoError(t, err)
	// The provider is no longer available to recovery; receipt custody suffices.
	e.gateway.mu.Lock()
	e.gateway.saleForOrder = map[string]string{}
	e.gateway.payments = map[string]map[string]any{}
	e.gateway.mu.Unlock()
	dueNow(t, e.pool, e.ctx, e.op)
	stats, err = e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Succeeded)
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	e.requireSettledOnce(t)
	require.Len(t, e.gateway.sentOrderIDs(), 1)
	// Generic terminal mutation is not an alternate completion door.
	require.Error(t, intents.NewStore(e.db).MarkFailedTerminal(e.ctx, e.op, "late refusal", nil))
}

func TestCollectionRefusalOutboxAndTerminalCommitTogether(t *testing.T) {
	for _, failure := range []string{"notification", "terminal"} {
		t.Run(failure, func(t *testing.T) {
			e := newNMIReceiptEnv(t)
			e.gateway.saleResponse = "response=2&responsetext=DECLINED&response_code=201"
			initialNotifications := 0
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM openrails.notifications WHERE customer_id=$1`, e.payer.UUID()).Scan(&initialNotifications))

			admin := dbtest.SharedSuperuserPGXPool(t)
			trigger := "atomic_refusal_" + uuid.NewString()[:8]
			table, event, condition := "notifications", "INSERT", fmt.Sprintf("NEW.customer_id='%s'::uuid", e.payer.UUID())
			if failure == "terminal" {
				table = "rail_intents"
				event = "UPDATE"
				condition = fmt.Sprintf("NEW.intent_type='invoice_collection' AND NEW.payload->>'invoice_id'='%s' AND NEW.status='failed_terminal'", e.invoice)
			}
			_, err := admin.Exec(e.ctx, fmt.Sprintf(`CREATE FUNCTION openrails.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 IF %s THEN RAISE EXCEPTION 'injected refusal completion failure'; END IF;
 RETURN NEW; END $$; CREATE TRIGGER %s BEFORE %s ON openrails.%s FOR EACH ROW EXECUTE FUNCTION openrails.%s()`, trigger, condition, trigger, event, table, trigger))
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = admin.Exec(context.WithoutCancel(e.ctx), "DROP FUNCTION IF EXISTS openrails."+trigger+"() CASCADE")
			})
			_, err = e.svc.ChargeOutstanding(e.ctx, e.runner, 0)
			require.NoError(t, err)
			operation := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
			e.op = operation.ID
			require.Equal(t, intents.StatusUnknownNeedsVerify, operation.Status)
			require.Equal(t, []string{"attempted"}, e.attemptStatuses(t))
			require.EqualValues(t, 0, e.invoiceRow(t).CollectionFailureCount)
			var notifications int
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM openrails.notifications WHERE customer_id=$1`, e.payer.UUID()).Scan(&notifications))
			require.Equal(t, initialNotifications, notifications)
			_, err = admin.Exec(e.ctx, "DROP FUNCTION openrails."+trigger+"() CASCADE")
			require.NoError(t, err)
			dueNow(t, e.pool, e.ctx, e.op)
			stats, err := e.runner.RunVerifyOnce(e.ctx)
			require.NoError(t, err)
			require.Equal(t, 1, stats.Terminal)
			require.Equal(t, intents.StatusFailedTerminal, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
			require.Equal(t, []string{"failed"}, e.attemptStatuses(t))
			require.NoError(t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM openrails.notifications WHERE customer_id=$1`, e.payer.UUID()).Scan(&notifications))
			require.Equal(t, initialNotifications+1, notifications)
			require.Len(t, e.gateway.sentOrderIDs(), 1, "the parsed refusal replays locally without another provider charge")
		})
	}
}

func TestConcurrentCollectionCompletionReplaysOneTerminalResult(t *testing.T) {
	e := nmiReceiptScenario(t)
	e.gateway.orderSale(e.op.String(), "concurrent-charge")
	e.gateway.payment("concurrent-charge", e.vault, "0.05", e.currency)
	operation := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	receipt, found, err := e.plane.ReadCollectionReceipt(e.ctx, operation, "concurrent-charge")
	require.NoError(t, err)
	require.True(t, found)
	_, err = intents.NewStore(e.db).RetainCollectedReceipt(e.ctx, operation, receipt)
	require.NoError(t, err)
	operation = latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	handler := money.NewInvoiceCollectionHandler(e.db, nil, e.plane, fullModeConfig(), nil)
	results := make(chan intents.Outcome, 2)
	for range 2 {
		go func() { results <- handler.Verify(e.ctx, operation) }()
	}
	for range 2 {
		outcome := <-results
		require.Equal(t, intents.OutcomeSucceeded, outcome.Class, outcome.Reason)
	}
	require.Equal(t, intents.StatusSucceeded, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	e.requireSettledOnce(t)
	require.Len(t, e.gateway.sentOrderIDs(), 1)
}

func TestCollectionCompletionRejectsNonExecutionUnderItsTransactionLock(t *testing.T) {
	e := nmiReceiptScenario(t)
	e.gateway.orderSale(e.op.String(), "known-charge")
	e.gateway.payment("known-charge", e.vault, "0.05", e.currency)
	operation := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
	receipt, found, err := e.plane.ReadCollectionReceipt(e.ctx, operation, "known-charge")
	require.NoError(t, err)
	require.True(t, found)
	_, err = intents.NewStore(e.db).RetainCollectedReceipt(e.ctx, operation, receipt)
	require.NoError(t, err)
	// Bypass the earlier handler check deliberately: the transaction's terminal
	// gate itself must reject refusal and roll back all preceding local effects.
	err = e.db.MerchantTx(e.ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE openrails.invoices SET status='voided',amount_due=0,collection_intent_id=NULL WHERE id=$1`, e.invoice); err != nil {
			return err
		}
		return intents.NewStore(e.db.NewWithPgxTx(tx)).CompleteInvoiceCollection(ctx, operation, intents.TerminalWithEvidence("not executed", map[string]any{"not_executed": true}), time.Now())
	})
	require.ErrorContains(t, err, "qualified collected payment cannot become nonexecution")
	invoice := e.invoiceRow(t)
	require.EqualValues(t, 50000, invoice.AmountDue)
	require.NotNil(t, invoice.CollectionIntentID)
	require.Equal(t, intents.StatusUnknownNeedsVerify, latestCollectionIntent(t, e.pool, e.ctx, e.invoice).Status)
	dueNow(t, e.pool, e.ctx, e.op)
	_, err = e.runner.RunVerifyOnce(e.ctx)
	require.NoError(t, err)
	e.requireSettledOnce(t)
}
