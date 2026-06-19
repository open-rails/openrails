package entitlements

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/shared/timeutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// grantSourceType bridges the entitlement source vocabulary
// (subscription/one_off/admin/grace) to the grant ledger's vocabulary
// (purchase/subscription/admin/grace): an `one_off` entitlement is a `purchase`
// grant; the others pass through. (MaterializeGrant maps it back.)
func grantSourceType(s models.EntitlementSourceType) grants.SourceType {
	if string(s) == "one_off" {
		return grants.Purchase
	}
	return grants.SourceType(string(s))
}

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
	// Run inside a merchant-scoped transaction so the migration-050 RLS policies
	// constrain every entitlement query to the request's merchant (db.MerchantTx
	// sets the app.merchant_id GUC from the context as the first statement). This is
	// the shared chokepoint for merchant-owned entitlement writes/reads.
	return s.db.MerchantTx(ctx, fn)
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

// CustomersWithEntitlementMaxPageSize bounds a single reverse-lookup page; the
// caller keyset-paginates (afterID) to walk larger result sets.
const CustomersWithEntitlementMaxPageSize = 10000

// ListCustomersWithEntitlement is the REVERSE entitlement lookup (#535): customer
// ids holding an active window of `entitlement` for the request's merchant,
// keyset-paginated by customer_id (afterID exclusive; uuid.Nil starts). Backs the
// host directory's filter-by-entitlement (AuthKit's EntitlementFilterProvider).
// limit <= 0 defaults to 1000; it is capped at CustomersWithEntitlementMaxPageSize.
func (s *EntitlementService) ListCustomersWithEntitlement(ctx context.Context, entitlement string, at time.Time, afterID uuid.UUID, limit int) ([]uuid.UUID, error) {
	if strings.TrimSpace(entitlement) == "" {
		return nil, fmt.Errorf("entitlement is required")
	}
	if limit <= 0 {
		limit = 1000
	}
	if limit > CustomersWithEntitlementMaxPageSize {
		limit = CustomersWithEntitlementMaxPageSize
	}
	return s.repo.ListCustomersWithEntitlement(ctx, strings.TrimSpace(entitlement), at, afterID, int32(limit))
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
	merchantID, mErr := merchant.Require(ctx)
	if mErr != nil {
		return nil, mErr
	}
	var created *models.Entitlement

	err := s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := repo.LockEntitlementTimeline(ctx, tx, p.UserID, p.Entitlement); err != nil {
			return err
		}

		// Resolve the payable merchant subject for this entitlement when the caller
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

		// #511: the entitlement is a DERIVED effect of a grant — the grant ledger is
		// the source of truth. Create the entitlement-kind grant for this window,
		// then derive-2 (MaterializeGrant) projects the entitlement row (carrying
		// grant_id + its preserved source_type/source_id so existing readers work).
		// The timeline-window computation above is unchanged; the grant just carries
		// the computed [start, end).
		gl := grants.New(gen.New(tx), merchantID.UUID())
		g, gErr := gl.Grant(ctx, grants.GrantInput{
			Customer: p.CustomerID, Kind: grants.Entitlement,
			Source: grantSourceType(p.SourceType), SourceID: p.SourceID.String(),
			Spec:     &grants.Spec{Entitlements: []string{p.Entitlement}},
			StartsAt: start, EndsAt: endAt,
		})
		if gErr != nil {
			return gErr
		}
		if mErr := gl.MaterializeGrant(ctx, g); mErr != nil {
			return mErr
		}
		// Return the entitlement window MaterializeGrant just projected for this grant.
		window, fErr := repo.GetEntitlementByGrant(ctx, tx, merchantID.UUID(), g.ID, p.Entitlement)
		if fErr != nil {
			return fErr
		}
		created = window
		return nil
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
	return s.RevokeSourcesForSubscriptionAsOf(ctx, userID, subscriptionID, s.now().UTC(), reason, sourceTypes...)
}

// RevokeSourcesForSubscriptionAsOf is RevokeSourcesForSubscription with an
// explicit as-of instant: the LIFE-plane grace_exhausted repair revokes access
// as-of when grace actually lapsed (converge-not-replay), not at convergence
// time. Pass s.now() for the normal "revoke now" semantics.
func (s *EntitlementService) RevokeSourcesForSubscriptionAsOf(ctx context.Context, userID string, subscriptionID uuid.UUID, asOf time.Time, reason models.EntitlementRevokeReason, sourceTypes ...models.EntitlementSourceType) error {
	if s == nil || s.repo == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	at := asOf.UTC()
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
				AsOf:        &at,
			}); err != nil {
				return fmt.Errorf("revoke %s entitlement %s: %w", sourceType, entName, err)
			}
		}
	}
	// #511 write-path unification: the windows above are the EFFECT; terminate the
	// matching live entitlement grants too, so the grant ledger (the source of
	// truth) reflects the retraction rather than drifting (a live grant whose
	// effect is revoked). This keeps the DERIVE grant-tier checks precise — a
	// properly-cancelled subscription leaves no "live grant, dead effect" residue.
	if err := s.revokeGrantsForSubscriptionSources(ctx, userID, subscriptionID, sourceTypes); err != nil {
		return fmt.Errorf("revoke grants for subscription sources: %w", err)
	}
	return nil
}

// revokeGrantsForSubscriptionSources terminates the live entitlement-kind grants
// of one subscription for the given entitlement source types (subscription /
// grace), keeping the grant ledger consistent with a source-keyed effect
// revocation (#511 write-path unification). The grant's free-text source_id is the
// subscription UUID string (set by PushNewEntitlement). Best-effort vocabulary
// bridge via grantSourceType (subscription→subscription, grace→grace).
func (s *EntitlementService) revokeGrantsForSubscriptionSources(ctx context.Context, userID string, subscriptionID uuid.UUID, sourceTypes []models.EntitlementSourceType) error {
	if len(sourceTypes) == 0 {
		return nil
	}
	mID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	customerID, err := repo.ResolveCustomerID(userID)
	if err != nil {
		return err
	}
	gsources := make([]grants.SourceType, 0, len(sourceTypes))
	for _, st := range sourceTypes {
		gsources = append(gsources, grantSourceType(st))
	}
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		gl := grants.New(gen.New(tx), mID.UUID())
		gl.SetClock(func() time.Time { return s.now().UTC() })
		return gl.RevokeBySource(ctx, customerID, grants.Entitlement, gsources, subscriptionID.String(), "subscription source revoked")
	})
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

	// AsOf is the instant the revocation takes effect (revoked_at value + the
	// active/future window boundary). Nil = now. The LIFE-plane grace_exhausted
	// repair sets this to grace-end so access is revoked as-of when it actually
	// lapsed (converge-not-replay), never re-running the missed dunning charges.
	AsOf *time.Time
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
	if p.AsOf != nil {
		now = p.AsOf.UTC()
	}
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

		// Filter the entitlement timeline by the payable merchant subject (#317);
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
