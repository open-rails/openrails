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
	clock clockwork.Clock
}

func NewEntitlementService(db *db.DB, clocks ...clockwork.Clock) *EntitlementService {
	return &EntitlementService{db: db, clock: timeutil.FirstClock(clocks...)}
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
	tsid, err := repo.ResolveCustomerID(userID)
	if err != nil {
		return false, err
	}
	return s.IsCustomerEntitled(ctx, tsid, entitlement, at)
}

func (s *EntitlementService) IsCustomerEntitled(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (bool, error) {
	// Merchant scoping (issue #223): the merchant is resolved from context and is
	// required — an absent merchant is an error.
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	return s.db.Gen(ctx).EntitlementExistsActive(ctx, gen.EntitlementExistsActiveParams{
		MerchantID:  tid.UUID(),
		CustomerID:  tenantSubjectID,
		Entitlement: entitlement,
		At:          at,
	})
}

func (s *EntitlementService) HasActiveIndefinite(ctx context.Context, userID, entitlement string, at time.Time) (bool, error) {
	tsid, err := repo.ResolveCustomerID(userID)
	if err != nil {
		return false, err
	}
	return s.HasActiveIndefiniteByCustomer(ctx, tsid, entitlement, at)
}

func (s *EntitlementService) HasActiveIndefiniteByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (bool, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	return s.db.Gen(ctx).EntitlementHasActiveIndefinite(ctx, gen.EntitlementHasActiveIndefiniteParams{
		MerchantID:  tid.UUID(),
		CustomerID:  tenantSubjectID,
		Entitlement: entitlement,
		At:          at,
	})
}

func (s *EntitlementService) ExistsBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID, entitlement string) (bool, error) {
	return s.db.Gen(ctx).EntitlementExistsBySource(ctx, gen.EntitlementExistsBySourceParams{
		SourceType:  string(sourceType),
		SourceID:    sourceID,
		Entitlement: entitlement,
	})
}

func (s *EntitlementService) LatestFiniteWindow(ctx context.Context, userID, entitlement string, at time.Time) (*models.Entitlement, error) {
	tsid, err := repo.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	return s.LatestFiniteWindowByCustomer(ctx, tsid, entitlement, at)
}

func (s *EntitlementService) LatestFiniteWindowByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, entitlement string, at time.Time) (*models.Entitlement, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.db.Gen(ctx).GetLatestFiniteActiveEntitlement(ctx, gen.GetLatestFiniteActiveEntitlementParams{
		MerchantID:  tid.UUID(),
		CustomerID:  tenantSubjectID,
		Entitlement: entitlement,
		At:          at,
	})
	if err != nil {
		return nil, err
	}
	return models.EntitlementFromGen(row), nil
}

// Insert persists a fully-populated entitlement window directly (test/seed
// surface; the production write path is PushNewEntitlement via the grant ledger).
func (s *EntitlementService) Insert(ctx context.Context, entitlement *models.Entitlement) error {
	// Validate that end_at > start_at if end_at is provided (non-indefinite entitlement)
	if entitlement.EndAt != nil && !entitlement.EndAt.After(entitlement.StartAt) {
		return fmt.Errorf("invalid entitlement: end_at (%v) must be after start_at (%v)", entitlement.EndAt, entitlement.StartAt)
	}

	// Stamp the resolved merchant (issue #223) when the caller did not set one,
	// so new rows are merchant-scoped consistently with reads. The merchant is
	// required from context — an absent merchant is an error.
	if (entitlement.MerchantID == uuid.UUID{}) {
		tid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		entitlement.MerchantID = tid.UUID()
	}

	id, err := s.db.Gen(ctx).CreateEntitlement(ctx, gen.CreateEntitlementParams{
		ID:           entitlement.ID,
		MerchantID:   entitlement.MerchantID,
		CustomerID:   entitlement.CustomerID,
		Entitlement:  entitlement.Entitlement,
		StartAt:      entitlement.StartAt,
		EndAt:        entitlement.EndAt,
		SourceID:     entitlement.SourceID,
		SourceType:   string(entitlement.SourceType),
		RevokedAt:    entitlement.RevokedAt,
		RevokeReason: models.RevokeReasonPtr(entitlement.RevokeReason),
		CreatedAt:    entitlement.CreatedAt,
		UpdatedAt:    entitlement.UpdatedAt,
	})
	if err != nil {
		return err
	}
	entitlement.ID = id
	return nil
}

func (s *EntitlementService) ListByUser(ctx context.Context, userID string) ([]models.Entitlement, error) {
	tsid, err := repo.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListEntitlementsByCustomer(ctx, tsid)
	if err != nil {
		return nil, err
	}
	return models.EntitlementsFromGen(rows), nil
}

func (s *EntitlementService) ListActiveRecords(ctx context.Context, userID string, at time.Time) ([]models.Entitlement, error) {
	tsid, err := repo.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListActiveEntitlementRecords(ctx, gen.ListActiveEntitlementRecordsParams{
		CustomerID: tsid,
		At:         at,
	})
	if err != nil {
		return nil, err
	}
	return models.EntitlementsFromGen(rows), nil
}

func (s *EntitlementService) ListActiveRecordsByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, at time.Time) ([]models.Entitlement, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListActiveEntitlementRecordsMerchant(ctx, gen.ListActiveEntitlementRecordsMerchantParams{
		MerchantID: tid.UUID(),
		CustomerID: tenantSubjectID,
		At:         at,
	})
	if err != nil {
		return nil, err
	}
	return models.EntitlementsFromGen(rows), nil
}

// ListActiveRecordsByExternalSubjects (#354/#539/#555): one query, grouped by the
// caller-supplied subject; subjects with no active rows are absent from the map.
// Customer identity is (merchant, stable host/AuthKit subject) — the merchant is
// pinned from the request credential (RLS), so no issuer is needed.
func (s *EntitlementService) ListActiveRecordsByExternalSubjects(ctx context.Context, subjects []string, at time.Time) (map[string][]models.Entitlement, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	merchantID := tid.UUID()
	resolved, err := s.db.Gen(ctx).LookupCustomerIDsBySubjects(ctx, gen.LookupCustomerIDsBySubjectsParams{
		MerchantID: merchantID,
		Subjects:   subjects,
	})
	if err != nil {
		return nil, err
	}
	subjectByCustomerID := make(map[uuid.UUID]string, len(resolved))
	customerIDs := make([]uuid.UUID, 0, len(resolved))
	for _, row := range resolved {
		subj := ""
		if row.Subject != nil {
			subj = *row.Subject
		}
		subjectByCustomerID[row.ID] = subj
		customerIDs = append(customerIDs, row.ID)
	}
	return s.listActiveRecordsByCustomerIDs(ctx, merchantID, subjectByCustomerID, customerIDs, at)
}

func (s *EntitlementService) listActiveRecordsByCustomerIDs(ctx context.Context, merchantID uuid.UUID, subjectByCustomerID map[uuid.UUID]string, customerIDs []uuid.UUID, at time.Time) (map[string][]models.Entitlement, error) {
	if len(customerIDs) == 0 {
		return map[string][]models.Entitlement{}, nil
	}
	rows, err := s.db.Gen(ctx).ListActiveEntitlementRecordsByCustomerIDs(ctx, gen.ListActiveEntitlementRecordsByCustomerIDsParams{
		MerchantID:  merchantID,
		CustomerIds: customerIDs,
		At:          at,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string][]models.Entitlement, len(rows))
	for _, row := range rows {
		sourceID := row.SourceID
		m := models.Entitlement{
			ID:          row.ID,
			MerchantID:  row.MerchantID,
			CustomerID:  row.CustomerID,
			Entitlement: row.Entitlement,
			StartAt:     row.StartAt,
			EndAt:       row.EndAt,
			SourceID:    &sourceID,
			SourceType:  models.EntitlementSourceType(row.SourceType),
			RevokedAt:   row.RevokedAt,
			CreatedAt:   row.CreatedAt,
			UpdatedAt:   row.UpdatedAt,
			DeletedAt:   row.DeletedAt,
		}
		if row.RevokeReason != nil {
			rr := models.EntitlementRevokeReason(*row.RevokeReason)
			m.RevokeReason = &rr
		}
		subj := subjectByCustomerID[row.CustomerID]
		out[subj] = append(out[subj], m)
	}
	return out, nil
}

func (s *EntitlementService) ListDistinctEntitlementNamesBySource(ctx context.Context, sourceType models.EntitlementSourceType, sourceID uuid.UUID) ([]string, error) {
	return s.db.Gen(ctx).ListDistinctEntitlementNamesBySource(ctx, gen.ListDistinctEntitlementNamesBySourceParams{
		SourceType: string(sourceType),
		SourceID:   sourceID,
	})
}

// ListActiveEntitlements returns a de-duplicated list of active entitlement names for a user at a point in time.
func (s *EntitlementService) ListActiveEntitlements(ctx context.Context, userID string, at time.Time) ([]string, error) {
	tsid, err := repo.ResolveCustomerID(userID)
	if err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ListActiveEntitlementNames(ctx, gen.ListActiveEntitlementNamesParams{
		CustomerID: tsid,
		At:         at,
	})
}

func (s *EntitlementService) ListActiveEntitlementsByCustomer(ctx context.Context, tenantSubjectID uuid.UUID, at time.Time) ([]string, error) {
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	return s.db.Gen(ctx).ListActiveEntitlementNamesMerchant(ctx, gen.ListActiveEntitlementNamesMerchantParams{
		MerchantID: tid.UUID(),
		CustomerID: tenantSubjectID,
		At:         at,
	})
}

// CustomersWithEntitlementMaxPageSize bounds a single reverse-lookup page; the
// caller keyset-paginates (afterID) to walk larger result sets.
const CustomersWithEntitlementMaxPageSize = 10000

// ListCustomersWithEntitlement is the REVERSE entitlement lookup (#535): customer
// ids holding an active window of `entitlement` for the request's merchant,
// keyset-paginated by customer_id (afterID exclusive; uuid.Nil starts). Backs the
// host directory's filter-by-entitlement (AuthKit's EntitlementFilterProvider).
// limit <= 0 defaults to 1000; it is capped at CustomersWithEntitlementMaxPageSize.
// The query is merchant-scoped by RLS (the merchant is pinned from the request).
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
	return s.db.Gen(ctx).ListCustomersWithEntitlement(ctx, gen.ListCustomersWithEntitlementParams{
		Entitlement: strings.TrimSpace(entitlement),
		At:          at,
		AfterID:     afterID,
		Lim:         int32(limit),
	})
}

// GetByID retrieves an entitlement by its ID
func (s *EntitlementService) GetByID(ctx context.Context, id uuid.UUID) (*models.Entitlement, error) {
	row, err := s.db.Gen(ctx).GetEntitlementByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return models.EntitlementFromGen(row), nil
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
		if err := LockEntitlementTimeline(ctx, tx, p.UserID, p.Entitlement); err != nil {
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
		hasIndefinite, err := TimelineHasIndefinite(ctx, tx, p.CustomerID, p.Entitlement)
		if err != nil {
			return err
		}
		if hasIndefinite {
			created, err = GetTimelineIndefinite(ctx, tx, p.CustomerID, p.Entitlement)
			if err != nil {
				return err
			}
			// #691: the covering window is THIS subscription's own STANDING window
			// and the caller pushed a bounded paid period — the projection is
			// already right (standing), but the paid period is still recorded as a
			// bounded per-period grant (the ledger stays per-period; the fold is
			// grants + closure events).
			if p.SourceType == models.EntitlementSourceSubscription && p.EndAt != nil &&
				created.SourceType == p.SourceType && created.SourceID != nil && *created.SourceID == p.SourceID {
				if err := s.appendCoveredPeriodGrant(ctx, tx, merchantID.UUID(), p, now); err != nil {
					return err
				}
			}
			return AttachCustomerIfMissing(ctx, tx, created, p.CustomerID, now)
		}

		tailEnd, err := GetTimelineTailEnd(ctx, tx, p.CustomerID, p.Entitlement)
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
				covered, cerr := GetTimelineCoveringWindow(ctx, tx, p.CustomerID, p.Entitlement, e)
				if cerr != nil {
					if errors.Is(cerr, pgx.ErrNoRows) {
						return fmt.Errorf("requested entitlement window is already covered by timeline tail but no covering row was found")
					}
					return cerr
				}
				created = covered
				return AttachCustomerIfMissing(ctx, tx, created, p.CustomerID, now)
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
		window, fErr := GetEntitlementByGrant(ctx, tx, merchantID.UUID(), g.ID, p.Entitlement)
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

// appendCoveredPeriodGrant records the bounded per-period grant for a renewal
// whose projection is already covered by the subscription's own standing window
// (#691). Idempotent: a replayed period whose end is not past the latest
// recorded bounded end appends nothing. Runs inside the caller's tx with the
// timeline lock held.
func (s *EntitlementService) appendCoveredPeriodGrant(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID, p PushNewEntitlementParams, now time.Time) error {
	q := gen.New(tx)
	end := p.EndAt.UTC()
	start := now
	if p.NotBefore != nil && !p.NotBefore.IsZero() {
		start = p.NotBefore.UTC()
	}
	latest, err := q.LatestEntitlementGrantEndForSource(ctx, gen.LatestEntitlementGrantEndForSourceParams{
		MerchantID: merchantID, CustomerID: p.CustomerID,
		SourceType: string(grantSourceType(p.SourceType)), SourceID: p.SourceID.String(),
		Entitlement: p.Entitlement,
	})
	if err != nil {
		return err
	}
	if !latest.IsZero() {
		if !end.After(latest) {
			return nil // replay: this period is already recorded
		}
		if latest.After(start) {
			start = latest // contiguous per-period ledger
		}
	}
	if !end.After(start) {
		return nil
	}
	gl := grants.New(q, merchantID)
	gl.SetClock(func() time.Time { return s.now().UTC() })
	g, err := gl.Grant(ctx, grants.GrantInput{
		Customer: p.CustomerID, Kind: grants.Entitlement,
		Source: grantSourceType(p.SourceType), SourceID: p.SourceID.String(),
		Spec:     &grants.Spec{Entitlements: []string{p.Entitlement}},
		StartsAt: start, EndsAt: &end,
	})
	if err != nil {
		return err
	}
	// The standing window satisfies the projection; MaterializeGrant is a no-op
	// window-wise but keeps usage-limit bindings and future retractions wired.
	return gl.MaterializeGrant(ctx, g)
}

// BoundSubscriptionAccess writes the PROVEN closure for a subscription's access
// (#691): live subscription-sourced windows get end_at = endAt — advance-written
// on disk, so a dead system cannot extend a cancelled sub — and scheduled
// windows starting at/after the closure are removed. Idempotent.
func (s *EntitlementService) BoundSubscriptionAccess(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	now := s.now().UTC()
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.SoftDeleteFutureEntitlementsBySubscription(ctx, gen.SoftDeleteFutureEntitlementsBySubscriptionParams{
			SourceID: subscriptionID, EndAt: endAt.UTC(), Now: now,
		}); err != nil {
			return err
		}
		return q.EndActiveEntitlementsBySubscription(ctx, gen.EndActiveEntitlementsBySubscriptionParams{
			SourceID: subscriptionID, EndAt: endAt.UTC(), Now: now, SetRevoked: false,
		})
	})
}

// ResumeSubscriptionAccess re-opens a resumed auto-renew subscription's latest
// bounded window (end_at = NULL), undoing an advance-written cancel closure
// (#691). Gated on the sub actually projecting standing access again (auto-renew
// price + non-terminal status), so terminal/bounded subs are a no-op.
func (s *EntitlementService) ResumeSubscriptionAccess(ctx context.Context, subscriptionID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	mID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	return s.withTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		standing, err := q.SubscriptionProjectsStandingAccess(ctx, gen.SubscriptionProjectsStandingAccessParams{
			MerchantID: mID.UUID(), ID: subscriptionID,
		})
		if err != nil {
			return err
		}
		if !standing {
			return nil
		}
		return q.ResumeEntitlementsBySubscription(ctx, gen.ResumeEntitlementsBySubscriptionParams{
			SourceID: subscriptionID, Now: now,
		})
	})
}

func (s *EntitlementService) ExtendActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	return s.extendActiveBySubscription(ctx, subscriptionID, endAt.UTC(), s.now().UTC())
}

// extendActiveBySubscription extends active entitlements for a subscription to endAt.
// It only updates rows whose end_at is NULL or before endAt, and will never shorten a window.
func (s *EntitlementService) extendActiveBySubscription(ctx context.Context, subscriptionID uuid.UUID, endAt time.Time, now time.Time) error {
	return s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Fetch all subscription entitlements that would be extended, then shift any following
		// scheduled windows forward by the same delta (per user+entitlement) to avoid overlaps.
		//
		// This keeps the entitlement timeline gapless for the affected entitlement key and avoids
		// double-access from overlapping scheduled windows.
		q := gen.New(tx)
		rows, err := q.ListExtendableSubscriptionEntitlements(ctx, gen.ListExtendableSubscriptionEntitlementsParams{
			SourceID: subscriptionID,
			EndAt:    endAt,
		})
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}

		for _, row := range rows {
			ent := models.EntitlementFromGen(row)
			if ent.EndAt == nil || ent.EndAt.IsZero() {
				continue
			}
			oldEnd := ent.EndAt.UTC()
			newEnd := endAt.UTC()
			if !newEnd.After(oldEnd) {
				continue
			}

			// Validate: do not produce end_at <= start_at
			if !newEnd.After(ent.StartAt) {
				return fmt.Errorf("cannot extend end_at to %v: entitlement start_at=%v would be >= end_at", newEnd, ent.StartAt)
			}

			if err := LockEntitlementTimeline(ctx, tx, ent.CustomerID.String(), ent.Entitlement); err != nil {
				return err
			}

			// Shift the following windows forward FIRST to open the gap. Extending
			// the subscription row to newEnd before shifting would transiently
			// overlap the next (not-yet-shifted) window, and entitlements_no_overlap
			// is an IMMEDIATE (non-deferrable) exclusion constraint checked per
			// statement — so it must never be violated mid-transaction.
			delta := newEnd.Sub(oldEnd)
			if err := ShiftEntitlementTimeline(ctx, tx, ent.CustomerID.String(), ent.Entitlement, oldEnd, delta, now, []uuid.UUID{ent.ID}); err != nil {
				return err
			}

			// Extend the subscription's entitlement row.
			if err := q.UpdateEntitlementEndAtIfMatch(ctx, gen.UpdateEntitlementEndAtIfMatchParams{
				ID:       ent.ID,
				NewEndAt: newEnd,
				Now:      now,
				OldEndAt: oldEnd,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *EntitlementService) EndActiveByPayment(ctx context.Context, paymentID uuid.UUID, reason models.EntitlementRevokeReason) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("entitlement service not initialized")
	}
	now := s.now().UTC()
	return s.endActiveByPayment(ctx, paymentID, now, now, &reason)
}

// endActiveByPayment revokes active one-off entitlements for a payment and removes future windows.
// The now parameter is used for updated_at and revoked_at timestamps to support mock clocks in tests.
func (s *EntitlementService) endActiveByPayment(ctx context.Context, paymentID uuid.UUID, endAt time.Time, now time.Time, reason *models.EntitlementRevokeReason) error {
	return s.db.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := q.SoftDeleteFutureOneOffEntitlements(ctx, gen.SoftDeleteFutureOneOffEntitlementsParams{
			SourceID: paymentID,
			Now:      now,
			EndAt:    endAt,
		}); err != nil {
			return err
		}
		return q.RevokeActiveOneOffEntitlements(ctx, gen.RevokeActiveOneOffEntitlementsParams{
			SourceID:     paymentID,
			EndAt:        endAt,
			Now:          now,
			RevokeReason: models.RevokeReasonPtr(reason),
		})
	})
}

func (s *EntitlementService) RevokeSourcesForSubscription(ctx context.Context, userID string, subscriptionID uuid.UUID, reason models.EntitlementRevokeReason, sourceTypes ...models.EntitlementSourceType) error {
	return s.RevokeSourcesForSubscriptionAsOf(ctx, userID, subscriptionID, s.now().UTC(), reason, sourceTypes...)
}

// RevokeSourcesForSubscriptionAsOf is RevokeSourcesForSubscription with an
// explicit as-of instant: the LIFE-plane grace_exhausted repair revokes access
// as-of when grace actually lapsed (converge-not-replay), not at convergence
// time. Pass s.now() for the normal "revoke now" semantics.
func (s *EntitlementService) RevokeSourcesForSubscriptionAsOf(ctx context.Context, userID string, subscriptionID uuid.UUID, asOf time.Time, reason models.EntitlementRevokeReason, sourceTypes ...models.EntitlementSourceType) error {
	if s == nil || s.db == nil {
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
	if err := s.revokeGrantsForSubscriptionSources(ctx, userID, subscriptionID, at, sourceTypes); err != nil {
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
func (s *EntitlementService) revokeGrantsForSubscriptionSources(ctx context.Context, userID string, subscriptionID uuid.UUID, asOf time.Time, sourceTypes []models.EntitlementSourceType) error {
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
		return gl.RevokeBySourceAsOf(ctx, customerID, grants.Entitlement, gsources, subscriptionID.String(), "subscription source revoked", asOf)
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
			ent, err := GetEntitlementByIDTx(ctx, tx, *p.EntitlementID)
			if err != nil {
				return err
			}
			userID = ent.CustomerID.String()
			entitlement = ent.Entitlement
			if err := LockEntitlementTimeline(ctx, tx, userID, entitlement); err != nil {
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
				return SoftDeleteEntitlementByID(ctx, tx, ent.ID, now)
			}
			if ent.EndAt == nil || ent.EndAt.After(now) {
				return RevokeEntitlementByID(ctx, tx, ent.ID, p.Reason, now)
			}
			return nil
		}

		if err := LockEntitlementTimeline(ctx, tx, userID, entitlement); err != nil {
			return err
		}

		// Filter the entitlement timeline by the payable merchant subject (#317);
		// the lock above still serializes on the userID string key.
		tsid, terr := repo.ResolveCustomerID(userID)
		if terr != nil {
			return terr
		}

		if err := RevokeActiveTimelineWindows(ctx, tx, tsid, entitlement, p.Reason, p.SourceType, p.SourceID, now); err != nil {
			return err
		}
		return SoftDeleteFutureTimelineWindows(ctx, tx, tsid, entitlement, p.SourceType, p.SourceID, now)
	})
}
