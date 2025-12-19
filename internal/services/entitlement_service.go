package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/doujins-org/doujins-billing/internal/db"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/doujins-org/doujins-billing/internal/db/repo"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
)

type EntitlementService struct {
	repo  *repo.EntitlementRepo
	Clock clockwork.Clock
}

func NewEntitlementService(db *db.DB) *EntitlementService {
	return &EntitlementService{repo: repo.NewEntitlementRepo(db)}
}

// SetClock sets the clock for this service. Used for testing.
func (s *EntitlementService) SetClock(c clockwork.Clock) {
	s.Clock = c
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *EntitlementService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

// IsEntitled returns true if the user currently has an active entitlement
func (s *EntitlementService) IsEntitled(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	return s.repo.IsEntitled(ctx, userID, entitlement, at)
}

func (s *EntitlementService) HasActiveIndefinite(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	return s.repo.HasActiveIndefinite(ctx, userID, entitlement, at)
}

func (s *EntitlementService) ExistsBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID, entitlement string) (bool, error) {
	return s.repo.ExistsBySource(ctx, sourceType, sourceID, entitlement)
}

func (s *EntitlementService) LatestFiniteWindow(ctx context.Context, userID, entitlement string, at time.Time) (*models.Entitlement, error) {
	return s.repo.GetLatestFiniteActive(ctx, userID, entitlement, at)
}

func (s *EntitlementService) ListByUser(ctx context.Context, userID string) ([]models.Entitlement, error) {
	return s.repo.ListByUser(ctx, userID)
}

func (s *EntitlementService) ListActiveRecords(ctx context.Context, userID string, at time.Time) ([]models.Entitlement, error) {
	return s.repo.ListActiveRecords(ctx, userID, at)
}

// AppendEntitlementDays appends a finite window of N days right after the latest window's end,
// unless the user currently has an indefinite window (end_at IS NULL). In that case, it is a no-op.
func (s *EntitlementService) AppendEntitlementDays(ctx context.Context, userID, entitlement string, days int, sourceType models.EntitlementSourceType, sourceID *uuid.UUID) (*models.Entitlement, error) {
	if days <= 0 {
		return nil, fmt.Errorf("days must be > 0")
	}
	now := s.now()

	exists, err := s.repo.HasActiveIndefinite(ctx, userID, entitlement, now)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("cannot append entitlement while subscription entitlement is active")
	}

	var start time.Time = now
	last, err := s.repo.GetLatestActive(ctx, userID, entitlement)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && last != nil && last.EndAt != nil {
		start = *last.EndAt
	}

	end := start.Add(time.Duration(days) * 24 * time.Hour)
	return s.GrantWindow(ctx, userID, entitlement, start, &end, sourceType, sourceID)
}

// ListActiveEntitlements returns a de-duplicated list of active entitlement names for a user at a point in time.
func (s *EntitlementService) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	return s.repo.ListActiveEntitlements(ctx, userID, at)
}

// GrantWindow creates a new entitlement window for a user
func (s *EntitlementService) GrantWindow(ctx context.Context, userID, entitlement string, startAt time.Time, endAt *time.Time, sourceType models.EntitlementSourceType, sourceID *uuid.UUID) (*models.Entitlement, error) {
	now := s.now()
	ent := &models.Entitlement{
		ID:          uuid.New(),
		UserID:      userID,
		Entitlement: entitlement,
		StartAt:     startAt,
		EndAt:       endAt,
		SourceType:  sourceType,
		SourceID:    sourceID,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := s.repo.Insert(ctx, ent); err != nil {
		return nil, err
	}
	return ent, nil
}

// EndActiveBySubscription ends active entitlements for a subscription at a given time
func (s *EntitlementService) EndActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time, reason *models.EntitlementRevokeReason) error {
	now := s.now()
	return s.repo.EndActiveBySubscription(ctx, subscriptionID, endAt, now, reason)
}

// ResumeBySubscription clears scheduled end_at for a subscription's entitlements.
func (s *EntitlementService) ResumeBySubscription(ctx context.Context, subscriptionID uuid.UUID) error {
	now := s.now()
	return s.repo.ResumeBySubscription(ctx, subscriptionID, now)
}

// EndActiveByPayment ends active entitlements for a one-off payment at a given time
func (s *EntitlementService) EndActiveByPayment(ctx context.Context, paymentID uuid.UUID, endAt time.Time, reason *models.EntitlementRevokeReason) error {
	now := s.now()
	return s.repo.EndActiveByPayment(ctx, paymentID, endAt, now, reason)
}

// GetByID retrieves an entitlement by its ID
func (s *EntitlementService) GetByID(ctx context.Context, id uuid.UUID) (*models.Entitlement, error) {
	return s.repo.GetByID(ctx, id)
}

// RevokeByID immediately revokes an entitlement by ID (admin action)
func (s *EntitlementService) RevokeByID(ctx context.Context, id uuid.UUID, reason models.EntitlementRevokeReason) error {
	now := s.now()
	return s.repo.RevokeByID(ctx, id, now, reason)
}

// RevokeBySubscriptionAndName revokes a specific entitlement by subscription and name.
// Used during downgrades to revoke entitlements that the new tier doesn't include.
func (s *EntitlementService) RevokeBySubscriptionAndName(ctx context.Context, subscriptionID uuid.UUID, entitlement string, revokeAt time.Time, reason models.EntitlementRevokeReason) error {
	return s.repo.RevokeBySubscriptionAndName(ctx, subscriptionID, entitlement, revokeAt, reason)
}

// GrantEntitlement creates a new entitlement for a user (admin action)
// If endAt is nil, the entitlement is indefinite (until manually revoked)
func (s *EntitlementService) GrantEntitlement(ctx context.Context, userID, entitlement string, endAt *time.Time) (*models.Entitlement, error) {
	now := s.now()
	return s.GrantWindow(ctx, userID, entitlement, now, endAt, models.EntitlementSourceAdmin, nil)
}
