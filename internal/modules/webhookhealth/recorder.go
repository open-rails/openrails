// Package webhookhealth records inbound-webhook liveness per (merchant, rail)
// (#786): verified-accepted watermark, signature-reject counter, and the
// pull-drift signal the provider-refresh job stamps. The #733 metrics engine
// reads the tables; the #736 webhook_* alert templates ride those metrics.
package webhookhealth

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Recorder writes webhook-health rows. Merchant comes from ctx; every write
// runs in MerchantTx so RLS holds on app-role pools. Accepted/Rejected are
// telemetry: they log-and-swallow errors so recording can never fail a webhook.
type Recorder struct {
	DB    *db.DB
	Clock clockwork.Clock
}

func (r *Recorder) now() time.Time {
	if r != nil && r.Clock != nil {
		return r.Clock.Now().UTC()
	}
	return time.Now().UTC()
}

// Accepted stamps the verified-accepted watermark. Call ONLY after signature
// verification succeeded (CCBill: after its IP-allowlist + payload gate).
func (r *Recorder) Accepted(ctx context.Context, rail string) {
	r.record(ctx, rail, "accepted", func(ctx context.Context, q *gen.Queries, mid merchant.ID) error {
		return q.RecordWebhookAccepted(ctx, gen.RecordWebhookAcceptedParams{
			MerchantID: mid.UUID(), Rail: rail, At: r.now(),
		})
	})
}

// Rejected counts a failed-verification delivery. Never touches the accepted
// watermark.
func (r *Recorder) Rejected(ctx context.Context, rail string) {
	r.record(ctx, rail, "rejected", func(ctx context.Context, q *gen.Queries, mid merchant.ID) error {
		return q.RecordWebhookRejected(ctx, gen.RecordWebhookRejectedParams{
			MerchantID: mid.UUID(), Rail: rail, At: r.now(),
		})
	})
}

func (r *Recorder) record(ctx context.Context, rail, kind string, fn func(ctx context.Context, q *gen.Queries, mid merchant.ID) error) {
	if r == nil || r.DB == nil || rail == "" {
		return
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return // no merchant to attribute to (e.g. rejected before resolution)
	}
	err = r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, gen.New(tx), mid)
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{"rail": rail, "kind": kind}).
			Warn("webhook health: recording failed (webhook processing unaffected)")
	}
}

// Drift records n pull-derived corrections for the rail at, gated in SQL on
// the accepted watermark predating the previous pull. Returns whether the gate
// admitted them. Merchant comes from ctx.
func Drift(ctx context.Context, database *db.DB, rail string, at time.Time, n int) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	var rows int64
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = gen.New(tx).RecordWebhookDrift(ctx, gen.RecordWebhookDriftParams{
			MerchantID: mid.UUID(), Rail: rail, At: at.UTC(), N: int64(n),
		})
		return err
	})
	return rows > 0, err
}

// StampPull advances the rail's pull watermark after a completed refresh pass.
// Merchant comes from ctx.
func StampPull(ctx context.Context, database *db.DB, rail string, at time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return gen.New(tx).StampWebhookPull(ctx, gen.StampWebhookPullParams{
			MerchantID: mid.UUID(), Rail: rail, At: at.UTC(),
		})
	})
}
