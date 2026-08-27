//go:build integration

package money_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/money"
)

// TestPassThroughProviderCostSettlementLifecycle is the focused th-005 proof
// on real Postgres. It red-arms exact replay, proves OpenRails' pass-through
// rating, preserves every other hold, and records above-authorization cost as
// owed rather than clamping it.
func TestPassThroughProviderCostSettlementLifecycle(t *testing.T) {
	svc, dbi, _, payer, cur, ctx := moneyInEnvWithDB(t)
	_, err := svc.Deposit(ctx, money.DepositParams{
		CustomerID: &payer, Invoker: payer.UUID().String(), Currency: cur,
		Amount: 10_000, Source: "th-005-settlement-" + uuid.NewString(),
	})
	require.NoError(t, err)

	open := func(in money.OperationAuthorizationInput) (*money.OperationAuthorization, error) {
		tx, beginErr := dbi.Pool().Begin(ctx)
		if beginErr != nil {
			return nil, beginErr
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		boundCtx, txDB, bindErr := dbi.BindMerchantTx(ctx, tx, dbtest.TestMerchantID)
		if bindErr != nil {
			return nil, bindErr
		}
		auth, openErr := svc.OpenOperationAuthorizationInTx(boundCtx, txDB, in, func(context.Context) (int64, error) { return 0, nil })
		if openErr != nil {
			return nil, openErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return nil, commitErr
		}
		return auth, nil
	}
	settle := func(in money.PassThroughProviderCostSettlementInput) (*money.OperationAuthorization, error) {
		tx, beginErr := dbi.Pool().Begin(ctx)
		if beginErr != nil {
			return nil, beginErr
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		boundCtx, txDB, bindErr := dbi.BindMerchantTx(ctx, tx, dbtest.TestMerchantID)
		if bindErr != nil {
			return nil, bindErr
		}
		auth, settleErr := svc.SettlePassThroughProviderCostInTx(boundCtx, txDB, in)
		if settleErr != nil {
			return nil, settleErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return nil, commitErr
		}
		return auth, nil
	}
	newAuthorization := func(label string, amount int64) money.OperationAuthorizationInput {
		operationID := label + "-" + uuid.NewString()
		body := []byte(`{"format":"th-auth-v1","operation_id":"` + operationID + `"}`)
		return money.OperationAuthorizationInput{
			OperationID: operationID, Payer: payer, RecordOwner: "issuer:owner-1",
			AuthorizedUSDMicros: amount, ClaimReference: "claim:" + uuid.NewString(),
			AuthorizationBody: body, AuthorizationBodySHA256: sha256.Sum256(body),
		}
	}
	mainAuthorization := newAuthorization("op-settle-main", 6_000)
	otherAuthorization := newAuthorization("op-settle-other", 3_000)
	_, err = open(mainAuthorization)
	require.NoError(t, err)
	_, err = open(otherAuthorization)
	require.NoError(t, err)

	// Opaque fixture bytes from the future evidence qualifier; this internal
	// settlement mechanism does not itself decide eligibility.
	finalBody := []byte(`{"format":"th-settlement-v1","provider_cost_usd_micros":8000,"qualified":true}`)
	finalInput := money.PassThroughProviderCostSettlementInput{
		OperationID: mainAuthorization.OperationID, ProviderCostUSDMicros: 8_000, SettlementBody: finalBody,
	}
	final, err := settle(finalInput)
	require.NoError(t, err)
	require.False(t, final.Replayed)
	require.Equal(t, money.OperationAuthorizationSettled, final.State)
	require.NotNil(t, final.SettlementProviderCostUSDMicros)
	require.NotNil(t, final.SettlementRatedUSDMicros)
	require.Equal(t, int64(8_000), *final.SettlementProviderCostUSDMicros)
	require.Equal(t, final.SettlementProviderCostUSDMicros, final.SettlementRatedUSDMicros,
		"permanent pass-through contract must rate provider cost without a hidden policy")
	require.Equal(t, finalBody, final.SettlementBody)
	finalDigest := sha256.Sum256(finalBody)
	require.Equal(t, "sha256:"+fmt.Sprintf("%x", finalDigest[:]), final.TerminalReference)

	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(3_000), bal.Balance)
	require.Equal(t, int64(3_000), bal.HeldBalance,
		"settlement must remove its own hold without consuming the other authorization")
	owed, err := svc.GetOutstandingOwed(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(1_000), owed,
		"all 8,000 rated settlement must post even though only 7,000 unreserved balance remained")

	replayed, err := settle(finalInput)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	balAfterReplay, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, bal, balAfterReplay, "exact replay must not move ledger money twice")

	changedCost := finalInput
	changedCost.ProviderCostUSDMicros++
	_, err = settle(changedCost)
	require.ErrorIs(t, err, money.ErrOperationAuthorizationConflict)
	var conflict *money.OperationAuthorizationConflict
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "provider_cost_usd_micros", conflict.Field)

	changedBody := finalInput
	changedBody.SettlementBody = []byte(`{"format":"th-settlement-v1","provider_cost_usd_micros":"changed"}`)
	_, err = settle(changedBody)
	require.ErrorIs(t, err, money.ErrOperationAuthorizationConflict)
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "settlement_body", conflict.Field)

	zeroBody := []byte(`{"format":"th-settlement-v1","provider_cost_usd_micros":0,"qualified":true}`)
	zero, err := settle(money.PassThroughProviderCostSettlementInput{
		OperationID: otherAuthorization.OperationID, ProviderCostUSDMicros: 0, SettlementBody: zeroBody,
	})
	require.NoError(t, err)
	require.Equal(t, money.OperationAuthorizationSettled, zero.State)
	require.NotNil(t, zero.SettlementProviderCostUSDMicros)
	require.NotNil(t, zero.SettlementRatedUSDMicros)
	require.Zero(t, *zero.SettlementProviderCostUSDMicros)
	require.Zero(t, *zero.SettlementRatedUSDMicros)
	balAfterZero, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.Equal(t, int64(3_000), balAfterZero.Balance, "zero cost must not invent a ledger movement")
	require.Zero(t, balAfterZero.HeldBalance, "zero settlement releases the authorization")
}
