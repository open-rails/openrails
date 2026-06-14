package entitlements

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

type EntitlementService struct {
	db    *db.DB
	repo  *repo.EntitlementRepo
	clock clockwork.Clock
}

func NewEntitlementService(db *db.DB, clocks ...clockwork.Clock) *EntitlementService {
	return &EntitlementService{db: db, repo: repo.NewEntitlementRepo(db), clock: timeutil.FirstClock(clocks...)}
}

func (s *EntitlementService) withTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	// Run inside a tenant-scoped transaction so the migration-050 RLS policies
	// constrain every entitlement query to the request's tenant (db.TenantTx
	// sets the app.tenant_id GUC from the context as the first statement). This is
	// the shared chokepoint for tenant-owned entitlement writes/reads.
	return s.db.TenantTx(ctx, fn)
}

// SetClock sets the clock for this service. Used for testing.
func (s *EntitlementService) SetClock(c clockwork.Clock) {
	s.clock = timeutil.FirstClock(c)
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

// IsEntitled returns true if the user currently has an active entitlement
func (s *EntitlementService) IsEntitled(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	return s.repo.IsEntitled(ctx, userID, entitlement, at)
}

func (s *EntitlementService) IsCustomerEntitled(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (bool, error) {
	return s.repo.IsCustomerEntitled(ctx, tenantSubjectID, entitlement, at)
}

func (s *EntitlementService) HasActiveIndefinite(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	return s.repo.HasActiveIndefinite(ctx, userID, entitlement, at)
}

func (s *EntitlementService) HasActiveIndefiniteByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (bool, error) {
	return s.repo.HasActiveIndefiniteByCustomer(ctx, tenantSubjectID, entitlement, at)
}

func (s *EntitlementService) ExistsBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID, entitlement string) (bool, error) {
	return s.repo.ExistsBySource(ctx, sourceType, sourceID, entitlement)
}

func (s *EntitlementService) LatestFiniteWindow(ctx context.Context, userID, entitlement string, at time.Time) (*models.Entitlement, error) {
	return s.repo.GetLatestFiniteActive(ctx, userID, entitlement, at)
}

func (s *EntitlementService) LatestFiniteWindowByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (*models.Entitlement, error) {
	return s.repo.GetLatestFiniteActiveByCustomer(ctx, tenantSubjectID, entitlement, at)
}

func (s *EntitlementService) ListByUser(ctx context.Context, userID string) ([]models.Entitlement, error) {
	return s.repo.ListByUser(ctx, userID)
}

func (s *EntitlementService) ListActiveRecords(ctx context.Context, userID string, at time.Time) ([]models.Entitlement, error) {
	return s.repo.ListActiveRecords(ctx, userID, at)
}

func (s *EntitlementService) ListActiveRecordsByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, at time.Time) ([]models.Entitlement, error) {
	return s.repo.ListActiveRecordsByCustomer(ctx, tenantSubjectID, at)
}

func (s *EntitlementService) ListActiveRecordsByExternalSubjects(ctx context.Context, issuer string, subjects []string, at time.Time) (map[string][]models.Entitlement, error) {
	return s.repo.ListActiveRecordsByExternalSubjects(ctx, issuer, subjects, at)
}

func (s *EntitlementService) ListDistinctEntitlementNamesBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID) ([]string, error) {
	return s.repo.ListDistinctEntitlementNamesBySource(ctx, sourceType, sourceID)
}

// ListActiveEntitlements returns a de-duplicated list of active entitlement names for a user at a point in time.
func (s *EntitlementService) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	return s.repo.ListActiveEntitlements(ctx, userID, at)
}

func (s *EntitlementService) ListActiveEntitlementsByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, at time.Time) ([]string, error) {
	return s.repo.ListActiveEntitlementsByCustomer(ctx, tenantSubjectID, at)
}

// GetByID retrieves an entitlement by its ID
func (s *EntitlementService) GetByID(ctx context.Context, id uuid.UUID) (*models.Entitlement, error) {
	return s.repo.GetByID(ctx, id)
}

type PushNewEntitlementParams struct {
	UserID      string
	CustomerID  uuid.UUID
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

	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := repo.LockEntitlementTimeline(ctx, tx, p.UserID, p.Entitlement); err != nil {
			return err
		}

		// Resolve the payable tenant subject for this entitlement when the caller
		// did not supply one (#317), so every window carries customer_id
		// alongside the legacy user_id. Self-service user UUIDs resolve to a
		// customers row whose id IS that UUID (see repo.EnsureCustomerID),
		// converging with the credits/commerce payable identity.
		if p.CustomerID == uuid.Nil {
			tsid, terr := repo.EnsureCustomerID(ctx, tx, uuid.Nil, p.UserID)
			if terr != nil {
				return terr
			}
			p.CustomerID = tsid
		}

		// If an indefinite entitlement exists, the timeline is terminal.
		hasIndefinite, err := repo.TimelineHasIndefinite(ctx, tx, p.CustomerID, p.Entitlement)
		if err != nil {
			return err
		}
		if hasIndefinite {
			created, err = repo.GetTimelineIndefinite(ctx, tx, p.CustomerID, p.Entitlement)
			if err != nil {
				return err
			}
			return repo.AttachCustomerIfMissing(ctx, tx, created, p.CustomerID, now)
		}

		tailEnd, err := repo.GetTimelineTailEnd(ctx, tx, p.CustomerID, p.Entitlement)
		if err != nil {
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
				covered, cerr := repo.GetTimelineCoveringWindow(ctx, tx, p.CustomerID, p.Entitlement, e)
				if cerr != nil {
					if errors.Is(cerr, pgx.ErrNoRows) {
						return fmt.Errorf("requested entitlement window is already covered by timeline tail but no covering row was found")
					}
					return cerr
				}
				created = covered
				return repo.AttachCustomerIfMissing(ctx, tx, created, p.CustomerID, now)
			}
			endAt = &e
		}

		created = &models.Entitlement{
			ID:          uuidutil.NewV7(),
			CustomerID:  p.CustomerID,
			Entitlement: p.Entitlement,
			StartAt:     start,
			EndAt:       endAt,
			SourceType:  p.SourceType,
			SourceID:    &p.SourceID,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		return repo.InsertTimelineWindow(ctx, tx, created)
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
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		userID := p.UserID
		entitlement := p.Entitlement
		if p.EntitlementID != nil {
			ent, err := repo.GetEntitlementByIDTx(ctx, tx, *p.EntitlementID)
			if err != nil {
				return err
			}
			userID = ent.CustomerID.String()
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
				return repo.SoftDeleteEntitlementByID(ctx, tx, ent.ID, now)
			}
			if ent.EndAt == nil || ent.EndAt.After(now) {
				return repo.RevokeEntitlementByID(ctx, tx, ent.ID, p.Reason, now)
			}
			return nil
		}

		if err := repo.LockEntitlementTimeline(ctx, tx, userID, entitlement); err != nil {
			return err
		}

		// Filter the entitlement timeline by the payable tenant subject (#317);
		// the lock above still serializes on the userID string key.
		tsid, terr := repo.ResolveCustomerID(userID)
		if terr != nil {
			return terr
		}

		if err := repo.RevokeActiveTimelineWindows(ctx, tx, tsid, entitlement, p.Reason, p.SourceType, p.SourceID, now); err != nil {
			return err
		}
		return repo.SoftDeleteFutureTimelineWindows(ctx, tx, tsid, entitlement, p.SourceType, p.SourceID, now)
	})
}
