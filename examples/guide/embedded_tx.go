package guide

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// EmbeddedJob queues the host's job and its admission in one transaction:
// a crash leaves both or neither.
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
	verdicts, err := local.AdmitTx(ctx, tx, []billing.AdmitParams{{
		RequestID: jobID, CustomerID: customer, Invoker: customer.String(), Currency: "USD",
		Usage:     &billing.UsageEstimate{EventType: "render", Dimensions: map[string]int64{"frames": 1200}},
		ExpiresAt: new(time.Now().Add(time.Hour)),
	}})
	if err != nil {
		return err
	}
	if !verdicts[0].Allowed() {
		return errors.New("refused") // verdicts[0] says why
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.jobs (id, customer_id) VALUES ($1, $2)`, jobID, customer); err != nil {
		return err
	}
	return tx.Commit(ctx)
	// guide:end
}
