//go:build integration

package tests

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// createTestMoneyBalance seeds a spendable money_block for the customer (#491:
// balance is derived from blocks). heldBalance is ignored — held is derived from
// active holds, which the caller seeds via createTestMoneyHold. Returns a
// MoneyBalance carrying the customer id for later derived reads.
func (suite *TestContainerSuite) createTestMoneyBalance(userID string, balance, heldBalance int64) *models.MoneyBalance {
	ctx := context.Background()
	now := suite.GetClock().Now()
	tenantSubjectID := suite.ensureCustomer(ctx, userID)
	_, err := suite.Pool.Exec(ctx, `
		INSERT INTO openrails.money_blocks (id, merchant_id, customer_id, currency, original_amount, remaining_amount, created_at)
		VALUES ($1, $2, $3, 'USD', $4, $4, $5)`,
		uuid.New(), dbtest.TestTenantID.UUID(), tenantSubjectID, balance, now)
	if err != nil {
		panic(err)
	}
	return &models.MoneyBalance{
		MerchantID: dbtest.TestTenantID.UUID(),
		CustomerID: tenantSubjectID,
		Currency:   "USD",
	}
}

// createTestMoneyHold creates a money hold for testing.
// Holds are stored as openrails.money_transactions rows with transaction_type='hold'.
func (suite *TestContainerSuite) createTestMoneyHold(userID string, amount int64, status string, expiresAt time.Time) *models.MoneyTransaction {
	ctx := context.Background()
	now := suite.GetClock().Now()
	tenantSubjectID := suite.ensureCustomer(ctx, userID)
	auth := amount
	sid := uuid.New().String()
	hold := &models.MoneyTransaction{
		ID:              uuid.New(),
		CustomerID:      tenantSubjectID,
		Currency:        "USD",
		Invoker:         userID,
		Amount:          0,
		BalanceAfter:    nil,
		TransactionType: "hold",
		Status:          status,
		Authorized:      &auth,
		Captured:        nil,
		Source:          "test",
		SourceID:        &sid,
		ExpiresAt:       &expiresAt,
		Description:     nil,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	_, err := suite.Pool.Exec(ctx, `
		INSERT INTO openrails.money_transactions (
			id, merchant_id, customer_id, currency, invoker, amount, balance_after,
			transaction_type, status, authorized_amount, captured_amount,
			source, source_id, expires_at, description, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		hold.ID, dbtest.TestTenantID.UUID(), hold.CustomerID, hold.Currency, hold.Invoker, hold.Amount, hold.BalanceAfter,
		hold.TransactionType, hold.Status, hold.Authorized, hold.Captured,
		hold.Source, hold.SourceID, hold.ExpiresAt, hold.Description, hold.CreatedAt, hold.UpdatedAt)
	if err != nil {
		panic(err)
	}
	return hold
}

// moneyTransactionCols is the column list scanned into models.MoneyTransaction.
const moneyTransactionCols = `id, merchant_id, customer_id, currency, invoker, resource, metadata,
	amount, balance_after, transaction_type, status, authorized_amount,
	captured_amount, source, source_id, expires_at, description, created_at, updated_at`

func scanMoneyTransaction(row interface{ Scan(...any) error }) (*models.MoneyTransaction, error) {
	t := new(models.MoneyTransaction)
	err := row.Scan(
		&t.ID, &t.MerchantID, &t.CustomerID, &t.Currency, &t.Invoker, &t.Resource, &t.Metadata,
		&t.Amount, &t.BalanceAfter, &t.TransactionType, &t.Status, &t.Authorized,
		&t.Captured, &t.Source, &t.SourceID, &t.ExpiresAt, &t.Description, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// getMoneyHold retrieves a money hold by ID
func (suite *TestContainerSuite) getMoneyHold(id uuid.UUID) *models.MoneyTransaction {
	row := suite.Pool.QueryRow(context.Background(),
		"SELECT "+moneyTransactionCols+" FROM openrails.money_transactions WHERE id = $1", id)
	hold, err := scanMoneyTransaction(row)
	if err != nil {
		panic(err)
	}
	return hold
}

// getMoneyBalance derives balance (SUM spendable blocks) + held (SUM active
// holds) for the customer (#491: no cache).
func (suite *TestContainerSuite) getMoneyBalance(b *models.MoneyBalance) *models.MoneyBalance {
	ctx := context.Background()
	out := &models.MoneyBalance{MerchantID: b.MerchantID, CustomerID: b.CustomerID, Currency: b.Currency}
	if err := suite.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(remaining_amount), 0)::bigint
		FROM openrails.money_blocks
		WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3 AND remaining_amount > 0
		  AND (expires_at IS NULL OR expires_at > now())`,
		b.MerchantID, b.CustomerID, b.Currency).Scan(&out.Balance); err != nil {
		panic(err)
	}
	if err := suite.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(COALESCE(authorized_amount, 0)), 0)::bigint
		FROM openrails.money_transactions
		WHERE merchant_id = $1 AND customer_id = $2 AND currency = $3
		  AND transaction_type = 'hold' AND status = 'active'`,
		b.MerchantID, b.CustomerID, b.Currency).Scan(&out.HeldBalance); err != nil {
		panic(err)
	}
	return out
}

// TestHoldExpiryWorkerNoExpiredHolds tests that the worker handles no expired holds gracefully
func TestHoldExpiryWorkerNoExpiredHolds(t *testing.T) {
	suite := getSharedTestSuite(t)

	// Create a fake clock set to "now"
	fakeClock := clockwork.NewFakeClockAt(time.Now())

	worker := &riverjobs.HoldExpiryWorker{
		DB:    suite.App.Runtime.DB,
		Clock: fakeClock,
	}

	job := &river.Job[riverjobs.HoldExpiryArgs]{
		Args: riverjobs.HoldExpiryArgs{},
	}

	// Worker should complete without error (no holds to process)
	err := worker.Work(context.Background(), job)
	require.NoError(t, err, "Worker should complete successfully with no expired holds")
}

// TestHoldExpiryWorkerExpiresActiveHolds tests that expired active holds are marked as expired
func TestHoldExpiryWorkerExpiresActiveHolds(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	// Create user with balance and held balance
	userID := uuid.New().String()
	balance := suite.createTestMoneyBalance(userID, 100, 50) // 100 total, 50 held

	// Create a fake clock
	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)

	// Create an expired hold (expires_at in the past)
	pastTime := now.Add(-1 * time.Hour)
	hold := suite.createTestMoneyHold(userID, 30, "active", pastTime)

	// Create a non-expired hold (expires_at in the future)
	futureTime := now.Add(1 * time.Hour)
	activeHold := suite.createTestMoneyHold(userID, 20, "active", futureTime)

	worker := &riverjobs.HoldExpiryWorker{
		DB:    suite.App.Runtime.DB,
		Clock: fakeClock,
	}

	job := &river.Job[riverjobs.HoldExpiryArgs]{
		Args: riverjobs.HoldExpiryArgs{},
	}

	// Run worker
	err := worker.Work(ctx, job)
	require.NoError(t, err)

	// Verify expired hold was marked as expired
	updatedHold := suite.getMoneyHold(hold.ID)
	assert.Equal(t, "expired", updatedHold.Status, "Expired hold should be marked as expired")

	// Verify non-expired hold is still active
	updatedActiveHold := suite.getMoneyHold(activeHold.ID)
	assert.Equal(t, "active", updatedActiveHold.Status, "Non-expired hold should still be active")

	// Derived held = remaining active holds (#491): the 30 expired, the 20 stays.
	updatedBalance := suite.getMoneyBalance(balance)
	assert.Equal(t, int64(20), updatedBalance.HeldBalance, "Held balance should be reduced by expired hold amount")
}

// TestHoldExpiryWorkerSkipsNonActiveHolds tests that non-active holds are not processed
func TestHoldExpiryWorkerSkipsNonActiveHolds(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	userID := uuid.New().String()
	balance := suite.createTestMoneyBalance(userID, 100, 30)

	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)
	pastTime := now.Add(-1 * time.Hour)

	// Create holds with different statuses (all expired time-wise)
	capturedHold := suite.createTestMoneyHold(userID, 10, "captured", pastTime)
	releasedHold := suite.createTestMoneyHold(userID, 10, "released", pastTime)
	alreadyExpiredHold := suite.createTestMoneyHold(userID, 10, "expired", pastTime)

	worker := &riverjobs.HoldExpiryWorker{
		DB:    suite.App.Runtime.DB,
		Clock: fakeClock,
	}

	job := &river.Job[riverjobs.HoldExpiryArgs]{
		Args: riverjobs.HoldExpiryArgs{},
	}

	err := worker.Work(ctx, job)
	require.NoError(t, err)

	// All holds should retain their original status
	assert.Equal(t, "captured", suite.getMoneyHold(capturedHold.ID).Status)
	assert.Equal(t, "released", suite.getMoneyHold(releasedHold.ID).Status)
	assert.Equal(t, "expired", suite.getMoneyHold(alreadyExpiredHold.ID).Status)

	// No active holds were created (captured/released/expired only), so derived
	// held is 0 (#491: held = SUM active holds, the seeded 30 was a no-op).
	updatedBalance := suite.getMoneyBalance(balance)
	assert.Equal(t, int64(0), updatedBalance.HeldBalance)
}

// TestHoldExpiryWorkerMultipleUserHolds tests expiring holds for multiple users
func TestHoldExpiryWorkerMultipleUserHolds(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)
	pastTime := now.Add(-1 * time.Hour)

	// Create 3 users with expired holds
	type testUser struct {
		userID  string
		balance *models.MoneyBalance
		hold    *models.MoneyTransaction
		holdAmt int64
		heldAmt int64
	}

	users := make([]testUser, 3)
	for i := 0; i < 3; i++ {
		userID := uuid.New().String()
		holdAmt := int64((i + 1) * 10) // 10, 20, 30
		heldAmt := int64((i + 1) * 20) // 20, 40, 60

		balance := suite.createTestMoneyBalance(userID, 100, heldAmt)
		hold := suite.createTestMoneyHold(userID, holdAmt, "active", pastTime)

		users[i] = testUser{
			userID:  userID,
			balance: balance,
			hold:    hold,
			holdAmt: holdAmt,
			heldAmt: heldAmt,
		}
	}

	worker := &riverjobs.HoldExpiryWorker{
		DB:    suite.App.Runtime.DB,
		Clock: fakeClock,
	}

	job := &river.Job[riverjobs.HoldExpiryArgs]{
		Args: riverjobs.HoldExpiryArgs{},
	}

	err := worker.Work(ctx, job)
	require.NoError(t, err)

	// Verify all holds were expired and balances updated
	for i, u := range users {
		updatedHold := suite.getMoneyHold(u.hold.ID)
		assert.Equal(t, "expired", updatedHold.Status, "User %d hold should be expired", i)

		// Each user's only active hold just expired, so derived held is 0 (#491).
		updatedBalance := suite.getMoneyBalance(u.balance)
		assert.Equal(t, int64(0), updatedBalance.HeldBalance, "User %d held balance should be 0 after expiry", i)
	}
}

// TestHoldExpiryWorkerBatching tests that the worker processes in batches
func TestHoldExpiryWorkerBatching(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	userID := uuid.New().String()
	// Create balance with enough held credits for all holds
	totalHolds := 5
	holdAmount := int64(10)
	suite.createTestMoneyBalance(userID, 1000, int64(totalHolds)*holdAmount)

	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)
	pastTime := now.Add(-1 * time.Hour)

	// Create multiple expired holds
	var holds []*models.MoneyTransaction
	for i := 0; i < totalHolds; i++ {
		hold := suite.createTestMoneyHold(userID, holdAmount, "active", pastTime)
		holds = append(holds, hold)
	}

	// Use small batch size to test batching
	worker := &riverjobs.HoldExpiryWorker{
		DB:        suite.App.Runtime.DB,
		Clock:     fakeClock,
		BatchSize: 2, // Process 2 at a time
	}

	job := &river.Job[riverjobs.HoldExpiryArgs]{
		Args: riverjobs.HoldExpiryArgs{},
	}

	err := worker.Work(ctx, job)
	require.NoError(t, err)

	// All holds should be expired
	for i, hold := range holds {
		updatedHold := suite.getMoneyHold(hold.ID)
		assert.Equal(t, "expired", updatedHold.Status, "Hold %d should be expired", i)
	}
}

// TestHoldExpiryWorkerNilDB tests that the worker returns error with nil DB
func TestHoldExpiryWorkerNilDB(t *testing.T) {
	worker := &riverjobs.HoldExpiryWorker{
		DB: nil,
	}

	job := &river.Job[riverjobs.HoldExpiryArgs]{
		Args: riverjobs.HoldExpiryArgs{},
	}

	err := worker.Work(context.Background(), job)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db is required")
}

// TestHoldExpiryWorkerHeldBalanceNeverNegative tests that held_balance never goes negative
func TestHoldExpiryWorkerHeldBalanceNeverNegative(t *testing.T) {
	suite := getSharedTestSuite(t)
	ctx := context.Background()

	userID := uuid.New().String()
	// Create balance with less held than the hold amount (edge case)
	balance := suite.createTestMoneyBalance(userID, 100, 10) // Only 10 held

	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)
	pastTime := now.Add(-1 * time.Hour)

	// Create hold for more than held balance (shouldn't happen normally, but testing safety)
	hold := suite.createTestMoneyHold(userID, 50, "active", pastTime)

	worker := &riverjobs.HoldExpiryWorker{
		DB:    suite.App.Runtime.DB,
		Clock: fakeClock,
	}

	job := &river.Job[riverjobs.HoldExpiryArgs]{
		Args: riverjobs.HoldExpiryArgs{},
	}

	err := worker.Work(ctx, job)
	require.NoError(t, err)

	// Hold should be expired
	updatedHold := suite.getMoneyHold(hold.ID)
	assert.Equal(t, "expired", updatedHold.Status)

	// Held balance should be 0, not negative (#491: derived from active holds).
	updatedBalance := suite.getMoneyBalance(balance)
	assert.GreaterOrEqual(t, updatedBalance.HeldBalance, int64(0), "Held balance should never be negative")
}
