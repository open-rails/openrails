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

// PlanMigrationRequest re-exports the operator request.
type PlanMigrationRequest = subscriptions.PlanMigrationRequest

// PlanMigrationResult re-exports the batch + per-subscription ledger result.
type PlanMigrationResult = subscriptions.PlanMigrationResult

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
	return pm.Preview(ctx, req)
}

// PlanMigrate commits a plan migration: batch header + per-subscription rows,
// source-price archive, rail pushes, schedule-time plan-change notices.
func (s *Service) PlanMigrate(ctx context.Context, req PlanMigrationRequest) (*PlanMigrationResult, error) {
	pm, err := s.planMigrations()
	if err != nil {
		return nil, err
	}
	return pm.Migrate(ctx, req)
}

// GetPlanMigration returns one migration batch header + its ledger rows.
func (s *Service) GetPlanMigration(ctx context.Context, batchID uuid.UUID, limit, offset int) (*models.RepriceBatch, []*models.SubscriptionReprice, error) {
	pm, err := s.planMigrations()
	if err != nil {
		return nil, nil, err
	}
	return pm.GetBatch(ctx, batchID, limit, offset)
}

// CancelPlanMigration cancels every still-scheduled row in the batch,
// returning the number canceled.
func (s *Service) CancelPlanMigration(ctx context.Context, batchID uuid.UUID) (int, error) {
	pm, err := s.planMigrations()
	if err != nil {
		return 0, err
	}
	return pm.CancelBatch(ctx, batchID)
}
