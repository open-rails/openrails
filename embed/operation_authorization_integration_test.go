//go:build integration

package embed

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
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

// TestOperationAuthorizationLifecycle induces the minimal th-005 contract on
// real Postgres: funded open, exact replay, changed-body refusal, durable
// capacity reservation, and idempotent release that restores capacity.
func TestOperationAuthorizationLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	rt, err := New(ctx, Options{Options: embedded.Options{
		Config: &config.Config{
			Env:      "dev",
			TestMode: config.CredentialPostureLive,
			DB:       &config.DBConfig{URL: dsn},
		},
		River: embedded.RiverManagedByOpenRails(),
	}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	rt.emb.App().Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)

	merchantPool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	payerID := dbtest.EnsureCustomerIDPgx(ctx, t, merchantPool, uuid.NewString())
	payer := identity.CustomerID(payerID)
	merchantCtx := merchant.WithID(ctx, dbtest.TestMerchantID)
	depositKey, err := service.NewDepositIdempotencyKey("th-005-test", uuid.NewString())
	require.NoError(t, err)
	_, err = rt.Service().DepositCredits(merchantCtx, service.DepositCreditsRequest{
		CustomerID: &payer,
		Invoker:    payerID.String(),
		Currency:   "USD",
		Amount:     10_000,
		Key:        depositKey,
	})
	require.NoError(t, err)

	body := []byte(`{"format":"th-auth-v1","operation_id":"op-funded"}`)
	request := OperationAuthorizationRequest{
		OperationID:             "op-funded-" + uuid.NewString(),
		Payer:                   payer,
		RecordOwner:             "issuer:owner-1",
		AuthorizedUSDMicros:     6_000,
		ClaimReference:          "claim:" + uuid.NewString(),
		AuthorizationBody:       body,
		AuthorizationBodySHA256: sha256.Sum256(body),
	}

	opened, err := openOperationAuthorizationInCommittedTx(ctx, rt, request)
	require.NoError(t, err)
	require.Equal(t, OperationAuthorizationOpen, opened.State)
	require.False(t, opened.Replayed)
	require.NotEqual(t, uuid.Nil, opened.LedgerAccountID, "reservation must link to the existing ledger")
	capacity, err := rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(6_000), capacity.HeldAmount)
	require.Equal(t, int64(4_000), capacity.AvailableAmount,
		"ordinary OpenRails capacity reads must honor the durable reservation")

	replayed, err := openOperationAuthorizationInCommittedTx(ctx, rt, request)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, opened.CreatedAt, replayed.CreatedAt)

	changed := request
	changed.AuthorizationBody = []byte(`{"format":"th-auth-v1","operation_id":"changed"}`)
	changed.AuthorizationBodySHA256 = sha256.Sum256(changed.AuthorizationBody)
	_, err = openOperationAuthorizationInCommittedTx(ctx, rt, changed)
	require.ErrorIs(t, err, ErrOperationAuthorizationConflict)
	var conflict *OperationAuthorizationConflict
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "authorization_body", conflict.Field)

	secondBody := []byte(`{"format":"th-auth-v1","operation_id":"op-capacity"}`)
	second := OperationAuthorizationRequest{
		OperationID:             "op-capacity-" + uuid.NewString(),
		Payer:                   payer,
		RecordOwner:             request.RecordOwner,
		AuthorizedUSDMicros:     5_000,
		ClaimReference:          "claim:" + uuid.NewString(),
		AuthorizationBody:       secondBody,
		AuthorizationBodySHA256: sha256.Sum256(secondBody),
	}
	_, err = openOperationAuthorizationInCommittedTx(ctx, rt, second)
	require.ErrorIs(t, err, service.ErrInsufficientCredits,
		"the first open row must reserve capacity without moving ledger money")

	released, err := rt.ReleaseOperationAuthorization(ctx, ReleaseOperationAuthorizationRequest{
		OperationID: request.OperationID, ReleaseReference: "absence-proof:" + uuid.NewString(),
	})
	require.NoError(t, err)
	require.Equal(t, OperationAuthorizationReleased, released.State)
	require.False(t, released.Replayed)
	require.NotNil(t, released.ReleasedAt)
	capacity, err = rt.Service().GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(0), capacity.HeldAmount)
	require.Equal(t, int64(10_000), capacity.AvailableAmount)

	releasedAgain, err := rt.ReleaseOperationAuthorization(ctx, ReleaseOperationAuthorizationRequest{
		OperationID: request.OperationID, ReleaseReference: released.TerminalReference,
	})
	require.NoError(t, err)
	require.True(t, releasedAgain.Replayed)
	require.Equal(t, released.ReleasedAt, releasedAgain.ReleasedAt)

	read, err := rt.GetOperationAuthorization(ctx, request.OperationID)
	require.NoError(t, err)
	require.Equal(t, OperationAuthorizationReleased, read.State)
	terminalReplay, err := openOperationAuthorizationInCommittedTx(ctx, rt, request)
	require.NoError(t, err)
	require.True(t, terminalReplay.Replayed)
	require.Equal(t, OperationAuthorizationReleased, terminalReplay.State,
		"an exact replay reports terminal truth and must never reopen capacity")

	openedSecond, err := openOperationAuthorizationInCommittedTx(ctx, rt, second)
	require.NoError(t, err, "release must restore the reserved capacity")
	require.Equal(t, OperationAuthorizationOpen, openedSecond.State)

	// 5,000 remains after openedSecond. Two distinct 3,000 operations race from
	// independent transactions: the customer-row money lock plus the open-row SUM
	// must allow exactly one, never let both read the same stale capacity.
	contenders := make([]OperationAuthorizationRequest, 2)
	for i := range contenders {
		contenderBody := []byte(`{"format":"th-auth-v1","operation_id":"concurrent"}`)
		contenders[i] = OperationAuthorizationRequest{
			OperationID:             "op-concurrent-" + uuid.NewString(),
			Payer:                   payer,
			RecordOwner:             request.RecordOwner,
			AuthorizedUSDMicros:     3_000,
			ClaimReference:          "claim:" + uuid.NewString(),
			AuthorizationBody:       contenderBody,
			AuthorizationBodySHA256: sha256.Sum256(contenderBody),
		}
	}
	results := make([]error, len(contenders))
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = openOperationAuthorizationInCommittedTx(ctx, rt, contenders[i])
		}(i)
	}
	wg.Wait()
	var openedCount, refusedCount int
	for _, resultErr := range results {
		switch {
		case resultErr == nil:
			openedCount++
		case errors.Is(resultErr, service.ErrInsufficientCredits):
			refusedCount++
		default:
			require.NoError(t, resultErr)
		}
	}
	require.Equal(t, 1, openedCount, "one distinct operation may reserve the remaining capacity")
	require.Equal(t, 1, refusedCount, "the other distinct operation must observe the first reservation")
}

func openOperationAuthorizationInCommittedTx(ctx context.Context, rt *Runtime, request OperationAuthorizationRequest) (*OperationAuthorization, error) {
	tx, err := rt.emb.App().Runtime.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	auth, err := rt.OpenOperationAuthorizationTx(ctx, tx, request)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return auth, nil
}
