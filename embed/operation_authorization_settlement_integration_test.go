//go:build integration

package embed

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/open-rails/openrails/pkg/service"
)

// TestOperationAuthorizationSettlementLifecycle is the focused th-005 final
// settlement proof on real Postgres + Redis. It red-arms exact replay, proves
// every other hold survives, and proves a rated settlement above authorization is
// recorded as balance + owed rather than clamped.
func TestOperationAuthorizationSettlementLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	rdb, _ := dbtest.SharedRedisClient(t)
	rt, err := New(ctx, Options{Options: embedded.Options{
		Config: &config.Config{
			Env:      "dev",
			TestMode: config.CredentialPostureLive,
			DB:       &config.DBConfig{URL: dsn},
		},
		Redis: rdb,
		River: embedded.RiverManagedByOpenRails(),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	rt.emb.App().Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)

	merchantPool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	payerID := dbtest.EnsureCustomerIDPgx(ctx, t, merchantPool, uuid.NewString())
	payer := identity.CustomerID(payerID)
	merchantCtx := merchant.WithID(ctx, dbtest.TestMerchantID)
	depositKey, err := service.NewDepositIdempotencyKey("th-005-settlement", uuid.NewString())
	require.NoError(t, err)
	_, err = rt.Service().DepositCredits(merchantCtx, service.DepositCreditsRequest{
		CustomerID: &payer, Invoker: payerID.String(), Currency: "USD", Amount: 10_000, Key: depositKey,
	})
	require.NoError(t, err)

	newAuthorization := func(label string, amount int64) OperationAuthorizationRequest {
		operationID := label + "-" + uuid.NewString()
		body := []byte(`{"format":"th-auth-v1","operation_id":"` + operationID + `"}`)
		return OperationAuthorizationRequest{
			OperationID: operationID, Payer: payer, RecordOwner: "issuer:owner-1",
			AuthorizedUSDMicros: amount, ClaimReference: "claim:" + uuid.NewString(),
			AuthorizationBody: body, AuthorizationBodySHA256: sha256.Sum256(body),
		}
	}
	mainAuthorization := newAuthorization("op-settle-main", 6_000)
	otherAuthorization := newAuthorization("op-settle-other", 3_000)
	_, err = openOperationAuthorizationInCommittedTx(ctx, rt, mainAuthorization)
	require.NoError(t, err)
	_, err = openOperationAuthorizationInCommittedTx(ctx, rt, otherAuthorization)
	require.NoError(t, err)

	finalBody := []byte(`{"format":"th-settlement-v1","rated_usd_micros":8000,"final":true}`)
	finalRequest := OperationAuthorizationSettlementRequest{
		OperationID:    mainAuthorization.OperationID,
		RatedUSDMicros: 8_000,
		SettlementBody: finalBody,
	}
	final, err := settleOperationAuthorizationInCommittedTx(ctx, rt, finalRequest)
	require.NoError(t, err)
	require.False(t, final.Replayed)
	require.Equal(t, OperationAuthorizationSettled, final.State)
	require.NotNil(t, final.SettlementRatedUSDMicros)
	require.Equal(t, int64(8_000), *final.SettlementRatedUSDMicros,
		"host-rated final settlement above the 6,000 authorization must not be clamped")
	require.Equal(t, finalBody, final.SettlementBody)
	finalDigest := sha256.Sum256(finalBody)
	require.Equal(t, "sha256:"+fmt.Sprintf("%x", finalDigest[:]), final.TerminalReference)
	require.NotNil(t, final.SettledAt)

	account, err := rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(3_000), account.BalanceAmount)
	require.Equal(t, int64(3_000), account.HeldAmount,
		"final capture must release its own full hold without consuming the other authorization")
	require.Equal(t, int64(1_000), account.OutstandingOwedAmount,
		"all 8,000 rated settlement must post even though only 7,000 unreserved balance remained")

	finalReplay, err := settleOperationAuthorizationInCommittedTx(ctx, rt, finalRequest)
	require.NoError(t, err)
	require.True(t, finalReplay.Replayed)
	accountAfterReplay, err := rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, account, accountAfterReplay, "exact replay must not move ledger money twice")

	changedAmount := finalRequest
	changedAmount.RatedUSDMicros++
	_, err = settleOperationAuthorizationInCommittedTx(ctx, rt, changedAmount)
	require.ErrorIs(t, err, ErrOperationAuthorizationConflict)
	var conflict *OperationAuthorizationConflict
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "rated_usd_micros", conflict.Field)

	changedBody := finalRequest
	changedBody.SettlementBody = []byte(`{"format":"th-settlement-v1","rated_usd_micros":"changed"}`)
	_, err = settleOperationAuthorizationInCommittedTx(ctx, rt, changedBody)
	require.ErrorIs(t, err, ErrOperationAuthorizationConflict)
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "settlement_body", conflict.Field)

	zeroBody := []byte(`{"format":"th-settlement-v1","rated_usd_micros":0,"final":true}`)
	zeroFinal, err := settleOperationAuthorizationInCommittedTx(ctx, rt, OperationAuthorizationSettlementRequest{
		OperationID:    otherAuthorization.OperationID,
		RatedUSDMicros: 0,
		SettlementBody: zeroBody,
	})
	require.NoError(t, err)
	require.Equal(t, OperationAuthorizationSettled, zeroFinal.State)
	require.NotNil(t, zeroFinal.SettlementRatedUSDMicros)
	require.Zero(t, *zeroFinal.SettlementRatedUSDMicros)
	accountAfterZero, err := rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(3_000), accountAfterZero.BalanceAmount, "zero final must not invent a ledger movement")
	require.Zero(t, accountAfterZero.HeldAmount, "zero final releases the authorization")
	require.Equal(t, int64(1_000), accountAfterZero.OutstandingOwedAmount)
}

func settleOperationAuthorizationInCommittedTx(ctx context.Context, rt *Runtime, request OperationAuthorizationSettlementRequest) (*OperationAuthorization, error) {
	tx, err := rt.emb.App().Runtime.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	authorization, err := rt.SettleOperationAuthorizationTx(ctx, tx, request)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return authorization, nil
}
