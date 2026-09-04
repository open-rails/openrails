//go:build integration

package money_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/modules/admission/spendgate"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/stretchr/testify/require"
)

func noAdmissionHolds(context.Context, string) (int64, error) { return 0, nil }

func TestCreditGrantSupportLifecycle(t *testing.T) {
	svc, dbi, _, payer, cur, ctx := moneyInEnvWithDB(t)
	source := uuid.NewString()
	expiry := time.Now().Add(time.Hour)
	grant, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: "support", Currency: cur, Amount: 100, Source: "admin", SourceID: &source, ExpiresAt: &expiry})
	require.NoError(t, err)
	key, err := money.NewIdempotencyKey(money.OpSpend, "support-test", uuid.NewString())
	require.NoError(t, err)
	_, err = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "support", Currency: cur, Amount: 30, Key: key})
	require.NoError(t, err)
	extraSource := uuid.NewString()
	_, err = svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: "support", Currency: cur, Amount: 40, Source: "admin", SourceID: &extraSource})
	require.NoError(t, err)
	page, err := svc.ListCreditGrants(ctx, payer, cur, 1, 0)
	require.NoError(t, err)
	require.EqualValues(t, 2, page.Total)
	require.Len(t, page.Grants, 1)
	second, err := svc.ListCreditGrants(ctx, payer, cur, 1, 1)
	require.NoError(t, err)
	require.Len(t, second.Grants, 1)
	require.Equal(t, grant.ID, second.Grants[0].ID)
	require.EqualValues(t, 30, second.Grants[0].SpentAmount)
	require.EqualValues(t, 70, second.Grants[0].RemainingAmount)
	_, err = svc.RevokeCreditGrant(ctx, payer, grant.ID, "Redis unavailable", func(context.Context, string) (int64, error) { return 0, errors.New("hold backend unavailable") })
	require.ErrorContains(t, err, "hold backend unavailable")
	_, err = svc.RevokeCreditGrant(ctx, payer, grant.ID, "support removal", func(context.Context, string) (int64, error) { return 50, nil })
	require.ErrorIs(t, err, money.ErrCreditGrantHeld)
	unchanged, err := svc.ListCreditGrants(ctx, payer, cur, 1, 1)
	require.NoError(t, err)
	require.Equal(t, "active", unchanged.Grants[0].State)
	result, err := svc.RevokeCreditGrant(ctx, payer, grant.ID, "support removal", func(context.Context, string) (int64, error) { return 30, nil })
	require.NoError(t, err)
	require.False(t, result.Replayed)
	require.Equal(t, "revoked", result.Grant.State)
	require.EqualValues(t, 70, result.Grant.RevokedAmount)
	require.Zero(t, result.Grant.RemainingAmount)
	replay, err := svc.RevokeCreditGrant(ctx, payer, grant.ID, "retry", func(context.Context, string) (int64, error) { return 0, errors.New("Redis unavailable") })
	require.NoError(t, err)
	require.True(t, replay.Replayed)
	require.Equal(t, result.Grant, replay.Grant)
	balance, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	require.EqualValues(t, 40, balance.Balance)
	transactions, _, err := svc.GetTransactionsByCustomer(ctx, payer, cur, 10, 0)
	require.NoError(t, err)
	revokeCount := 0
	for _, tx := range transactions {
		if tx.TransactionType == "credit_revoke" {
			revokeCount++
			require.EqualValues(t, -70, tx.Amount)
		}
	}
	require.Equal(t, 1, revokeCount)
	other := identity.CustomerID(uuid.New())
	_, err = svc.RevokeCreditGrant(ctx, other, grant.ID, "wrong customer", noAdmissionHolds)
	require.ErrorIs(t, err, money.ErrCreditGrantNotFound)
	empty, err := svc.ListCreditGrants(ctx, payer, "EUR", 10, 0)
	require.NoError(t, err)
	require.Zero(t, empty.Total)
	// Original facts and the existing clawback remain the only authorities.
	var spent, revoked int64
	require.NoError(t, dbi.Qx(ctx).QueryRow(ctx, `SELECT COALESCE(sum(amount) FILTER(WHERE transfer_type='credit_spend'),0),COALESCE(sum(amount) FILTER(WHERE transfer_type='credit_revoke'),0) FROM openrails.ledger_transfers WHERE grant_id=$1`, grant.ID).Scan(&spent, &revoked))
	require.EqualValues(t, 100, spent+revoked)
}

func TestCreditGrantSupportExpiryAndDurableHolds(t *testing.T) {
	svc, dbi, _, payer, cur, ctx := moneyInEnvWithDB(t)
	source := uuid.NewString()
	grant, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: "support", Currency: cur, Amount: 100, Source: "admin", SourceID: &source})
	require.NoError(t, err)
	body := []byte(`{"test":"credit-support"}`)
	operation := money.OperationAuthorizationInput{OperationID: uuid.NewString(), Payer: payer, RecordOwner: "support-test", AuthorizedUSDMicros: 25, ClaimReference: "test-claim", AuthorizationBody: body, AuthorizationBodySHA256: sha256.Sum256(body)}
	require.NoError(t, dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		bound, txDB, err := dbi.BindMerchantTx(ctx, tx, dbtest.TestMerchantID)
		if err != nil {
			return err
		}
		_, err = svc.OpenOperationAuthorizationInTx(bound, txDB, operation, func(context.Context) (int64, error) { return 0, nil })
		return err
	}))
	_, err = svc.RevokeCreditGrant(ctx, payer, grant.ID, "held", noAdmissionHolds)
	require.ErrorIs(t, err, money.ErrCreditGrantHeld)
	_, err = svc.ReleaseOperationAuthorization(ctx, operation.OperationID, "test-released")
	require.NoError(t, err)
	_, err = svc.RevokeCreditGrant(ctx, payer, grant.ID, "released", noAdmissionHolds)
	require.NoError(t, err)

	testClock := clockwork.NewFakeClockAt(time.Now().Add(-2 * time.Hour))
	svc.SetClock(testClock)
	expiredAt := testClock.Now().Add(time.Hour)
	source = uuid.NewString()
	expired, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: "support", Currency: cur, Amount: 15, Source: "admin", SourceID: &source, ExpiresAt: &expiredAt})
	require.NoError(t, err)
	testClock.Advance(2 * time.Hour)
	_, err = svc.RevokeCreditGrant(ctx, payer, expired.ID, "expired", noAdmissionHolds)
	require.ErrorIs(t, err, money.ErrCreditGrantUnavailable)
	require.NoError(t, dbi.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := grants.New(gen.New(tx), dbtest.TestMerchantID.UUID()).ExpireLapsed(ctx, payer.UUID(), cur)
		return err
	}))
	page, err := svc.ListCreditGrants(ctx, payer, cur, 10, 0)
	require.NoError(t, err)
	for _, g := range page.Grants {
		if g.ID == expired.ID {
			require.Equal(t, "expired", g.State)
			require.EqualValues(t, 15, g.ExpiredAmount)
			require.Zero(t, g.RemainingAmount)
		}
	}
}

func TestCreditGrantSupportConcurrentSpendAndRevoke(t *testing.T) {
	for range 8 {
		svc, dbi, _, payer, cur, ctx := moneyInEnvWithDB(t)
		source := uuid.NewString()
		grant, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: "support", Currency: cur, Amount: 100, Source: "admin", SourceID: &source})
		require.NoError(t, err)
		key, err := money.NewIdempotencyKey(money.OpSpend, "support-race", uuid.NewString())
		require.NoError(t, err)
		start := make(chan struct{})
		var spendErr, revokeErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, spendErr = svc.SpendCredits(ctx, money.SpendParams{Payer: &payer, Invoker: "support", Currency: cur, Amount: 30, Key: key})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, revokeErr = svc.RevokeCreditGrant(ctx, payer, grant.ID, "concurrent support revoke", noAdmissionHolds)
		}()
		close(start)
		wg.Wait()
		require.NoError(t, revokeErr)
		if spendErr != nil {
			require.ErrorIs(t, spendErr, money.ErrInsufficientCredits)
		}
		bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
		require.NoError(t, err)
		require.Zero(t, bal.Balance)
		var total int64
		require.NoError(t, dbi.Qx(ctx).QueryRow(ctx, `SELECT COALESCE(sum(amount),0) FROM openrails.ledger_transfers WHERE grant_id=$1 AND transfer_type IN ('credit_spend','credit_revoke')`, grant.ID).Scan(&total))
		require.EqualValues(t, 100, total)
	}
}

func TestCreditGrantSupportConcurrentAdmission(t *testing.T) {
	svc, _, _, payer, cur, ctx := moneyInEnvWithDB(t)
	rdb := dbtest.NewSharedRedisClient(t)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())
	gate := spendgate.New(rdb)
	mid := dbtest.TestMerchantID.String()
	requestID := uuid.NewString()
	source := uuid.NewString()
	grant, err := svc.Deposit(ctx, money.DepositParams{CustomerID: &payer, Invoker: "support", Currency: cur, Amount: 100, Source: "admin", SourceID: &source})
	require.NoError(t, err)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var heldDecision spendgate.Decision
	var admissionErr, revokeErr error
	go func() {
		defer wg.Done()
		<-start
		admissionErr = svc.WithLockedAdmissionCapacity(ctx, payer, cur, func(capacity money.AdmissionCapacity) error {
			var err error
			heldDecision, err = gate.Admit(ctx, spendgate.AdmitInput{Merchant: mid, Customer: payer.UUID().String(), Currency: cur, RequestID: requestID, Cost: 40, AccountBalance: capacity.Balance - capacity.Held, HoldTTL: time.Minute})
			return err
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, revokeErr = svc.RevokeCreditGrant(ctx, payer, grant.ID, "race hold", func(ctx context.Context, currency string) (int64, error) {
			return gate.HeldAmount(ctx, mid, payer.UUID().String(), currency)
		})
	}()
	close(start)
	wg.Wait()
	require.NoError(t, admissionErr)
	bal, err := svc.GetBalanceForCustomer(ctx, payer, cur)
	require.NoError(t, err)
	held, err := gate.HeldAmount(ctx, mid, payer.UUID().String(), cur)
	require.NoError(t, err)
	require.GreaterOrEqual(t, bal.Balance-held, int64(0))
	if heldDecision.Allowed {
		require.ErrorIs(t, revokeErr, money.ErrCreditGrantHeld)
		require.EqualValues(t, 100, bal.Balance)
	} else {
		require.NoError(t, revokeErr)
		require.Zero(t, bal.Balance)
	}
	require.NoError(t, gate.Release(ctx, spendgate.ReleaseInput{Merchant: mid, Customer: payer.UUID().String(), Currency: cur, RequestID: requestID}))
}
