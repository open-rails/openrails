package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/logx"
)

// mrrRiskChannel is what billing.notify_mrr_risk_due notifies at the commit of
// a queued change; its payload is the schema.
const mrrRiskChannel = "openrails_mrr_risk"

// mrrRiskBatch bounds one claim and its transaction.
const mrrRiskBatch = 100

// MRRRisk re-evaluates the risk of the customers a change outside their
// subscriptions queued in billing.mrr_risk_due: a card, a default card, a
// provider's decline or delinquency. Workers claim disjoint changes, so any
// number may drain at once.
type MRRRisk struct {
	DB  *db.DB
	Log *slog.Logger
}

// Drain claims every queued change, a batch per transaction in its merchant's
// scope. A merchant that fails stays queued and fails the drain once the
// others are done.
func (r *MRRRisk) Drain(ctx context.Context) error {
	merchants, err := r.DB.GenDirectory().ListMRRRiskDueMerchants(ctx)
	if err != nil {
		return err
	}
	var failed error
	for _, id := range merchants {
		err := r.DB.RunInMerchantScope(ctx, billing.MerchantID(id), "mrr risk", func(ctx context.Context) error {
			return r.drainMerchant(ctx, id)
		})
		if ctx.Err() != nil {
			return errors.Join(failed, ctx.Err())
		}
		if err != nil {
			failed = errors.Join(failed, fmt.Errorf("mrr risk: merchant %s: %w", id, err))
		}
	}
	return failed
}

func (r *MRRRisk) drainMerchant(ctx context.Context, id uuid.UUID) error {
	for {
		claimed, err := r.DB.Gen(ctx).EffectDueMRRRisk(ctx, gen.EffectDueMRRRiskParams{MerchantID: id, Batch: mrrRiskBatch})
		if err != nil || claimed < mrrRiskBatch {
			return err
		}
	}
}

// Run drains on every wake until ctx ends: each commit that queued a change,
// and every (re)connect, for those queued while nothing listened.
func (r *MRRRisk) Run(ctx context.Context) {
	if r.DB.Pool() == nil {
		return
	}
	wake := make(chan struct{}, 1)
	listening := make(chan struct{})
	go func() { defer close(listening); r.listen(ctx, wake) }()
	defer func() { <-listening }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
		if err := r.Drain(ctx); err != nil && ctx.Err() == nil {
			r.Log.WarnContext(ctx, "mrr risk: drain failed; the periodic drain retries", logx.Err(err))
		}
	}
}

// listen rings wake for this schema's notifications, on a dedicated session.
func (r *MRRRisk) listen(ctx context.Context, wake chan<- struct{}) {
	schema := r.DB.DataPool().Schema()
	ring := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	for ctx.Err() == nil {
		err := func() error {
			conn, err := r.DB.ConnectSession(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
			if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{mrrRiskChannel}.Sanitize()); err != nil {
				return err
			}
			ring()
			for {
				n, err := conn.WaitForNotification(ctx)
				if err != nil {
					return err
				}
				if n.Payload == schema {
					ring()
				}
			}
		}()
		if err != nil && ctx.Err() == nil {
			r.Log.WarnContext(ctx, "mrr risk: wake notifications interrupted; reconnecting", logx.Err(err))
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}
