package guide

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// EmbeddedJob queues the host's job and starts its running charge in one
// transaction: a crash leaves both or neither.
func EmbeddedJob(ctx context.Context, pool *pgxpool.Pool, bill *openrails.Engine, merchant billing.MerchantID, customer billing.CustomerID, jobID string) error {
	// guide:embedded-tx
	local, err := bill.LocalMerchant(merchant)
	if err != nil {
		return err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := local.StartRunningChargeTx(ctx, tx, billing.StartRunningChargeParams{
		Reference: jobID, CustomerID: customer, Currency: "USD", RatePerHour: 3_600_000,
	}); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.jobs (id, customer_id) VALUES ($1, $2)`, jobID, customer); err != nil {
		return err
	}
	return tx.Commit(ctx)
	// guide:end
}
