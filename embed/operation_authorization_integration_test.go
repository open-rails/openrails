//go:build integration

package embed

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/pkg/merchant"
)

// TestOperationAuthorizationLifecycle induces the minimal th-005 contract on
// real Postgres: replay/refusal/release and payer-lock capacity serialization
// against durable admission in both reservation interleavings.
func TestOperationAuthorizationLifecycle(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	rdb, _ := dbtest.SharedRedisClient(t)
	rt, err := New(ctx, Options{
		Config: &config.Config{
			ProviderWriteMode: config.ProviderWriteModeReadOnly,
			TestMode:          config.CredentialPostureLive,
			DB:                &config.DBConfig{URL: dsn},
		},
		Redis: rdb,
		River: RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	rt.app.Runtime.SetConfiguredMerchant(dbtest.TestMerchantID)

	merchantPool := dbtest.SharedMerchantPool(t, dbtest.TestMerchantID.UUID())
	payerID := dbtest.EnsureCustomerIDPgx(ctx, t, merchantPool, uuid.NewString())
	payer := identity.CustomerID(payerID)
	merchantCtx := merchant.WithID(ctx, dbtest.TestMerchantID)
	depositKey, err := service.NewDepositIdempotencyKey("th-005-test", uuid.NewString())
	require.NoError(t, err)
	_, err = rt.svc.DepositCredits(merchantCtx, service.DepositCreditsRequest{
		CustomerID: &payer,
		Invoker:    payerID.String(),
		Currency:   "USD",
		Amount:     10_000,
		Key:        depositKey,
	})
	require.NoError(t, err)

	body := []byte(`{"format":"th-auth-v1","operation_id":"op-funded"}`)
	request := openrails.OperationAuthorizationRequest{
		OperationID:             "op-funded-" + uuid.NewString(),
		Payer:                   openrails.CustomerID(payer),
		RecordOwner:             "issuer:owner-1",
		AuthorizedUSDMicros:     6_000,
		ClaimReference:          "claim:" + uuid.NewString(),
		AuthorizationBody:       body,
		AuthorizationBodySHA256: sha256.Sum256(body),
	}

	opened, err := openOperationAuthorizationInCommittedTx(ctx, rt, request)
	require.NoError(t, err)
	require.Equal(t, openrails.OperationAuthorizationOpen, opened.State)
	require.False(t, opened.Replayed)
	capacity, err := rt.svc.GetCreditAccount(merchantCtx, payer, "USD")
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
	require.ErrorIs(t, err, openrails.ErrOperationAuthorizationConflict)
	var conflict *openrails.OperationAuthorizationConflict
	require.True(t, errors.As(err, &conflict))
	require.Equal(t, "authorization_body", conflict.Field)

	secondBody := []byte(`{"format":"th-auth-v1","operation_id":"op-capacity"}`)
	second := openrails.OperationAuthorizationRequest{
		OperationID:             "op-capacity-" + uuid.NewString(),
		Payer:                   openrails.CustomerID(payer),
		RecordOwner:             request.RecordOwner,
		AuthorizedUSDMicros:     5_000,
		ClaimReference:          "claim:" + uuid.NewString(),
		AuthorizationBody:       secondBody,
		AuthorizationBodySHA256: sha256.Sum256(secondBody),
	}
	_, err = openOperationAuthorizationInCommittedTx(ctx, rt, second)
	require.ErrorIs(t, err, service.ErrInsufficientCredits,
		"the first open row must reserve capacity without moving ledger money")

	client, err := rt.Client(openrails.WithMerchantID(dbtest.TestMerchantID))
	require.NoError(t, err)
	released, err := client.ReleaseOperationAuthorization(ctx, openrails.ReleaseOperationAuthorizationRequest{
		OperationID: request.OperationID, ReleaseReference: "absence-proof:" + uuid.NewString(),
	})
	require.NoError(t, err)
	require.Equal(t, openrails.OperationAuthorizationReleased, released.State)
	require.False(t, released.Replayed)
	require.NotNil(t, released.ReleasedAt)
	capacity, err = rt.svc.GetCreditAccount(merchantCtx, payer, "USD")
	require.NoError(t, err)
	require.Equal(t, int64(0), capacity.HeldAmount)
	require.Equal(t, int64(10_000), capacity.AvailableAmount)

	releasedAgain, err := client.ReleaseOperationAuthorization(ctx, openrails.ReleaseOperationAuthorizationRequest{
		OperationID: request.OperationID, ReleaseReference: released.TerminalReference,
	})
	require.NoError(t, err)
	require.True(t, releasedAgain.Replayed)
	require.Equal(t, released.ReleasedAt, releasedAgain.ReleasedAt)

	read, err := client.GetOperationAuthorization(ctx, request.OperationID)
	require.NoError(t, err)
	require.Equal(t, openrails.OperationAuthorizationReleased, read.State)
	terminalReplay, err := openOperationAuthorizationInCommittedTx(ctx, rt, request)
	require.NoError(t, err)
	require.True(t, terminalReplay.Replayed)
	require.Equal(t, openrails.OperationAuthorizationReleased, terminalReplay.State,
		"an exact replay reports terminal truth and must never reopen capacity")

	openedSecond, err := openOperationAuthorizationInCommittedTx(ctx, rt, second)
	require.NoError(t, err, "release must restore the reserved capacity")
	require.Equal(t, openrails.OperationAuthorizationOpen, openedSecond.State)

	// 5,000 remains after openedSecond. Two distinct 3,000 operations race from
	// independent transactions: the customer-row money lock plus the open-row SUM
	// must allow exactly one, never let both read the same stale capacity.
	contenders := make([]openrails.OperationAuthorizationRequest, 2)
	for i := range contenders {
		contenderBody := []byte(`{"format":"th-auth-v1","operation_id":"concurrent"}`)
		contenders[i] = openrails.OperationAuthorizationRequest{
			OperationID:             "op-concurrent-" + uuid.NewString(),
			Payer:                   openrails.CustomerID(payer),
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

	// Prove the shared payer lock in both interleavings on fresh payers.
	// First, a durable authorization owns the customer lock before admission:
	// admission must wait for commit, then read the reduced PG capacity and deny.
	fundPayer := func(amount int64) identity.CustomerID {
		id := dbtest.EnsureCustomerIDPgx(ctx, t, merchantPool, uuid.NewString())
		p := identity.CustomerID(id)
		key, keyErr := service.NewDepositIdempotencyKey("th-005-race", uuid.NewString())
		require.NoError(t, keyErr)
		_, depositErr := rt.svc.DepositCredits(merchantCtx, service.DepositCreditsRequest{
			CustomerID: &p, Invoker: id.String(), Currency: "USD", Amount: amount, Key: key,
		})
		require.NoError(t, depositErr)
		return p
	}
	newAuthorization := func(p identity.CustomerID, amount int64) openrails.OperationAuthorizationRequest {
		operationID := "op-cross-store-" + uuid.NewString()
		body := []byte(`{"format":"th-auth-v1","operation_id":"` + operationID + `"}`)
		return openrails.OperationAuthorizationRequest{
			OperationID: operationID, Payer: openrails.CustomerID(p), RecordOwner: request.RecordOwner,
			AuthorizedUSDMicros: amount, ClaimReference: "claim:" + uuid.NewString(),
			AuthorizationBody: body, AuthorizationBodySHA256: sha256.Sum256(body),
		}
	}
	type admissionResult struct {
		decision spendgate.Decision
		err      error
	}
	lockedAdmission := func(p identity.CustomerID, requestID string, cost int64, entered chan<- struct{}, release <-chan struct{}) admissionResult {
		gate := spendgate.New(rt.app.Runtime.DB)
		var decision spendgate.Decision
		err := rt.app.Runtime.MoneyService.WithLockedAdmissionCapacity(merchantCtx, p, "USD", func(merchantCtx context.Context, txDB *db.DB, capacity money.AdmissionCapacity) error {
			var gateErr error
			decision, gateErr = gate.Admit(merchantCtx, txDB.Gen(merchantCtx), spendgate.AdmitInput{
				Customer: p.UUID(), Currency: "USD",
				RequestID: requestID, Cost: cost, AccountBalance: capacity.Balance - capacity.Held,
				ExpiresAt: time.Now().Add(time.Hour),
			})
			if entered != nil {
				close(entered)
			}
			if release != nil {
				<-release
			}
			return gateErr
		})
		return admissionResult{decision: decision, err: err}
	}

	authFirstPayer := fundPayer(10_000)
	authFirstTx, err := rt.app.Runtime.DB.Pool().Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = authFirstTx.Rollback(context.Background()) })
	_, err = NewHostTransactions(rt).OpenOperationAuthorization(ctx, authFirstTx, newAuthorization(authFirstPayer, 6_000))
	require.NoError(t, err)
	admissionStarted := make(chan struct{})
	admissionDone := make(chan admissionResult, 1)
	go func() {
		close(admissionStarted)
		admissionDone <- lockedAdmission(authFirstPayer, "admit-auth-first-"+uuid.NewString(), 5_000, nil, nil)
	}()
	<-admissionStarted
	requirePayerLockWait(t, ctx, dsn)
	require.NoError(t, authFirstTx.Commit(ctx))
	afterAuth := <-admissionDone
	require.NoError(t, afterAuth.err)
	require.False(t, afterAuth.decision.Allowed)
	require.True(t, afterAuth.decision.BlockedBalance)

	// Then admission owns the same payer lock through its durable reservation.
	// Authorization must wait, then subtract that admission hold and refuse.
	admissionFirstPayer := fundPayer(10_000)
	admissionEntered := make(chan struct{})
	releaseAdmission := make(chan struct{})
	admissionFirstDone := make(chan admissionResult, 1)
	go func() {
		admissionFirstDone <- lockedAdmission(admissionFirstPayer, "admit-first-"+uuid.NewString(), 6_000, admissionEntered, releaseAdmission)
	}()
	<-admissionEntered
	authorizationDone := make(chan error, 1)
	go func() {
		_, openErr := openOperationAuthorizationInCommittedTx(ctx, rt, newAuthorization(admissionFirstPayer, 5_000))
		authorizationDone <- openErr
	}()
	requirePayerLockWait(t, ctx, dsn)
	close(releaseAdmission)
	admissionFirst := <-admissionFirstDone
	require.NoError(t, admissionFirst.err)
	require.True(t, admissionFirst.decision.Allowed)
	require.ErrorIs(t, <-authorizationDone, service.ErrInsufficientCredits)

	// BIGINT-edge capacity must fail closed: MaxInt64 prepaid plus one unit of
	// remaining arrears credit cannot wrap into an apparently usable capacity.
	// Use a fresh merchant so its clearing counter starts at zero, and fund
	// through an actual balanced deposit instead of rewriting immutable facts.
	overflowRuntime, err := New(ctx, Options{
		Merchant: &MerchantDeclaration{Slug: "auth-overflow-" + uuid.NewString()},
		Config:   &config.Config{ProviderWriteMode: config.ProviderWriteModeReadOnly, TestMode: config.CredentialPostureLive, DB: &config.DBConfig{URL: dsn}},
		Redis:    rdb, River: RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = overflowRuntime.Close(context.Background()) })
	overflowClient, err := overflowRuntime.Client()
	require.NoError(t, err)
	overflowMerchant := overflowClient.MerchantID()
	overflowCustomer := openrails.CustomerID(uuid.New())
	_, err = overflowClient.EnsureCustomer(ctx, (overflowCustomer).String())
	require.NoError(t, err)
	_, err = overflowClient.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: new(overflowCustomer.String()), Invoker: overflowCustomer.String(), Currency: "USD",
		Amount: math.MaxInt64, Source: "th-005-overflow", SourceID: uuid.NewString(),
	})
	require.NoError(t, err)
	overflowPayer := identity.CustomerID(overflowCustomer)
	overflowCtx := merchant.WithID(ctx, overflowMerchant)
	arrears := money.BillingModeArrears
	require.NoError(t, overflowRuntime.svc.SetCreditAccountSettings(overflowCtx, overflowPayer, "USD", money.AccountSettingsInput{
		BillingMode: &arrears,
	}))
	require.NoError(t, overflowRuntime.svc.SetCreditLimit(overflowCtx, overflowPayer, "USD", 1))
	overflowRequest := newAuthorization(overflowPayer, 1)
	_, err = openOperationAuthorizationInCommittedTx(ctx, overflowRuntime, overflowRequest)
	require.ErrorContains(t, err, "capacity overflow")
	_, err = overflowClient.GetOperationAuthorization(ctx, overflowRequest.OperationID)
	require.ErrorIs(t, err, openrails.ErrOperationAuthorizationNotFound, "overflow refusal must not leave an authorization row")
}

func requirePayerLockWait(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { require.NoError(t, admin.Close(context.Background())) }()
	require.Eventually(t, func() bool {
		var waiting int
		err := admin.QueryRow(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE wait_event_type = 'Lock'
			  AND query LIKE '%LockCustomerForSpend%'
		`).Scan(&waiting)
		return err == nil && waiting > 0
	}, 5*time.Second, 10*time.Millisecond, "a contender must be visibly waiting on the shared PostgreSQL payer lock")
}

func openOperationAuthorizationInCommittedTx(ctx context.Context, rt *Runtime, request openrails.OperationAuthorizationRequest) (*openrails.OperationAuthorization, error) {
	tx, err := rt.app.Runtime.DB.Pool().Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	auth, err := NewHostTransactions(rt).OpenOperationAuthorization(ctx, tx, request)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return auth, nil
}
