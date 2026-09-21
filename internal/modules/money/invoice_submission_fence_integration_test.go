//go:build integration

package money_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/dbtest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/stretchr/testify/require"
)

type invoicePrepareHook func(context.Context, money.ChargeRequest) (money.PreparedCharge, error)

func (f invoicePrepareHook) Prepare(ctx context.Context, r money.ChargeRequest) (money.PreparedCharge, error) {
	return f(ctx, r)
}

func TestInvoiceSubmissionFenceRecovery(t *testing.T) {
	t.Run("lost claim before marker", func(t *testing.T) {
		e := newNMIReceiptEnv(t)
		_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, nil, e.plane), 0)
		require.NoError(t, err)
		pending := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
		store := intents.NewStore(e.db)
		abandoned, claimed, err := store.ClaimByID(e.ctx, pending.ID, time.Now(), time.Now().Add(-time.Second))
		require.NoError(t, err)
		require.True(t, claimed)
		require.EqualValues(t, 1, abandoned.Attempts)
		require.Empty(t, intents.EvidenceString(abandoned, "submitted_at"))
		// No handler ran for the abandoned claim. A fresh executor must still send.
		_, err = e.runner.RunExecuteOnce(e.ctx)
		require.NoError(t, err)
		recovered, err := store.Get(e.ctx, pending.ID)
		require.NoError(t, err)
		require.EqualValues(t, 2, recovered.Attempts)
		require.Equal(t, intents.StatusUnknownNeedsVerify, recovered.Status)
		require.Len(t, e.gateway.sentOrderIDs(), 1)
		_, err = e.runner.ExecuteByID(e.ctx, pending.ID)
		require.NoError(t, err)
		require.Len(t, e.gateway.sentOrderIDs(), 1)
		e.op = pending.ID
		e.gateway.orderSale(e.op.String(), "recovered-once")
		e.gateway.payment("recovered-once", e.vault, "0.05", e.currency)
		require.Equal(t, intents.StatusSucceeded, e.verify(t))
		e.requireSettledOnce(t)
		require.Len(t, e.gateway.sentOrderIDs(), 1)
	})
	t.Run("sealed refusal survives local commit failure", func(t *testing.T) {
		e := newNMIReceiptEnv(t)
		_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, nil, e.plane), 0)
		require.NoError(t, err)
		pending := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
		store := intents.NewStore(e.db)
		claimed, ok, err := store.ClaimByID(e.ctx, pending.ID, time.Now(), time.Now().Add(time.Minute))
		require.NoError(t, err)
		require.True(t, ok)
		admin := dbtest.SharedSuperuserPGXPool(t)
		trigger := "invoice_nonsend_" + uuid.NewString()[:8]
		_, err = admin.Exec(e.ctx, fmt.Sprintf(`CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
        IF NEW.id='%s'::uuid AND NEW.status='failed_terminal' THEN RAISE EXCEPTION 'injected local completion failure'; END IF;
        RETURN NEW; END $$; CREATE TRIGGER %s BEFORE UPDATE ON billing.rail_intents FOR EACH ROW EXECUTE FUNCTION billing.%s()`, trigger, claimed.ID, trigger, trigger))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = admin.Exec(context.WithoutCancel(e.ctx), "DROP FUNCTION IF EXISTS billing."+trigger+"() CASCADE")
		})
		prepares := 0
		charger := invoicePrepareHook(func(context.Context, money.ChargeRequest) (money.PreparedCharge, error) {
			prepares++
			return money.PreparedChargeFunc(func(context.Context) (money.ChargeResult, error) {
				return money.ChargeResult{}, charge.ErrNotDispatched
			}), nil
		})
		handler := money.NewInvoiceCollectionHandler(e.db, charger, e.plane, fullModeConfig(), nil)
		outcome := handler.Execute(e.ctx, claimed)
		require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)
		require.Empty(t, outcome.Evidence)
		current, err := store.Get(e.ctx, claimed.ID)
		require.NoError(t, err)
		_, found, err := intents.LoadInvoiceNonexecution(current)
		require.NoError(t, err)
		require.True(t, found)
		require.NotNil(t, e.invoiceRow(t).CollectionIntentID)
		require.Empty(t, intents.EvidenceString(current, "not_executed"))
		_, err = admin.Exec(e.ctx, "DROP FUNCTION billing."+trigger+"() CASCADE")
		require.NoError(t, err)
		require.NoError(t, store.RetainCollectionCandidate(e.ctx, claimed, intents.CollectionCandidate{TransactionID: "unpaid-candidate"}))
		restarted := money.NewInvoiceCollectionHandler(e.db, nil, nil, fullModeConfig(), nil)
		outcome = restarted.Verify(e.ctx, claimed)
		require.Equal(t, intents.OutcomeTerminal, outcome.Class)
		current, err = store.Get(e.ctx, claimed.ID)
		require.NoError(t, err)
		require.Equal(t, intents.StatusFailedTerminal, current.Status)
		require.Nil(t, e.invoiceRow(t).CollectionIntentID)
		require.Equal(t, 1, prepares)
		require.Empty(t, e.gateway.sentOrderIDs())
		require.Zero(t, e.owedPaymentTransfers(t))
	})

	t.Run("conflicting qualified facts refuse completion", func(t *testing.T) {
		e := newNMIReceiptEnv(t)
		_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, nil, e.plane), 0)
		require.NoError(t, err)
		pending := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
		store := intents.NewStore(e.db)
		claimed, ok, err := store.ClaimByID(e.ctx, pending.ID, time.Now(), time.Now().Add(time.Minute))
		require.NoError(t, err)
		require.True(t, ok)
		proof, first, err := store.BeginInvoiceCollection(e.ctx, claimed, time.Now())
		require.NoError(t, err)
		require.True(t, first)
		require.NoError(t, store.RetainInvoiceNonexecution(e.ctx, claimed, proof, "not_dispatched", "known before POST"))
		// Inject conflicting positive provider facts, rather than an untrusted
		// outcome boolean. Neither qualified fact may silently win this conflict.
		e.gateway.orderSale(claimed.ID.String(), "contradicted-paid")
		e.gateway.payment("contradicted-paid", e.vault, "0.05", e.currency)
		receipt, found, err := e.plane.ReadCollectionReceipt(e.ctx, claimed, "contradicted-paid")
		require.NoError(t, err)
		require.True(t, found)
		_, err = store.RetainCollectedReceipt(e.ctx, claimed, receipt)
		require.NoError(t, err)
		handler := money.NewInvoiceCollectionHandler(e.db, nil, nil, fullModeConfig(), nil)
		require.Equal(t, intents.OutcomeAmbiguous, handler.Verify(e.ctx, claimed).Class)
		current, err := store.Get(e.ctx, claimed.ID)
		require.NoError(t, err)
		err = e.db.MerchantTx(e.ctx, func(c context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(c, `UPDATE billing.invoices SET collection_intent_id=NULL WHERE id=$1`, e.invoice)
			if err != nil {
				return err
			}
			return intents.NewStore(e.db.NewWithPgxTx(tx)).CompleteInvoiceCollection(c, current, intents.Succeeded(nil), time.Now())
		})
		require.ErrorContains(t, err, "contradicts qualified nonexecution")
		require.NotNil(t, e.invoiceRow(t).CollectionIntentID)
		require.Zero(t, e.settledPayments(t))
		require.Zero(t, e.owedPaymentTransfers(t))
	})

	for _, mode := range []string{"instrument changed during Prepare", "park during Prepare", "stale unsent resolution", "atomic stale release", "atomic freshly reloaded release", "expire fenced pending", "retry fenced unknown", "generic proof injection"} {
		t.Run(mode, func(t *testing.T) {
			e := newNMIReceiptEnv(t)
			_, err := e.svc.ChargeOutstanding(e.ctx, collectionRunner(e.db, nil, e.plane), 0)
			require.NoError(t, err)
			pending := latestCollectionIntent(t, e.pool, e.ctx, e.invoice)
			store := intents.NewStore(e.db)
			stale, claimed, err := store.ClaimByID(e.ctx, pending.ID, time.Now(), time.Now().Add(time.Minute))
			require.NoError(t, err)
			require.True(t, claimed)
			fence := func() {
				first, err := store.RecordProgressIfAbsent(e.ctx, stale.ID, "submitted_at", time.Now().UTC().Format(time.RFC3339Nano))
				require.NoError(t, err)
				require.True(t, first)
			}
			hook := invoicePrepareHook(func(context.Context, money.ChargeRequest) (money.PreparedCharge, error) {
				// Deterministic interleave: a newer owner fences while this older Prepare
				// is still doing read-only work. This caller has no submission authority.
				fence()
				if mode == "instrument changed during Prepare" {
					return nil, charge.ErrInstrumentChanged
				}
				return nil, errors.New("read-only qualification unavailable")
			})
			handler := money.NewInvoiceCollectionHandler(e.db, hook, e.plane, fullModeConfig(), nil)
			switch mode {
			case "instrument changed during Prepare":
				outcome := handler.Execute(e.ctx, stale)
				require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)
				require.Empty(t, outcome.Evidence)
			case "park during Prepare":
				outcome := handler.Execute(e.ctx, stale)
				if outcome.Class == intents.OutcomeParked {
					require.NoError(t, store.Park(e.ctx, stale.ID, time.Now(), outcome.Reason))
				}
			case "stale unsent resolution":
				fence()
				_, err := handler.ResolveUnsent(e.ctx, stale, intents.Resolution{NotExecuted: true})
				require.Error(t, err)
			case "atomic stale release", "atomic freshly reloaded release":
				fence()
				if mode == "atomic freshly reloaded release" {
					stale, err = store.Get(e.ctx, stale.ID)
					require.NoError(t, err)
				}
				err := e.db.MerchantTx(e.ctx, func(c context.Context, tx pgx.Tx) error {
					_, err := tx.Exec(c, `UPDATE billing.invoices SET collection_intent_id=NULL WHERE id=$1`, e.invoice)
					if err != nil {
						return err
					}
					return intents.NewStore(e.db.NewWithPgxTx(tx)).CompleteInvoiceCollection(c, stale, intents.TerminalWithEvidence("stale nonexecution", map[string]any{"not_executed": true}), time.Now())
				})
				require.Error(t, err)
			case "generic proof injection":
				fence()
				forged := map[string]any{"qualified_invoice_nonexecution": map[string]any{"not_executed": true}}
				require.Error(t, store.RecordProgress(e.ctx, stale.ID, forged))
				_, err := store.RecordProgressIfAbsent(e.ctx, stale.ID, "qualified_invoice_nonexecution", forged)
				require.Error(t, err)
				require.Error(t, store.MarkUnknown(e.ctx, stale.ID, time.Now(), "forged proof", forged))
				require.NoError(t, store.RecordProgress(e.ctx, stale.ID, map[string]any{"not_executed": true, "not_executed_code": "not_dispatched"}))
				current, err := store.Get(e.ctx, stale.ID)
				require.NoError(t, err)
				outcome := handler.Verify(e.ctx, current)
				require.Equal(t, intents.OutcomeAmbiguous, outcome.Class)
				err = e.db.MerchantTx(e.ctx, func(c context.Context, tx pgx.Tx) error {
					return intents.NewStore(e.db.NewWithPgxTx(tx)).CompleteInvoiceCollection(c, current, intents.TerminalWithEvidence("forged", map[string]any{"not_executed": true}), time.Now())
				})
				require.Error(t, err)
			case "retry fenced unknown":
				fence()
				require.NoError(t, store.MarkUnknown(e.ctx, stale.ID, time.Now(), "possibly submitted", nil))
				require.Error(t, store.MarkFailedRetryable(e.ctx, stale.ID, time.Now(), "stale verifier saw no marker"))
			case "expire fenced pending":
				fence()
				_, err := e.pool.Exec(e.ctx, `UPDATE billing.rail_intents SET status='pending',attempts=0,claimed_until=NULL,expires_at=now()-interval '1 second' WHERE id=$1`, stale.ID)
				require.NoError(t, err)
				_, err = e.db.Gen(e.ctx).ExpireOverdueRailIntents(e.ctx, gen.ExpireOverdueRailIntentsParams{Now: time.Now()})
				require.NoError(t, err)
			}
			current, err := store.Get(e.ctx, stale.ID)
			require.NoError(t, err)
			if mode == "retry fenced unknown" {
				require.Equal(t, intents.StatusUnknownNeedsVerify, current.Status)
			} else if mode == "expire fenced pending" {
				require.Equal(t, intents.StatusPending, current.Status)
			} else {
				require.Equal(t, intents.StatusInFlight, current.Status)
				require.Equal(t, stale.Attempts, current.Attempts)
			}
			require.NotEmpty(t, intents.EvidenceString(current, "submitted_at"))
			if mode != "generic proof injection" {
				require.Empty(t, intents.EvidenceString(current, "not_executed"))
			}
			require.NotNil(t, e.invoiceRow(t).CollectionIntentID)
			require.Zero(t, e.settledPayments(t))
			require.Zero(t, e.owedPaymentTransfers(t))
		})
	}
}
