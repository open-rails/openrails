// Package webhookhealth records inbound-webhook liveness per event source (a
// PSP or a custodian): the verified-accepted watermark, the verification-reject
// counter and the pull-drift signal. The metrics engine reads these tables.
package webhookhealth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// Recorder writes webhook-health rows. The merchant and the source (the PSP
// or custodian the webhook plane pinned) come from ctx; every write runs in
// MerchantTx. Accepted/Rejected are telemetry: they log-and-swallow errors so
// recording can never fail a webhook.
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

// Accepted stamps the verified-accepted watermark. Call ONLY after
// verification succeeded (CCBill: after its IP-allowlist + payload gate).
func (r *Recorder) Accepted(ctx context.Context) {
	r.record(ctx, "accepted", func(ctx context.Context, q *gen.Queries, mid billing.MerchantID, psp, custodian *uuid.UUID) error {
		return q.RecordWebhookAccepted(ctx, gen.RecordWebhookAcceptedParams{
			MerchantID: mid.UUID(), PspID: psp, CustodianID: custodian, At: r.now(),
		})
	})
}

// Rejected counts a failed-verification delivery. Never touches the accepted
// watermark.
func (r *Recorder) Rejected(ctx context.Context) {
	r.record(ctx, "rejected", func(ctx context.Context, q *gen.Queries, mid billing.MerchantID, psp, custodian *uuid.UUID) error {
		return q.RecordWebhookRejected(ctx, gen.RecordWebhookRejectedParams{
			MerchantID: mid.UUID(), PspID: psp, CustodianID: custodian, At: r.now(),
		})
	})
}

func (r *Recorder) record(ctx context.Context, kind string, fn func(ctx context.Context, q *gen.Queries, mid billing.MerchantID, psp, custodian *uuid.UUID) error) {
	if r == nil || r.DB == nil {
		return
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return // no merchant to attribute to (rejected before resolution)
	}
	psp, custodian := source(ctx)
	if psp == nil && custodian == nil {
		return // no source to attribute to
	}
	err = r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, gen.New(tx), mid, psp, custodian)
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("kind", kind).
			Warn("webhook health: recording failed (webhook processing unaffected)")
	}
}

// source is the event source the webhook plane pinned on ctx: a PSP, else a
// custodian.
func source(ctx context.Context) (psp, custodian *uuid.UUID) {
	if id := db.PSPIDFromContext(ctx); id != uuid.Nil {
		return &id, nil
	}
	if id := db.CustodianIDFromContext(ctx); id != uuid.Nil {
		return nil, &id
	}
	return nil, nil
}

// Drift records n pull-derived corrections for the PSP at, gated in SQL on
// its accepted watermark predating its previous pull. Returns whether the
// gate admitted them. Merchant comes from ctx.
func Drift(ctx context.Context, database *db.DB, pspID uuid.UUID, at time.Time, n int) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	var rows int64
	err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = gen.New(tx).RecordWebhookDrift(ctx, gen.RecordWebhookDriftParams{
			MerchantID: mid.UUID(), PspID: pspID, At: at.UTC(), N: int64(n),
		})
		return err
	})
	return rows > 0, err
}

// StampPull advances the PSP's pull watermark after a completed refresh pass.
// Merchant comes from ctx.
func StampPull(ctx context.Context, database *db.DB, pspID uuid.UUID, at time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	return database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return gen.New(tx).StampWebhookPull(ctx, gen.StampWebhookPullParams{
			MerchantID: mid.UUID(), PspID: pspID, At: at.UTC(),
		})
	})
}
