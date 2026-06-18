//go:build integration

package tests

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/grants"
)

// Gen returns the sqlc query catalog bound to the suite's pgx pool.
func (suite *TestContainerSuite) Gen() *gen.Queries {
	return gen.New(suite.Pool)
}

// Count runs a `SELECT COUNT(*) ...` style query ($1 placeholders) and returns
// the scalar result.
func (suite *TestContainerSuite) Count(ctx context.Context, query string, args ...any) int {
	suite.t.Helper()
	var n int
	require.NoError(suite.t, suite.Pool.QueryRow(ctx, query, args...).Scan(&n))
	return n
}

// --- model-based insert helpers (replacements for bun NewInsert().Model(x)) ---
// These delegate to the production repo layer so the JSONB/enum mapping is the
// same one the application uses.

func (suite *TestContainerSuite) InsertProduct(ctx context.Context, p *models.Product) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewProductRepo(suite.App.Runtime.DB).Create(ctx, p), "Failed to insert product")
}

func (suite *TestContainerSuite) InsertPrice(ctx context.Context, p *models.Price) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewPriceRepo(suite.App.Runtime.DB).Create(ctx, p), "Failed to insert price")
}

func (suite *TestContainerSuite) InsertSubscription(ctx context.Context, s *models.Subscription) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewSubscriptionRepo(suite.App.Runtime.DB).Create(ctx, s), "Failed to insert subscription")
}

func (suite *TestContainerSuite) InsertPaymentMethod(ctx context.Context, pm *models.PaymentMethod) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewPaymentMethodRepo(suite.App.Runtime.DB).Create(ctx, pm), "Failed to insert payment method")
}

func (suite *TestContainerSuite) InsertPayment(ctx context.Context, p *models.Payment) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewPaymentRepo(suite.App.Runtime.DB).Create(ctx, p), "Failed to insert payment")
}

func (suite *TestContainerSuite) InsertEntitlement(ctx context.Context, e *models.Entitlement) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewEntitlementRepo(suite.App.Runtime.DB).Insert(ctx, e), "Failed to insert entitlement")
}

func (suite *TestContainerSuite) InsertNotification(ctx context.Context, n *models.NotificationQueue) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewNotificationQueueRepo(suite.App.Runtime.DB).Create(ctx, n), "Failed to insert notification")
}

// insertMoneyCreditLot seeds a credit lot — a #514 credit grant — and
// materializes it into the #512 ledger (the single-entry money_blocks table is
// gone, #512 hard cut). Returns the lot (grant) id.
func (suite *TestContainerSuite) insertMoneyCreditLot(ctx context.Context, merchantID, customerID uuid.UUID, currency string, amount int64, expiresAt *time.Time, now time.Time) uuid.UUID {
	suite.t.Helper()
	gl := grants.New(gen.New(suite.Pool), merchantID)
	gl.SetClock(func() time.Time { return now })
	amt, cur := amount, currency
	g, err := gl.Grant(ctx, grants.GrantInput{
		Customer: customerID, Kind: grants.Credit, Source: grants.Admin,
		SourceID: "test_lot_" + uuid.NewString(), Amount: &amt, Currency: &cur,
		StartsAt: now, EndsAt: expiresAt,
	})
	require.NoError(suite.t, err, "create credit lot grant")
	require.NoError(suite.t, gl.MaterializeGrant(ctx, g), "materialize credit lot")
	return g.ID
}

// lotRemaining returns a credit lot's derived remaining (amount − spent − expired).
func (suite *TestContainerSuite) lotRemaining(ctx context.Context, merchantID, lotID uuid.UUID) int64 {
	suite.t.Helper()
	var remaining int64
	require.NoError(suite.t, suite.Pool.QueryRow(ctx, `
		SELECT g.amount - COALESCE((
			SELECT SUM(t.amount) FROM openrails.ledger_transfers t
			WHERE t.merchant_id = g.merchant_id AND t.grant_id = g.id
			  AND t.transfer_type IN ('credit_spend', 'credit_expire')
		), 0)
		FROM openrails.grants g WHERE g.id = $1`, lotID).Scan(&remaining))
	return remaining
}

// --- read helpers ---

// GetPaymentByID loads a payment by id (fails the test when missing).
func (suite *TestContainerSuite) GetPaymentByID(ctx context.Context, id uuid.UUID) *models.Payment {
	suite.t.Helper()
	p, err := dbrepo.NewPaymentRepo(suite.App.Runtime.DB).GetByID(ctx, id)
	require.NoError(suite.t, err, "Failed to get payment %s", id)
	return p
}

// GetPaymentByTransaction loads a payment by (processor, transaction_id);
// fails the test when missing.
func (suite *TestContainerSuite) GetPaymentByTransaction(ctx context.Context, processor models.Processor, transactionID string) *models.Payment {
	suite.t.Helper()
	p, err := dbrepo.NewPaymentRepo(suite.App.Runtime.DB).GetByTransactionID(ctx, processor, transactionID)
	require.NoError(suite.t, err, "Failed to get payment by transaction %s", transactionID)
	return p
}

// GetPaymentMethod loads a payment method by id (fails the test when missing).
func (suite *TestContainerSuite) GetPaymentMethod(ctx context.Context, id uuid.UUID) *models.PaymentMethod {
	suite.t.Helper()
	pm, err := dbrepo.NewPaymentMethodRepo(suite.App.Runtime.DB).GetByID(ctx, id)
	require.NoError(suite.t, err, "Failed to get payment method %s", id)
	return pm
}

// entitlementCols is the explicit column list QueryEntitlements scans; it
// deliberately includes deleted_at so callers control soft-delete visibility
// in their WHERE clause (bun's implicit `deleted_at IS NULL` is gone).
const entitlementCols = `id, tenant_id, customer_id, entitlement, start_at, end_at,
	source_id, source_type, revoked_at, revoke_reason, created_at, updated_at, deleted_at`

// QueryEntitlements runs a SELECT over openrails.entitlements with the given
// tail (WHERE/ORDER BY/LIMIT, $1 placeholders). NOTE: no implicit
// soft-delete filter — add "deleted_at IS NULL" where bun's Model select
// previously implied it.
func (suite *TestContainerSuite) QueryEntitlements(ctx context.Context, tail string, args ...any) []models.Entitlement {
	suite.t.Helper()
	rows, err := suite.Pool.Query(ctx, "SELECT "+entitlementCols+" FROM openrails.entitlements "+tail, args...)
	require.NoError(suite.t, err, "Failed to query entitlements")
	defer rows.Close()

	var out []models.Entitlement
	for rows.Next() {
		var e models.Entitlement
		var sourceType string
		var revokeReason *string
		require.NoError(suite.t, rows.Scan(
			&e.ID, &e.MerchantID, &e.CustomerID, &e.Entitlement, &e.StartAt, &e.EndAt,
			&e.SourceID, &sourceType, &e.RevokedAt, &revokeReason, &e.CreatedAt, &e.UpdatedAt, &e.DeletedAt,
		))
		e.SourceType = models.EntitlementSourceType(sourceType)
		if revokeReason != nil {
			rr := models.EntitlementRevokeReason(*revokeReason)
			e.RevokeReason = &rr
		}
		out = append(out, e)
	}
	require.NoError(suite.t, rows.Err())
	return out
}

// GetEntitlement loads a single non-deleted entitlement by id (fails the test
// when missing) — the equivalent of bun's soft-delete-filtered Model select.
func (suite *TestContainerSuite) GetEntitlement(ctx context.Context, id uuid.UUID) *models.Entitlement {
	suite.t.Helper()
	ents := suite.QueryEntitlements(ctx, "WHERE id = $1 AND deleted_at IS NULL", id)
	require.Len(suite.t, ents, 1, "expected entitlement %s", id)
	return &ents[0]
}

// GetSubscriptionByProcessorID retrieves a subscription by processor
// subscription ID, or nil when none exists (matching the bun-era helper that
// swallowed the not-found error).
func (suite *TestContainerSuite) GetSubscriptionByProcessorID(processorSubID string) *models.Subscription {
	suite.t.Helper()
	ctx := context.Background()

	var id uuid.UUID
	err := suite.Pool.QueryRow(ctx,
		"SELECT id FROM openrails.subscriptions WHERE processor_subscription_id = $1 LIMIT 1",
		processorSubID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	require.NoError(suite.t, err, "Failed to look up subscription by processor id %s", processorSubID)

	sub, err := dbrepo.NewSubscriptionRepo(suite.App.Runtime.DB).GetByID(ctx, id)
	if err != nil {
		return nil
	}
	return sub
}
