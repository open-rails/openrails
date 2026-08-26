//go:build integration

package embed

import (
	"context"
	"crypto/sha256"
	"errors"
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

// TestOperationAuthorizationSettlementLifecycle is the focused th-005
// settlement proof on real Postgres + Redis. It red-arms exact replay, proves a
// partial capture reduces only its own hold, and proves final actual cost above
// the authorization is recorded as balance + owed rather than clamped.
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
		CustomerID: &payer,
		Invoker:    payerID.String(),
		Currency:   "USD",
		Amount:     10_000,
		Key:        depositKey,
	})
	require.NoError(t, err)

	newAuthorization := func(label string, amount int64) OperationAuthorizationRequest {
		operationID := label + "-" + uuid.NewString()
		body := []byte(`{"format":"th-auth-v1","operation_id":"` + operationID + `"}`)
		return OperationAuthorizationRequest{
			OperationID:             operationID,
			Payer:                   payer,
			RecordOwner:             "issuer:owner-1",
			AuthorizedUSDMicros:     amount,
			ClaimReference:          "claim:" + uuid.NewString(),
			AuthorizationBody:       body,
			AuthorizationBodySHA256: sha256.Sum256(body),
		}
	}
	mainAuthorization := newAuthorization("op-settle-main", 6_000)
	otherAuthorization := newAuthorization("op-settle-other", 3_000)
	_, err = openOperationAuthorizationInCommittedTx(ctx, rt, mainAuthorization)
	require.NoError(t, err)
	_, err = openOperationAuthorizationInCommittedTx(ctx, rt, otherAuthorization)
	require.NoError(t, err)

	partialBody := []byte(`{"format":"th-settlement-v1","window":1,"actual_usd_micros":2000}`)
	partialRequest := OperationAuthorizationSettlementRequest{
		OperationID:          mainAuthorization.OperationID,
		SettlementID:         "window-1-" + uuid.NewString(),
		AmountUSDMicros:      2_000,
		SettlementBody:       partialBody,
		SettlementBodySHA256: sha256.Sum256(partialBody),
	}
	partial, err := settleOperationAuthorizationInCommittedTx(ctx, rt, partialRequest)
	require.NoError(t, err)
	require.False(t, partial.Replayed)
	require.False(t, partial.Final)
	require.Equal(t, OperationAuthorizationOpen, partial.Authorization.State)
	require.Equal(t, int64(2_000), partial.Authorization.CapturedUSDMicros)
	require.Equal(t, int64(4_000), partial.Authorization.RemainingReservedUSDMicros)

	account, err := rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(8_000), account.BalanceAmount)
	require.Equal(t, int64(7_000), account.HeldAmount,
		"partial capture must reduce its own hold to 4,000 and preserve the other 3,000 hold")
	require.Equal(t, int64(1_000), account.AvailableAmount)

	replayed, err := settleOperationAuthorizationInCommittedTx(ctx, rt, partialRequest)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, partial.CreatedAt, replayed.CreatedAt)

	changedAmount := partialRequest
	changedAmount.AmountUSDMicros++
	_, err = settleOperationAuthorizationInCommittedTx(ctx, rt, changedAmount)
	require.ErrorIs(t, err, ErrOperationAuthorizationConflict)
	var conflict *OperationAuthorizationConflict
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "amount_usd_micros", conflict.Field)

	changedBody := partialRequest
	changedBody.SettlementBody = []byte(`{"format":"th-settlement-v1","window":"changed"}`)
	changedBody.SettlementBodySHA256 = sha256.Sum256(changedBody.SettlementBody)
	_, err = settleOperationAuthorizationInCommittedTx(ctx, rt, changedBody)
	require.ErrorIs(t, err, ErrOperationAuthorizationConflict)
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "settlement_body", conflict.Field)

	finalBody := []byte(`{"format":"th-settlement-v1","window":2,"actual_usd_micros":6000,"final":true}`)
	finalRequest := OperationAuthorizationSettlementRequest{
		OperationID:          mainAuthorization.OperationID,
		SettlementID:         "window-2-" + uuid.NewString(),
		AmountUSDMicros:      6_000,
		SettlementBody:       finalBody,
		SettlementBodySHA256: sha256.Sum256(finalBody),
		Final:                true,
		FinalReference:       "billing-stop-proof:" + uuid.NewString(),
	}
	final, err := settleOperationAuthorizationInCommittedTx(ctx, rt, finalRequest)
	require.NoError(t, err)
	require.Equal(t, OperationAuthorizationSettled, final.Authorization.State)
	require.Equal(t, int64(8_000), final.Authorization.CapturedUSDMicros,
		"actual cumulative cost above the 6,000 authorization must not be clamped")
	require.Zero(t, final.Authorization.RemainingReservedUSDMicros)
	require.Equal(t, finalRequest.FinalReference, final.Authorization.TerminalReference)
	require.NotNil(t, final.Authorization.SettledAt)

	account, err = rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(3_000), account.BalanceAmount)
	require.Equal(t, int64(3_000), account.HeldAmount,
		"final capture must release its own remainder without consuming the other authorization")
	require.Equal(t, int64(1_000), account.OutstandingOwedAmount,
		"all 6,000 final actual cost must post even though only 5,000 unreserved balance remained")

	finalReplay, err := settleOperationAuthorizationInCommittedTx(ctx, rt, finalRequest)
	require.NoError(t, err)
	require.True(t, finalReplay.Replayed)
	accountAfterReplay, err := rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, account, accountAfterReplay, "exact replay must not move ledger money twice")

	zeroBody := []byte(`{"format":"th-settlement-v1","actual_usd_micros":0,"final":true}`)
	zeroFinal, err := settleOperationAuthorizationInCommittedTx(ctx, rt, OperationAuthorizationSettlementRequest{
		OperationID:          otherAuthorization.OperationID,
		SettlementID:         "zero-window-" + uuid.NewString(),
		AmountUSDMicros:      0,
		SettlementBody:       zeroBody,
		SettlementBodySHA256: sha256.Sum256(zeroBody),
		Final:                true,
		FinalReference:       "zero-window-proof:" + uuid.NewString(),
	})
	require.NoError(t, err)
	require.Equal(t, OperationAuthorizationSettled, zeroFinal.Authorization.State)
	accountAfterZero, err := rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(3_000), accountAfterZero.BalanceAmount, "zero final must not invent a ledger movement")
	require.Zero(t, accountAfterZero.HeldAmount, "zero final releases the remaining reservation")
	require.Equal(t, int64(1_000), accountAfterZero.OutstandingOwedAmount)
}

func settleOperationAuthorizationInCommittedTx(ctx context.Context, rt *Runtime, request OperationAuthorizationSettlementRequest) (*OperationAuthorizationSettlement, error) {
	tx, err := rt.emb.App().Runtime.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	settlement, err := rt.SettleOperationAuthorizationTx(ctx, tx, request)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return settlement, nil
}
