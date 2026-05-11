package entitlements

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/uptrace/bun"
)

type EntitlementService struct {
	db    *db.DB
	repo  *repo.EntitlementRepo
	clock clockwork.Clock
}

func NewEntitlementService(db *db.DB, clocks ...clockwork.Clock) *EntitlementService {
	return &EntitlementService{db: db, repo: repo.NewEntitlementRepo(db), clock: firstClock(clocks...)}
}

func (s *EntitlementService) withTx(ctx context.Context, fn func(ctx context.Context, tx bun.Tx) error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	switch dbi := s.db.GetDB().(type) {
	case *bun.DB:
		return dbi.RunInTx(ctx, nil, fn)
	case bun.Tx:
		return fn(ctx, dbi)
	default:
		return fmt.Errorf("unsupported db type for entitlement transaction")
	}
}

// SetClock sets the clock for this service. Used for testing.
func (s *EntitlementService) SetClock(c clockwork.Clock) {
	s.clock = firstClock(c)
}

func (s *EntitlementService) Clock() clockwork.Clock {
	return s.clock
}

// now returns the current time from the service's clock, or time.Now() if no clock is set.
func (s *EntitlementService) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

func firstClock(clocks ...clockwork.Clock) clockwork.Clock {
	for _, c := range clocks {
		if c != nil {
			return c
		}
	}
	return clockwork.NewRealClock()
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

func (s *EntitlementService) ListDistinctEntitlementNamesBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID) ([]string, error) {
	return s.repo.ListDistinctEntitlementNamesBySource(ctx, sourceType, sourceID)
}

// ListActiveEntitlements returns a de-duplicated list of active entitlement names for a user at a point in time.
func (s *EntitlementService) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	return s.repo.ListActiveEntitlements(ctx, userID, at)
}

// GetByID retrieves an entitlement by its ID
func (s *EntitlementService) GetByID(ctx context.Context, id uuid.UUID) (*models.Entitlement, error) {
	return s.repo.GetByID(ctx, id)
}

type PushNewEntitlementParams struct {
	UserID      string
	Entitlement string

	// NotBefore allows callers to delay the start of the new window.
	// The final start_at is max(NotBefore, tail_end, now).
	NotBefore *time.Time

	// Exactly one of (Indefinite, Duration, EndAt) must be set.
	Indefinite bool
	Duration   *time.Duration
	EndAt      *time.Time

	SourceType models.EntitlementSourceType
	SourceID   uuid.UUID
}

// PushNewEntitlement appends a new entitlement window to the per-(user_id, entitlement) timeline.
// It does not mutate existing windows (end_at is immutable); it schedules the new window to start
// after the current tail end (or now), optionally honoring NotBefore.
//
// If EndAt is provided and EndAt <= computed start_at, this is covered by the
// existing canonical timeline and returns the covering row when one can be found.
func (s *EntitlementService) PushNewEntitlement(ctx context.Context, p PushNewEntitlementParams) (*models.Entitlement, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("entitlement service not initialized")
	}
	if p.UserID == "" || p.Entitlement == "" {
		return nil, fmt.Errorf("userID and entitlement are required")
	}
	if p.SourceID == uuid.Nil {
		return nil, fmt.Errorf("sourceID is required")
	}
	setCount := 0
	if p.Indefinite {
		setCount++
	}
	if p.Duration != nil {
		setCount++
	}
	if p.EndAt != nil {
		setCount++
	}
	if setCount != 1 {
		return nil, fmt.Errorf("exactly one of Indefinite, Duration, or EndAt must be set")
	}
	if p.Duration != nil && *p.Duration <= 0 {
		return nil, fmt.Errorf("duration must be > 0")
	}
	if p.EndAt != nil && p.EndAt.IsZero() {
		return nil, fmt.Errorf("endAt must be non-zero")
	}

	now := s.now().UTC()
	var created *models.Entitlement

	err := s.withTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := repo.LockEntitlementTimeline(ctx, tx, p.UserID, p.Entitlement); err != nil {
			return err
		}

		// If an indefinite entitlement exists, the timeline is terminal.
		var hasIndefinite bool
		if err := tx.NewSelect().
			Model((*models.Entitlement)(nil)).
			ColumnExpr("COUNT(*) > 0").
			Where("ent.user_id = ?", p.UserID).
			Where("ent.entitlement = ?", p.Entitlement).
			Where("ent.revoked_at IS NULL").
			Where("ent.deleted_at IS NULL").
			Where("ent.end_at IS NULL").
			Scan(ctx, &hasIndefinite); err != nil {
			return err
		}
		if hasIndefinite {
			created = new(models.Entitlement)
			return tx.NewSelect().
				Model(created).
				Where("ent.user_id = ?", p.UserID).
				Where("ent.entitlement = ?", p.Entitlement).
				Where("ent.revoked_at IS NULL").
				Where("ent.deleted_at IS NULL").
				Where("ent.end_at IS NULL").
				OrderExpr("ent.start_at ASC").
				Limit(1).
				Scan(ctx)
		}

		var tailEnd *time.Time
		if err := tx.NewSelect().
			Model((*models.Entitlement)(nil)).
			ColumnExpr("MAX(ent.end_at)").
			Where("ent.user_id = ?", p.UserID).
			Where("ent.entitlement = ?", p.Entitlement).
			Where("ent.revoked_at IS NULL").
			Where("ent.deleted_at IS NULL").
			Where("ent.end_at IS NOT NULL").
			Scan(ctx, &tailEnd); err != nil {
			return err
		}

		start := now
		if p.NotBefore != nil {
			nb := p.NotBefore.UTC()
			if nb.After(start) {
				start = nb
			}
		}
		if tailEnd != nil && tailEnd.After(start) {
			start = *tailEnd
		}

		var endAt *time.Time
		switch {
		case p.Indefinite:
			endAt = nil
		case p.Duration != nil:
			e := start.Add(*p.Duration)
			endAt = &e
		case p.EndAt != nil:
			e := p.EndAt.UTC()
			if !e.After(start) {
				covered := new(models.Entitlement)
				err := tx.NewSelect().
					Model(covered).
					Where("ent.user_id = ?", p.UserID).
					Where("ent.entitlement = ?", p.Entitlement).
					Where("ent.revoked_at IS NULL").
					Where("ent.deleted_at IS NULL").
					Where("ent.start_at < ?", e).
					Where("ent.end_at IS NULL OR ent.end_at >= ?", e).
					OrderExpr("ent.end_at DESC NULLS LAST").
					Limit(1).
					Scan(ctx)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return fmt.Errorf("requested entitlement window is already covered by timeline tail but no covering row was found")
					}
					return err
				}
				created = covered
				return nil
			}
			endAt = &e
		}

		created = &models.Entitlement{
			ID:          uuid.New(),
			UserID:      p.UserID,
			Entitlement: p.Entitlement,
			StartAt:     start,
			EndAt:       endAt,
			SourceType:  p.SourceType,
			SourceID:    &p.SourceID,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		_, err := tx.NewInsert().Model(created).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

func (s *EntitlementService) ExtendActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time) error {
	if s == nil || s.repo == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	return s.repo.ExtendActiveBySubscription(ctx, subscriptionID, endAt.UTC(), s.now().UTC())
}

func (s *EntitlementService) EndActiveByPayment(ctx context.Context, paymentID uuid.UUID, reason models.EntitlementRevokeReason) error {
	if s == nil || s.repo == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	now := s.now().UTC()
	return s.repo.EndActiveByPayment(ctx, paymentID, now, now, &reason)
}

func (s *EntitlementService) RevokeSourcesForSubscription(ctx context.Context, userID string, subscriptionID uuid.UUID, reason models.EntitlementRevokeReason, sourceTypes ...models.EntitlementSourceType) error {
	if s == nil || s.repo == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	for _, sourceType := range sourceTypes {
		names, err := s.ListDistinctEntitlementNamesBySource(ctx, sourceType, subscriptionID)
		if err != nil {
			return fmt.Errorf("list %s entitlements: %w", sourceType, err)
		}
		st := sourceType
		sid := subscriptionID
		for _, entName := range names {
			if err := s.RevokeExistingEntitlement(ctx, RevokeExistingEntitlementParams{
				UserID:      userID,
				Entitlement: entName,
				SourceType:  &st,
				SourceID:    &sid,
				Reason:      reason,
			}); err != nil {
				return fmt.Errorf("revoke %s entitlement %s: %w", sourceType, entName, err)
			}
		}
	}
	return nil
}

type RevokeExistingEntitlementParams struct {
	// Exactly one of EntitlementID or (UserID+Entitlement) must be provided.
	EntitlementID *uuid.UUID
	UserID        string
	Entitlement   string

	// Optional filters to only affect windows from a specific source.
	SourceType *models.EntitlementSourceType
	SourceID   *uuid.UUID

	Reason models.EntitlementRevokeReason
}

// RevokeExistingEntitlement immediately removes access by:
// - revoking any active entitlement window(s) at now (revoked_at + revoke_reason)
// - soft-deleting any future scheduled windows
//
// It does not mutate end_at of existing windows (end_at is immutable).
func (s *EntitlementService) RevokeExistingEntitlement(ctx context.Context, p RevokeExistingEntitlementParams) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	if p.EntitlementID == nil && (p.UserID == "" || p.Entitlement == "") {
		return fmt.Errorf("entitlementID or (userID, entitlement) is required")
	}
	if p.EntitlementID != nil && (p.UserID != "" || p.Entitlement != "") {
		return fmt.Errorf("provide either entitlementID or (userID, entitlement), not both")
	}

	now := s.now().UTC()
	return s.withTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		userID := p.UserID
		entitlement := p.Entitlement
		if p.EntitlementID != nil {
			ent, err := repo.GetEntitlementByIDTx(ctx, tx, *p.EntitlementID)
			if err != nil {
				return err
			}
			userID = ent.UserID
			entitlement = ent.Entitlement
			if err := repo.LockEntitlementTimeline(ctx, tx, userID, entitlement); err != nil {
				return err
			}
			if ent.RevokedAt != nil || ent.DeletedAt != nil {
				return nil
			}
			if p.SourceType != nil && ent.SourceType != *p.SourceType {
				return nil
			}
			if p.SourceID != nil {
				if ent.SourceID == nil || *ent.SourceID != *p.SourceID {
					return nil
				}
			}
			if ent.StartAt.After(now) {
				_, err := tx.NewUpdate().
					Model((*models.Entitlement)(nil)).
					Set("deleted_at = ?", now).
					Set("updated_at = ?", now).
					Where("ent.id = ?", ent.ID).
					Where("ent.revoked_at IS NULL").
					Where("ent.deleted_at IS NULL").
					Exec(ctx)
				return err
			}
			if ent.EndAt == nil || ent.EndAt.After(now) {
				_, err := tx.NewUpdate().
					Model((*models.Entitlement)(nil)).
					Set("revoked_at = ?", now).
					Set("revoke_reason = ?", &p.Reason).
					Set("updated_at = ?", now).
					Where("ent.id = ?", ent.ID).
					Where("ent.revoked_at IS NULL").
					Where("ent.deleted_at IS NULL").
					Exec(ctx)
				return err
			}
			return nil
		}

		if err := repo.LockEntitlementTimeline(ctx, tx, userID, entitlement); err != nil {
			return err
		}

		active := tx.NewUpdate().
			Model((*models.Entitlement)(nil)).
			Set("revoked_at = ?", now).
			Set("revoke_reason = ?", &p.Reason).
			Set("updated_at = ?", now).
			Where("ent.user_id = ?", userID).
			Where("ent.entitlement = ?", entitlement).
			Where("ent.revoked_at IS NULL").
			Where("ent.deleted_at IS NULL").
			Where("ent.start_at <= ?", now).
			Where("(ent.end_at IS NULL OR ent.end_at > ?)", now)

		future := tx.NewUpdate().
			Model((*models.Entitlement)(nil)).
			Set("deleted_at = ?", now).
			Set("updated_at = ?", now).
			Where("ent.user_id = ?", userID).
			Where("ent.entitlement = ?", entitlement).
			Where("ent.revoked_at IS NULL").
			Where("ent.deleted_at IS NULL").
			Where("ent.start_at > ?", now)

		if p.SourceType != nil {
			active = active.Where("ent.source_type = ?", *p.SourceType)
			future = future.Where("ent.source_type = ?", *p.SourceType)
		}
		if p.SourceID != nil && *p.SourceID != uuid.Nil {
			active = active.Where("ent.source_id = ?", *p.SourceID)
			future = future.Where("ent.source_id = ?", *p.SourceID)
		}

		if _, err := active.Exec(ctx); err != nil {
			return err
		}
		_, err := future.Exec(ctx)
		return err
	})
}
