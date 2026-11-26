package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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
		if err == nil {
			if claimed < mrrRiskBatch {
				return nil
			}
			continue
		}
		if !evaluationFailed(ctx, err) {
			return err
		}
		// A batch failed: its customers one at a time, so one that still
		// fails is set aside and the rest drain.
		progressed, alone := r.oneByOne(ctx, id)
		if alone != nil {
			return alone
		}
		if !progressed {
			return err
		}
	}
}

// oneByOne evaluates the merchant's queued customers one at a time, setting
// aside any whose evaluation fails alone. It reports whether it took or set
// aside any.
func (r *MRRRisk) oneByOne(ctx context.Context, id uuid.UUID) (bool, error) {
	customers, err := r.DB.Gen(ctx).ListMRRRiskDueCustomers(ctx, gen.ListMRRRiskDueCustomersParams{MerchantID: id, RowLimit: mrrRiskBatch})
	if err != nil {
		return false, err
	}
	progressed := false
	for _, customer := range customers {
		claimed, err := r.DB.Gen(ctx).EffectDueMRRRisk(ctx, gen.EffectDueMRRRiskParams{MerchantID: id, Batch: math.MaxInt32, CustomerID: &customer})
		if err == nil {
			progressed = progressed || claimed > 0
			continue
		}
		if !evaluationFailed(ctx, err) {
			return progressed, err
		}
		if err := r.setAside(ctx, id, customer, err); err != nil {
			return progressed, err
		}
		progressed = true
	}
	return progressed, nil
}

// EvaluationFailedFindingType is the finding a customer whose at-risk MRR
// could not be re-evaluated opens; its queued changes are retried hourly.
const EvaluationFailedFindingType = "mrr.risk.evaluation_failed"

// setAside holds customer's queued changes for an hour and opens its finding.
func (r *MRRRisk) setAside(ctx context.Context, id, customer uuid.UUID, cause error) error {
	err := r.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := q.SetAsideMRRRiskCustomer(ctx, gen.SetAsideMRRRiskCustomerParams{MerchantID: id, CustomerID: customer}); err != nil {
			return err
		}
		action := "OpenRails could not re-evaluate this customer's at-risk MRR, so it may be stale. Its queued changes are retried hourly and the finding closes once one succeeds; find and fix the cause: " + cause.Error()
		evidence, err := json.Marshal(map[string]string{"customer_id": customer.String(), "error": cause.Error()})
		if err != nil {
			return err
		}
		_, err = q.UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
			MerchantID: id, FindingType: EvaluationFailedFindingType, SubjectKey: customer.String(),
			Severity: "medium", Status: "requires_review", RecommendedAction: &action, Evidence: evidence,
		})
		return err
	})
	if err == nil {
		r.Log.WarnContext(ctx, "mrr risk: a customer's evaluation failed; set aside", logx.Err(cause), "merchant_id", id, "customer_id", customer)
	}
	return err
}

// evaluationFailed reports an error the evaluation itself raised, as opposed
// to one of the connection, the server or a transaction's concurrency, which
// the next drain retries as it is.
func evaluationFailed(ctx context.Context, err error) bool {
	var pgErr *pgconn.PgError
	if ctx.Err() != nil || !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "08", "40", "53", "55", "57", "58":
		return false
	}
	return true
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

// listen rings wake for this schema's notifications, and on every
// (re)connect for those queued while nothing listened, on a session of its own.
func (r *MRRRisk) listen(ctx context.Context, wake chan<- struct{}) {
	schema := r.DB.DataPool().Schema()
	ring := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	r.DB.Listen(ctx, db.Listener{
		Channel:   mrrRiskChannel,
		Connected: func(context.Context) { ring() },
		Notified: func(_ context.Context, payload string) {
			if payload == schema {
				ring()
			}
		},
		Interrupted: func(ctx context.Context, err error) {
			r.Log.WarnContext(ctx, "mrr risk: wake notifications interrupted; reconnecting", logx.Err(err))
		},
	})
}
