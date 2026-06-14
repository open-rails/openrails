//go:build integration

package tests

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	dbrepo "github.com/open-rails/openrails/internal/db/repo"
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

func (suite *TestContainerSuite) InsertEntitlementGrant(ctx context.Context, g *models.EntitlementGrant) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewEntitlementGrantRepo(suite.App.Runtime.DB).Create(ctx, g), "Failed to insert admin grant")
}

func (suite *TestContainerSuite) InsertNotification(ctx context.Context, n *models.NotificationQueue) {
	suite.t.Helper()
	require.NoError(suite.t, dbrepo.NewNotificationQueueRepo(suite.App.Runtime.DB).Create(ctx, n), "Failed to insert notification")
}

// insertMoneyBlock inserts a money lot (openrails.money_blocks). The caller
// must set MerchantID, CustomerID, and Currency.
func (suite *TestContainerSuite) insertMoneyBlock(ctx context.Context, b *models.MoneyBlock) {
	suite.t.Helper()
	_, err := suite.Pool.Exec(ctx, `
		INSERT INTO openrails.money_blocks (
			id, tenant_id, customer_id, currency, original_amount,
			remaining_amount, expires_at, source_transaction_id, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		b.ID, b.MerchantID, b.CustomerID, b.Currency, b.OriginalAmount,
		b.RemainingAmount, b.ExpiresAt, b.SourceTransactionID, b.CreatedAt)
	require.NoError(suite.t, err, "Failed to insert money block")
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

// GetEntitlementGrant loads an admin grant by id (fails the test when missing).
func (suite *TestContainerSuite) GetEntitlementGrant(ctx context.Context, id uuid.UUID) *models.EntitlementGrant {
	suite.t.Helper()
	g, err := dbrepo.NewEntitlementGrantRepo(suite.App.Runtime.DB).GetByID(ctx, id)
	require.NoError(suite.t, err, "Failed to get admin grant %s", id)
	return g
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
