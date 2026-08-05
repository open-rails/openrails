package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// #813 embedded-facade parity for plan migrations: hosts (e.g. cozy-art)
// drive the operator workflow in-process — same service the merchant HTTP
// routes wrap. Requests/results are the module types re-exported so embedded
// callers never import internal/.
//
// Every method here pins a merchant-scoped connection the way the money
// surfaces do (spend.go). Without it, a host calling in-process under the
// or#885-mandated openrails_app role read NOTHING — the reads resolved to the
// base pool, where `app.merchant_id` is unset, so RLS answered zero rows and no
// error and Preview reported "source price: no rows" about a price the same
// host had just written (or#900). RunInMerchantConn reuses an already-pinned
// connection, so this is correct from a request, a worker, or a bare Go call.

// PlanMigrationRequest re-exports the operator request.
type PlanMigrationRequest = subscriptions.PlanMigrationRequest

// PlanMigrationResult re-exports the batch + per-subscription ledger result.
type PlanMigrationResult = subscriptions.PlanMigrationResult

// PlanMigrationOutcome re-exports one subscription's classification — a LEAF of
// PlanMigrationResult. Re-exported (#814) because a host that could name the
// root but not its leaves had to mirror them field-by-field, which drifts on
// every upstream change.
type PlanMigrationOutcome = subscriptions.PlanMigrationOutcome

// RailCounts re-exports the per-rail capability summary in
// PlanMigrationResult.ByRail.
type RailCounts = subscriptions.RailCounts

// PlanMigrationCancelResult re-exports CancelPlanMigration's result.
type PlanMigrationCancelResult = subscriptions.PlanMigrationCancelResult

func (s *Service) planMigrations() (*subscriptions.PlanMigrationService, error) {
	if s.rt.PlanMigrationService == nil {
		return nil, fmt.Errorf("billing service: plan migration service unavailable")
	}
	return s.rt.PlanMigrationService, nil
}

// PreviewPlanMigration classifies a plan migration's cohort without writing
// anything — per-rail auto/requires-action/skip counts plus the full
// per-subscription ledger. The operator's commit gate.
func (s *Service) PreviewPlanMigration(ctx context.Context, req PlanMigrationRequest) (*PlanMigrationResult, error) {
	pm, err := s.planMigrations()
	if err != nil {
		return nil, err
	}
	var out *PlanMigrationResult
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		out, e = pm.Preview(ctx, req)
		return e
	})
	return out, err
}

// PlanMigrate commits a plan migration: batch header + per-subscription rows,
// source-price archive, rail pushes, schedule-time plan-change notices.
func (s *Service) PlanMigrate(ctx context.Context, req PlanMigrationRequest) (*PlanMigrationResult, error) {
	pm, err := s.planMigrations()
	if err != nil {
		return nil, err
	}
	var out *PlanMigrationResult
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		out, e = pm.Migrate(ctx, req)
		return e
	})
	return out, err
}

// GetPlanMigration returns one migration batch header + its ledger rows.
func (s *Service) GetPlanMigration(ctx context.Context, batchID uuid.UUID, limit, offset int) (*models.RepriceBatch, []*models.SubscriptionReprice, error) {
	pm, err := s.planMigrations()
	if err != nil {
		return nil, nil, err
	}
	var batch *models.RepriceBatch
	var rows []*models.SubscriptionReprice
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		batch, rows, e = pm.GetBatch(ctx, batchID, limit, offset)
		return e
	})
	return batch, rows, err
}

// CancelPlanMigration cancels every still-scheduled row in the batch. The
// result carries RailReleaseRequired + Warning when Stripe-side schedules
// survive the cancel and must be released out of band.
func (s *Service) CancelPlanMigration(ctx context.Context, batchID uuid.UUID) (*PlanMigrationCancelResult, error) {
	pm, err := s.planMigrations()
	if err != nil {
		return nil, err
	}
	var out *PlanMigrationCancelResult
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		var e error
		out, e = pm.CancelBatch(ctx, batchID)
		return e
	})
	return out, err
}
