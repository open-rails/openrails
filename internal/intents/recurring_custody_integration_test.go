//go:build integration

package intents

import (
	"context"
	"encoding/json"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/stretchr/testify/require"
)

// Both recurring families share this custody/commit protocol. The engine row
// adapts the existing native receipt fixture; this is not enrollment evidence.
func TestRecurringReceiptCustodyRefusesContradictions(t *testing.T) {
	for _, kind := range []string{subscriptions.TypeManualRebill, subscriptions.TypeSubscriptionCollection} {
		for _, first := range []string{"receipt", "decline"} {
			t.Run(kind+"/"+first, func(t *testing.T) {
				fx := seedPastDueSubscription(t)
				gateway, client := newFakeNMIRebillGateway(t, fx)
				ctx := fx.handlerCtx()
				h := NewManualRebillHandler(fx.db, fullModeConfig(), fakeNMIResolver{client: client}, nil)
				op, err := h.EnqueueScheduled(ctx, fx.subID)
				require.NoError(t, err)
				native, err := subscriptions.DecodeManualRebillPayload(op)
				require.NoError(t, err)
				gateway.orderID.Store(native.OrderReference)
				if kind == subscriptions.TypeSubscriptionCollection {
					custody := uuid.New()
					_, err = fx.db.Pool().Exec(ctx, `INSERT INTO billing.custodians(id,merchant_id,key,kind,account_id,environment,settings) VALUES($1,$2,$3,'hyperswitch',$3,'test','{"profile_id":"fixture","public_api_key":"fixture"}')`, custody, fx.merchantID, custody.String())
					require.NoError(t, err)
					instrument := native.Instrument
					instrument.Custodian = models.CustodianHyperSwitch
					instrument.CustodianID = &custody
					key := subscriptions.SubscriptionCollectionKey(fx.subID, native.Renewal.PeriodStart, 0)
					p := subscriptions.SubscriptionCollectionPayload{Initiator: charge.InitiatorMerchant, Renewal: native.Renewal, PreviousPeriodEnd: native.Renewal.PeriodStart, AcceptedAt: native.Renewal.PeriodStart, PaymentMethodID: native.PaymentMethodID, Instrument: instrument, AmountMinor: native.AmountMinor, OrderReference: subscriptions.RebillOrderReference(key)}
					p.HyperSwitch.AccountID, p.HyperSwitch.ProfileID, p.HyperSwitch.APIBaseURL = "fixture", "fixture", "http://127.0.0.1:1"
					raw, err := json.Marshal(p)
					require.NoError(t, err)
					_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.rail_intents SET intent_type=$2,payload=$3,idempotency_key=$4,custodian_id=$5 WHERE id=$1`, op.ID, kind, raw, key, custody)
					require.NoError(t, err)
					gateway.orderID.Store(p.OrderReference)
				}
				store := NewStore(fx.db)
				op, ok, err := store.ClaimByID(ctx, op.ID, time.Now().UTC(), time.Now().Add(time.Minute))
				require.NoError(t, err)
				require.True(t, ok)
				gateway.charged.Store(true)
				receipt, found, err := ReadNMICollectionReceipt(ctx, op, fakeNMIResolver{client: client}, gateway.txnID)
				require.NoError(t, err)
				require.True(t, found)
				if first == "receipt" {
					_, err = store.RetainCollectedReceipt(ctx, op, receipt)
					require.NoError(t, err)
					require.Error(t, store.RetainRecurringDecline(ctx, op, 200, ""))
				} else {
					require.NoError(t, store.RetainRecurringDecline(ctx, op, 200, ""))
					_, err = store.RetainCollectedReceipt(ctx, op, receipt)
					require.Error(t, err)
				}
				current, err := store.Get(ctx, op.ID)
				require.NoError(t, err)
				var retained map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(current.ResultEvidence, &retained))
				require.Len(t, retained, 1, "the second contradictory fact must not be written")

				// Legacy/corrupt coexisting custody cannot commit local effects, even if a
				// caller reaches the shared transaction boundary after writing them.
				binding, err := collectionBinding(op)
				require.NoError(t, err)
				both, err := json.Marshal(map[string]any{qualifiedReceiptKey: receipt.data, rebillDeclineKey: rebillDecline{Binding: binding, ResponseCode: 200}})
				require.NoError(t, err)
				_, err = fx.db.Pool().Exec(ctx, `UPDATE billing.rail_intents SET result_evidence=$2 WHERE id=$1`, op.ID, both)
				require.NoError(t, err)
				err = fx.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
					d := fx.db.NewWithPgxTx(tx)
					lc := subscriptions.NewSubscriptionLifecycleService(d, nil, nil, nil, nil, payments.NewPaymentService(d), nil)
					if err := lc.RenewMembership(ctx, &subscriptions.RenewMembershipParams{Prepared: &native.Renewal, Rail: models.RailNMI, RailSubscriptionID: native.RailSubscriptionID, TransactionID: gateway.txnID, Amount: native.Renewal.Amount, AmountProvided: true, Currency: native.Renewal.Currency}); err != nil {
						return err
					}
					outcome := Succeeded(map[string]any{"transaction_id": gateway.txnID})
					if kind == subscriptions.TypeManualRebill {
						return NewStore(d).CompleteManualRebill(ctx, op, outcome, time.Now())
					}
					return NewStore(d).CompleteSubscriptionCollection(ctx, op, outcome, time.Now())
				})
				require.ErrorContains(t, err, "decline")
				require.Zero(t, fx.paymentsFor(t, gateway.txnID))
				require.True(t, fx.subscription(t).CurrentPeriodEndsAt.Equal(fx.periodEnd))
				current, err = store.Get(ctx, op.ID)
				require.NoError(t, err)
				_, _, err = LoadCollectedReceipt(current)
				require.ErrorContains(t, err, "decline")

				// A correct capsule cannot bless a contradictory successful projection.
				current.Status = StatusSucceeded
				current.ResultEvidence, err = json.Marshal(map[string]any{qualifiedReceiptKey: receipt.data, "transaction_id": "wrong-transaction"})
				require.NoError(t, err)
				_, _, err = LoadCollectedReceipt(current)
				require.ErrorContains(t, err, "projection")
				if kind == subscriptions.TypeManualRebill {
					require.Error(t, ValidateManualRebillTerminal(current))
				} else {
					require.Error(t, ValidateSubscriptionCollectionTerminal(current))
				}
				require.Zero(t, gateway.saleCalls.Load())
			})
		}
	}
}
