//go:build integration

package intents

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestManualRebillSavedCandidateStillRequiresExactRead(t *testing.T) {
	for _, key := range []string{"transaction_id", rebillCandidateKey} {
		t.Run(key, func(t *testing.T) {
			fx := seedPastDueSubscription(t)
			fake, client := newFakeNMIRebillGateway(t)
			fake.saleStatus.Store(http.StatusBadGateway)
			runner := fx.rebillRunner(client, fullModeConfig())
			ctx := fx.handlerCtx()
			row, err := runner.EnqueueAndExecute(ctx, fx.enqueueParams(1))
			require.NoError(t, err)
			require.Equal(t, StatusUnknownNeedsVerify, row.Status)
			require.NoError(t, fx.store.RecordProgress(ctx, row.ID, map[string]any{key: fake.txnID}))
			fake.charged.Store(true)
			fake.recordSale("", "0.01", "USD")
			_, err = fx.db.Pool().Exec(ctx, `UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1`, row.ID)
			require.NoError(t, err)
			_, err = runner.RunVerifyOnce(ctx)
			require.NoError(t, err)
			got := fx.intentByID(t, row.ID)
			require.Equal(t, StatusUnknownNeedsVerify, got.Status)
			require.Contains(t, string(got.ResultEvidence), rebillEvidenceContradiction)
			require.Zero(t, fx.paymentsFor(t, fake.txnID))
			require.Equal(t, "past_due", string(fx.subscription(t).Status))
			require.EqualValues(t, 1, fake.saleCalls.Load())
			require.Positive(t, fake.queryCalls.Load(), "candidate requires provider read")
		})
	}
}

func TestManualRebillReceiptCustodyFailureDoesNotApply(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)
	ctx := fx.handlerCtx()
	// Reject the actual insert-once receipt write, after the provider approved.
	// Scope the trigger to this operation's subscription, leaving other tests alone.
	name := "reject_receipt_" + fmt.Sprintf("%x", fx.subID[:])
	sql := fmt.Sprintf(`CREATE FUNCTION openrails.%s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.subscription_id='%s'::uuid AND NEW.result_evidence ? '%s' THEN RAISE EXCEPTION 'injected receipt custody failure'; END IF; RETURN NEW; END $$;
 CREATE TRIGGER %s BEFORE UPDATE ON openrails.rail_intents FOR EACH ROW EXECUTE FUNCTION openrails.%s()`, name, fx.subID, rebillReceiptKey, name, name)
	admin, err := pgx.Connect(ctx, dbtest.SharedSuperuserDSN(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	_, err = admin.Exec(ctx, sql)
	require.NoError(t, err)
	drop := func() {
		_, err := admin.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON openrails.rail_intents; DROP FUNCTION IF EXISTS openrails.%s()`, name, name))
		require.NoError(t, err)
	}
	t.Cleanup(drop)
	runner := fx.rebillRunner(client, fullModeConfig())
	row, err := runner.EnqueueAndExecute(ctx, fx.enqueueParams(1))
	require.NoError(t, err)
	require.Equal(t, StatusUnknownNeedsVerify, row.Status)
	require.Contains(t, *row.LastFailureReason, "custody failed")
	require.Equal(t, fake.txnID, EvidenceString(row, rebillCandidateKey))
	require.Zero(t, fx.paymentsFor(t, fake.txnID))
	require.Equal(t, "past_due", string(fx.subscription(t).Status))
	drop()
	_, err = fx.db.Pool().Exec(ctx, `UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1`, row.ID)
	require.NoError(t, err)
	_, err = runner.RunVerifyOnce(ctx)
	require.NoError(t, err)
	got := fx.intentByID(t, row.ID)
	require.Equal(t, StatusSucceeded, got.Status)
	require.Contains(t, string(got.ResultEvidence), rebillReceiptKey)
	require.Equal(t, 1, fx.paymentsFor(t, fake.txnID))
	require.EqualValues(t, 1, fake.saleCalls.Load(), "failed custody never resubmits")
}

func TestManualRebillFrozenProviderCoordinates(t *testing.T) {
	for _, field := range []string{"billing", "subscription", "credential"} {
		t.Run(field, func(t *testing.T) {
			fx := seedPastDueSubscription(t)
			fake, client := newFakeNMIRebillGateway(t)
			params := fx.enqueueParams(1)
			ctx := dbtest.WithTestMerchant(context.Background())
			// Leave the accepted operation pending under limited mode, then edit live state.
			row, err := fx.rebillRunner(client, limitedModeConfig()).EnqueueAndExecute(ctx, params)
			require.NoError(t, err)
			var stmt string
			switch field {
			case "billing":
				stmt = `UPDATE openrails.payment_methods SET rail_method_ref='remapped' WHERE id=$1`
			case "credential":
				stmt = `UPDATE openrails.payment_methods SET stored_credential_recurring_ref='later-anchor' WHERE id=$1`
			case "subscription":
				stmt = `UPDATE openrails.subscriptions SET rail_subscription_id='remapped' WHERE payment_method_id=$1`
			}
			_, err = fx.db.Pool().Exec(ctx, stmt, fx.methodID)
			require.NoError(t, err)
			_, err = fx.db.Pool().Exec(ctx, `UPDATE openrails.rail_intents SET next_attempt_at=now() WHERE id=$1`, row.ID)
			require.NoError(t, err)
			_, err = fx.rebillRunner(client, fullModeConfig()).RunExecuteOnce(ctx)
			require.NoError(t, err)
			got := fx.intentByID(t, row.ID)
			if field != "credential" {
				require.Equal(t, StatusSuperseded, got.Status)
				require.Zero(t, fake.saleCalls.Load())
				return
			}
			require.Equal(t, StatusSucceeded, got.Status)
			form := fake.saleForm.Load().(url.Values)
			require.Equal(t, params.Payload.(ManualRebillPayload).CredentialReference, form.Get("initial_transaction_id"))
			require.Equal(t, fx.billing, form.Get("billing_id"))
			require.Equal(t, fx.railSub, form.Get("subscription_id"))
		})
	}
}

func TestManualRebillReceiptCannotBeOverwritten(t *testing.T) {
	fx := seedPastDueSubscription(t)
	fake, client := newFakeNMIRebillGateway(t)
	fake.saleStatus.Store(http.StatusBadGateway)
	ctx := fx.handlerCtx()
	row, err := fx.rebillRunner(client, fullModeConfig()).EnqueueAndExecute(ctx, fx.enqueueParams(1))
	require.NoError(t, err)
	p, err := decodeManualRebillPayload(row)
	require.NoError(t, err)
	h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
	receipt := manualRebillReceipt{"receipt-A", rebillReceiptBinding(row, p, "receipt-A")}
	require.NoError(t, h.saveRebillReceipt(ctx, row, p, receipt))
	other := manualRebillReceipt{"receipt-B", rebillReceiptBinding(row, p, "receipt-B")}
	require.Error(t, h.saveRebillReceipt(ctx, row, p, other))
	stored := fx.intentByID(t, row.ID)
	var evidence map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(stored.ResultEvidence, &evidence))
	loaded, found, err := loadRebillReceipt(stored, p)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, receipt, loaded)
	require.Zero(t, fx.paymentsFor(t, fake.txnID))
}
