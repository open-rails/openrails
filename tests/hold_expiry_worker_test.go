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
	riverjobs "github.com/open-rails/openrails/internal/river"
)

// createTestCreditType creates a credit type for testing
func (suite *TestContainerSuite) createTestCreditType(name string) *models.CreditType {
	now := suite.GetClock().Now()
	ct := &models.CreditType{
		ID:            uuid.New(),
		Name:          name,
		DisplayName:   name,
		Unit:          "credits",
		DecimalPlaces: 0,
		IsActive:      true,
		CreatedAt:     now,
	}
	_, err := suite.BunDB.NewInsert().Model(ct).Exec(context.Background())
	if err != nil {
		panic(err)
	}
	return ct
}

// createTestCreditBalance creates a user credit balance for testing
func (suite *TestContainerSuite) createTestCreditBalance(userID string, creditTypeID uuid.UUID, balance, heldBalance int64) *models.CreditBalance {
	ctx := context.Background()
	now := suite.GetClock().Now()
	tenantSubjectID := suite.ensureTenantSubject(ctx, userID)
	bal := &models.CreditBalance{
		ID:              uuid.New(),
		TenantSubjectID: tenantSubjectID,
		CreditTypeID:    creditTypeID,
		Balance:         balance,
		HeldBalance:     heldBalance,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	_, err := suite.BunDB.NewInsert().Model(bal).Exec(ctx)
	if err != nil {
		panic(err)
	}
	return bal
}

// createTestCreditHold creates a credit hold for testing.
// Holds are stored as billing.credit_transactions rows with transaction_type='hold'.
func (suite *TestContainerSuite) createTestCreditHold(userID string, creditTypeID uuid.UUID, amount int64, status string, expiresAt time.Time) *models.CreditTransaction {
	ctx := context.Background()
	now := suite.GetClock().Now()
	tenantSubjectID := suite.ensureTenantSubject(ctx, userID)
	auth := amount
	sid := uuid.New().String()
	hold := &models.CreditTransaction{
		ID:              uuid.New(),
		TenantSubjectID: tenantSubjectID,
		Actor:           userID,
		CreditTypeID:    creditTypeID,
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
	_, err := suite.BunDB.NewInsert().Model(hold).Exec(ctx)
	if err != nil {
		panic(err)
	}
	return hold
}

// getCreditHold retrieves a credit hold by ID
func (suite *TestContainerSuite) getCreditHold(id uuid.UUID) *models.CreditTransaction {
	hold := new(models.CreditTransaction)
	err := suite.BunDB.NewSelect().Model(hold).Where("id = ?", id).Scan(context.Background())
	if err != nil {
		panic(err)
	}
	return hold
}

// getCreditBalance retrieves a user credit balance by ID
func (suite *TestContainerSuite) getCreditBalance(id uuid.UUID) *models.CreditBalance {
	bal := new(models.CreditBalance)
	err := suite.BunDB.NewSelect().Model(bal).Where("id = ?", id).Scan(context.Background())
	if err != nil {
		panic(err)
	}
	return bal
}

// TestHoldExpiryWorkerNoExpiredHolds tests that the worker handles no expired holds gracefully
func TestHoldExpiryWorkerNoExpiredHolds(t *testing.T) {
	suite := setupTestSuite(t)

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
	suite := setupTestSuite(t)
	ctx := context.Background()

	// Use a unique credit type name to avoid conflicts
	creditTypeName := "test-hold-expiry-" + uuid.New().String()[:8]

	// Create credit type
	creditType := suite.createTestCreditType(creditTypeName)

	// Create user with balance and held balance
	userID := uuid.New().String()
	balance := suite.createTestCreditBalance(userID, creditType.ID, 100, 50) // 100 total, 50 held

	// Create a fake clock
	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)

	// Create an expired hold (expires_at in the past)
	pastTime := now.Add(-1 * time.Hour)
	hold := suite.createTestCreditHold(userID, creditType.ID, 30, "active", pastTime)

	// Create a non-expired hold (expires_at in the future)
	futureTime := now.Add(1 * time.Hour)
	activeHold := suite.createTestCreditHold(userID, creditType.ID, 20, "active", futureTime)

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
	updatedHold := suite.getCreditHold(hold.ID)
	assert.Equal(t, "expired", updatedHold.Status, "Expired hold should be marked as expired")

	// Verify non-expired hold is still active
	updatedActiveHold := suite.getCreditHold(activeHold.ID)
	assert.Equal(t, "active", updatedActiveHold.Status, "Non-expired hold should still be active")

	// Verify held_balance was reduced by the expired hold amount
	updatedBalance := suite.getCreditBalance(balance.ID)
	// Original held: 50, expired hold: 30, so new held should be 20
	assert.Equal(t, int64(20), updatedBalance.HeldBalance, "Held balance should be reduced by expired hold amount")
}

// TestHoldExpiryWorkerSkipsNonActiveHolds tests that non-active holds are not processed
func TestHoldExpiryWorkerSkipsNonActiveHolds(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	creditTypeName := "test-hold-expiry-skip-" + uuid.New().String()[:8]
	creditType := suite.createTestCreditType(creditTypeName)

	userID := uuid.New().String()
	balance := suite.createTestCreditBalance(userID, creditType.ID, 100, 30)

	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)
	pastTime := now.Add(-1 * time.Hour)

	// Create holds with different statuses (all expired time-wise)
	capturedHold := suite.createTestCreditHold(userID, creditType.ID, 10, "captured", pastTime)
	releasedHold := suite.createTestCreditHold(userID, creditType.ID, 10, "released", pastTime)
	alreadyExpiredHold := suite.createTestCreditHold(userID, creditType.ID, 10, "expired", pastTime)

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
	assert.Equal(t, "captured", suite.getCreditHold(capturedHold.ID).Status)
	assert.Equal(t, "released", suite.getCreditHold(releasedHold.ID).Status)
	assert.Equal(t, "expired", suite.getCreditHold(alreadyExpiredHold.ID).Status)

	// Held balance should be unchanged
	updatedBalance := suite.getCreditBalance(balance.ID)
	assert.Equal(t, int64(30), updatedBalance.HeldBalance)
}

// TestHoldExpiryWorkerMultipleUserHolds tests expiring holds for multiple users
func TestHoldExpiryWorkerMultipleUserHolds(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	creditTypeName := "test-hold-expiry-multi-" + uuid.New().String()[:8]
	creditType := suite.createTestCreditType(creditTypeName)

	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)
	pastTime := now.Add(-1 * time.Hour)

	// Create 3 users with expired holds
	type testUser struct {
		userID  string
		balance *models.CreditBalance
		hold    *models.CreditTransaction
		holdAmt int64
		heldAmt int64
	}

	users := make([]testUser, 3)
	for i := 0; i < 3; i++ {
		userID := uuid.New().String()
		holdAmt := int64((i + 1) * 10) // 10, 20, 30
		heldAmt := int64((i + 1) * 20) // 20, 40, 60

		balance := suite.createTestCreditBalance(userID, creditType.ID, 100, heldAmt)
		hold := suite.createTestCreditHold(userID, creditType.ID, holdAmt, "active", pastTime)

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
		updatedHold := suite.getCreditHold(u.hold.ID)
		assert.Equal(t, "expired", updatedHold.Status, "User %d hold should be expired", i)

		updatedBalance := suite.getCreditBalance(u.balance.ID)
		expectedHeld := u.heldAmt - u.holdAmt
		assert.Equal(t, expectedHeld, updatedBalance.HeldBalance, "User %d held balance should be reduced", i)
	}
}

// TestHoldExpiryWorkerBatching tests that the worker processes in batches
func TestHoldExpiryWorkerBatching(t *testing.T) {
	suite := setupTestSuite(t)
	ctx := context.Background()

	creditTypeName := "test-hold-expiry-batch-" + uuid.New().String()[:8]
	creditType := suite.createTestCreditType(creditTypeName)

	userID := uuid.New().String()
	// Create balance with enough held credits for all holds
	totalHolds := 5
	holdAmount := int64(10)
	suite.createTestCreditBalance(userID, creditType.ID, 1000, int64(totalHolds)*holdAmount)

	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)
	pastTime := now.Add(-1 * time.Hour)

	// Create multiple expired holds
	var holds []*models.CreditTransaction
	for i := 0; i < totalHolds; i++ {
		hold := suite.createTestCreditHold(userID, creditType.ID, holdAmount, "active", pastTime)
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
		updatedHold := suite.getCreditHold(hold.ID)
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
	suite := setupTestSuite(t)
	ctx := context.Background()

	creditTypeName := "test-hold-expiry-negative-" + uuid.New().String()[:8]
	creditType := suite.createTestCreditType(creditTypeName)

	userID := uuid.New().String()
	// Create balance with less held than the hold amount (edge case)
	balance := suite.createTestCreditBalance(userID, creditType.ID, 100, 10) // Only 10 held

	now := time.Now()
	fakeClock := clockwork.NewFakeClockAt(now)
	pastTime := now.Add(-1 * time.Hour)

	// Create hold for more than held balance (shouldn't happen normally, but testing safety)
	hold := suite.createTestCreditHold(userID, creditType.ID, 50, "active", pastTime)

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
	updatedHold := suite.getCreditHold(hold.ID)
	assert.Equal(t, "expired", updatedHold.Status)

	// Held balance should be 0, not negative
	updatedBalance := suite.getCreditBalance(balance.ID)
	assert.GreaterOrEqual(t, updatedBalance.HeldBalance, int64(0), "Held balance should never be negative")
}
