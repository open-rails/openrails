//go:build integration

package intents

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/stretchr/testify/require"
)

func TestRefundIntentKeepsArchivedAccountWithCollidingTransactions(t *testing.T) {
	fx := seedRefundablePayment(t, 500)
	ctx := dbtest.WithTestMerchant(context.Background())
	sibling := uuid.New()
	accountA, accountB := uuid.NewString(), uuid.NewString()
	_, err := fx.db.Qx(ctx).Exec(ctx, `UPDATE openrails.psps SET account_id=$2,key=$2 WHERE id=$1`, fx.pspID, accountA)
	require.NoError(t, err)
	_, err = fx.db.Qx(ctx).Exec(ctx, `INSERT INTO openrails.psps(id,merchant_id,rail,environment,account_id,key) VALUES($1,$2,'nmi','test',$3,$3)`, sibling, dbtest.TestMerchantID.UUID(), accountB)
	require.NoError(t, err)
	siblingPayment := uuid.New()
	_, err = fx.db.Qx(ctx).Exec(ctx, `INSERT INTO openrails.payments(id,price_id,rail,psp_id,transaction_id,amount,list_amount,currency,status,customer_id,merchant_id,money_movement) SELECT $2,price_id,rail,$3,transaction_id,amount,list_amount,currency,status,customer_id,merchant_id,money_movement FROM openrails.payments WHERE id=$1`, fx.paymentID, siblingPayment, sibling)
	require.NoError(t, err)
	secrets := merchants.NewMemorySecretStore()
	for account, key := range map[string]string{accountA: "selected-secret", accountB: "sibling-secret"} {
		name, err := merchants.PSPSecretName("nmi", "test", account, "security_key")
		require.NoError(t, err)
		_, err = secrets.Put(ctx, dbtest.TestMerchantID, name, key)
		require.NoError(t, err)
	}
	registry, err := merchants.NewService(db.WrapPool(fx.db.Pool(), ""), secrets, "test")
	require.NoError(t, err)
	var calls atomic.Int64
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "selected-secret" || r.URL.Path != "/payments/"+fx.originalTxn+"/refund" {
			t.Errorf("refund escaped captured account: authorization=%t path=%s", r.Header.Get("Authorization") == "selected-secret", r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		calls.Add(1)
		fmt.Fprint(w, `{"object":"transaction","id":"refund-selected","response":"1","response_text":"SUCCESS"}`)
	}))
	defer gateway.Close()
	cfg := fullModeConfig()
	cfg.TestMode = config.CredentialPostureSandbox
	resolver := &railresolve.NMIArmer{DB: fx.db, Config: cfg, MerchantsFn: func() *merchants.Service { return registry }, Endpoints: railresolve.NMIEndpoints{V5BaseURL: gateway.URL}}
	runner := &Runner{Store: fx.store, Registry: NewRegistry(NewNMIRefundHandler(fx.db, resolver, nil)), Config: cfg}
	// Queue the obligation, then change its display name and admission state.
	intent, err := fx.store.Enqueue(ctx, fx.enqueueParams(500))
	require.NoError(t, err)
	_, err = fx.db.Qx(ctx).Exec(ctx, `UPDATE openrails.psps SET key=$2,archived=true WHERE id=$1`, fx.pspID, "renamed-"+accountA)
	require.NoError(t, err)
	result, err := runner.EnqueueAndExecute(ctx, fx.enqueueParams(500))
	require.NoError(t, err)
	require.Equal(t, intent.ID, result.ID)
	require.Equal(t, StatusSucceeded, result.Status)
	require.EqualValues(t, 1, calls.Load())
	replay, err := runner.EnqueueAndExecute(ctx, fx.enqueueParams(500))
	require.NoError(t, err)
	require.Equal(t, result.ID, replay.ID)
	require.EqualValues(t, 1, calls.Load())
	var siblingRefunds int
	require.NoError(t, fx.db.Qx(ctx).QueryRow(ctx, `SELECT count(*) FROM openrails.payments WHERE refunded_payment_id=$1`, siblingPayment).Scan(&siblingRefunds))
	require.Zero(t, siblingRefunds)
}
