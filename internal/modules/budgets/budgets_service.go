// Package budgets implements the rolling-window money-budget engine (issue #304).
//
// A delegated user (invoker) under an tenant subject is capped to a money budget over
// one or more ROLLING windows (e.g. "$2 per 4h, $5 per week"). For each window
// the engine computes used / reserved / remaining and an allow/deny decision plus
// a retry hint. The windows are PASSED IN by the caller — this package does NOT
// read them from any tier table — so admission/tier-policy integration is left to
// the caller.
//
// State lives in billing.budget_reservations (migration 068). Each reservation is
// one in-flight or settled charge:
//
//	Reserve  -> status "active"   (counts against `reserved` by amount_millicents)
//	Capture  -> status "captured" (counts against `used` by captured_millicents)
//	Release  -> status "released" (counts against neither)
//
// All windows are ROLLING relative to now: a reservation counts against a window
// iff created_at >= now - window. Reservations age out of a window simply by
// falling outside that lookback, with no reset job.
package budgets

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/tenant"
	"github.com/uptrace/bun"
)

// ErrTenantSubjectRequired is returned when a budget operation is given a zero tenant subject
// id. Like the credits engine, the payer is supplied by the caller and is never
// synthesized.
var ErrTenantSubjectRequired = errors.New("tenant_subject_id required")

// ErrReservationNotFound is returned by Capture/Release when no active
// reservation with the given id exists for the request tenant.
var ErrReservationNotFound = errors.New("budget_reservation_not_found")

// BudgetWindow is one rolling money-budget window, passed in by the caller.
type BudgetWindow struct {
	// Key is a stable identifier for the window (e.g. "4h", "week"). Echoed back
	// in the matching WindowStatus.
	Key string
	// WindowSeconds is the rolling lookback length in seconds.
	WindowSeconds int64
	// LimitMillicents is the spend cap over the window, in millicents.
	LimitMillicents int64
}

// WindowStatus is the computed state of one window for a Check/Reserve.
type WindowStatus struct {
	Key               string    `json:"key"`
	WindowSeconds     int64     `json:"window_seconds"`
	Limit             int64     `json:"limit_millicents"`
	Used              int64     `json:"used_millicents"`
	Reserved          int64     `json:"reserved_millicents"`
	Remaining         int64     `json:"remaining_millicents"`
	ResetAt           time.Time `json:"reset_at"`
	Allowed           bool      `json:"allowed"`
	RetryAfterSeconds int64     `json:"retry_after_seconds,omitempty"`
}

// Service is the rolling-window money-budget engine over a tenant-scoped DB. The
// clock is injectable so tests can advance time past a window.
type Service struct {
	db    *db.DB
	clock clockwork.Clock
}

// NewService builds the budget engine. An optional clock may be supplied
// (clockwork.NewFakeClock() in tests); when omitted a real clock is used.
func NewService(database *db.DB, clocks ...clockwork.Clock) *Service {
	return &Service{db: database, clock: firstClock(clocks...)}
}

// SetClock swaps the engine's clock (used by tests).
func (s *Service) SetClock(c clockwork.Clock) { s.clock = firstClock(c) }

// Clock returns the engine's clock.
func (s *Service) Clock() clockwork.Clock { return s.clock }

func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock.Now().UTC()
	}
	return time.Now().UTC()
}

func firstClock(clocks ...clockwork.Clock) clockwork.Clock {
	for _, c := range clocks {
		if c != nil {
			return c
		}
	}
	return clockwork.NewRealClock()
}

// Check computes per-window used/reserved/remaining for an invoker and returns the
// allow/deny decision for requestedMillicents WITHOUT writing anything. allowed
// is true iff every window can absorb the request.
func (s *Service) Check(ctx context.Context, payer identity.TenantSubjectID, invoker string, windows []BudgetWindow, requestedMillicents int64) ([]WindowStatus, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("budgets service not initialized")
	}
	if payer.IsZero() {
		return nil, false, ErrTenantSubjectRequired
	}
	invoker = strings.TrimSpace(invoker)
	if invoker == "" {
		return nil, false, fmt.Errorf("invoker required")
	}

	var statuses []WindowStatus
	var allowed bool
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		var e error
		statuses, allowed, e = s.computeWindows(ctx, s.db.Q(ctx), payer, invoker, windows, requestedMillicents)
		return e
	})
	if err != nil {
		return nil, false, err
	}
	return statuses, allowed, nil
}

// Reserve idempotently reserves amountMillicents against the invoker's windows.
//
// It is idempotent on (tenant, payer, invoker, source, source_id): if a matching
// reservation row already exists it is returned as-is (allowed=true), regardless
// of the current window state. Otherwise it runs Check within the same
// transaction; if every window allows the request it inserts an "active"
// reservation and returns allowed=true, else it inserts nothing and returns
// allowed=false. The in-window rows are SELECTed and the insert performed in one
// tx so the decision and the write are consistent.
func (s *Service) Reserve(ctx context.Context, payer identity.TenantSubjectID, invoker string, windows []BudgetWindow, amountMillicents int64, source, sourceID string, ttl time.Duration) (uuid.UUID, []WindowStatus, bool, error) {
	if s == nil || s.db == nil {
		return uuid.Nil, nil, false, fmt.Errorf("budgets service not initialized")
	}
	if payer.IsZero() {
		return uuid.Nil, nil, false, ErrTenantSubjectRequired
	}
	invoker = strings.TrimSpace(invoker)
	if invoker == "" {
		return uuid.Nil, nil, false, fmt.Errorf("invoker required")
	}
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if source == "" || sourceID == "" {
		return uuid.Nil, nil, false, fmt.Errorf("source and source_id required")
	}
	if amountMillicents < 0 {
		return uuid.Nil, nil, false, fmt.Errorf("amount must be non-negative")
	}

	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()

	tx, err := s.db.BeginTenantTx(ctx)
	if err != nil {
		return uuid.Nil, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()

	// Idempotency: a replayed Reserve returns the existing row verbatim.
	existing := new(models.BudgetReservation)
	err = tx.NewSelect().
		Model(existing).
		Where("tenant_id = ? AND tenant_subject_id = ?", tenantID, ownerID).
		Where("invoker_id = ?", invoker).
		Where("source = ? AND source_id = ?", source, sourceID).
		Limit(1).
		Scan(ctx)
	if err == nil {
		// Report the current window state alongside the existing reservation; the
		// reservation already exists, so the decision is allowed.
		statuses, _, cerr := s.computeWindows(ctx, tx, payer, invoker, windows, amountMillicents)
		if cerr != nil {
			return uuid.Nil, nil, false, cerr
		}
		if cerr := tx.Commit(); cerr != nil {
			return uuid.Nil, nil, false, cerr
		}
		return existing.ID, statuses, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, nil, false, err
	}

	statuses, allowed, err := s.computeWindows(ctx, tx, payer, invoker, windows, amountMillicents)
	if err != nil {
		return uuid.Nil, nil, false, err
	}
	if !allowed {
		// No insert when denied; commit (nothing written) so the tx releases cleanly.
		if cerr := tx.Commit(); cerr != nil {
			return uuid.Nil, nil, false, cerr
		}
		return uuid.Nil, statuses, false, nil
	}

	now := s.now()
	res := &models.BudgetReservation{
		ID:               uuidutil.NewV7(),
		TenantID:         tenantID,
		TenantSubjectID:  ownerID,
		InvokerID:        invoker,
		AmountMillicents: amountMillicents,
		Status:           "active",
		Source:           source,
		SourceID:         sourceID,
		CreatedAt:        now,
	}
	if ttl > 0 {
		exp := now.Add(ttl)
		res.ExpiresAt = &exp
	}
	if _, err := tx.NewInsert().Model(res).Exec(ctx); err != nil {
		return uuid.Nil, nil, false, err
	}
	// Recompute so the returned statuses reflect the just-inserted reservation
	// (the caller sees this reservation already counted against `reserved`).
	statuses, _, err = s.computeWindows(ctx, tx, payer, invoker, windows, amountMillicents)
	if err != nil {
		return uuid.Nil, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil, nil, false, err
	}
	return res.ID, statuses, true, nil
}

// Capture settles an active reservation: status -> "captured", captured_millicents
// -> actualMillicents. After capture the reservation counts against `used` (by
// actualMillicents) instead of `reserved`.
func (s *Service) Capture(ctx context.Context, reservationID uuid.UUID, actualMillicents int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("budgets service not initialized")
	}
	if actualMillicents < 0 {
		return fmt.Errorf("captured amount must be non-negative")
	}
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		r, err := s.db.Q(ctx).NewUpdate().
			Model((*models.BudgetReservation)(nil)).
			Set("status = ?", "captured").
			Set("captured_millicents = ?", actualMillicents).
			Where("id = ?", reservationID).
			Where("status = ?", "active").
			Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			return ErrReservationNotFound
		}
		return nil
	})
}

// Release frees an active reservation: status -> "released". After release it
// counts against neither `used` nor `reserved`.
func (s *Service) Release(ctx context.Context, reservationID uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("budgets service not initialized")
	}
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		r, err := s.db.Q(ctx).NewUpdate().
			Model((*models.BudgetReservation)(nil)).
			Set("status = ?", "released").
			Where("id = ?", reservationID).
			Where("status = ?", "active").
			Exec(ctx)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		if n == 0 {
			return ErrReservationNotFound
		}
		return nil
	})
}

// computeWindows runs the per-window aggregation on the supplied queryable handle
// (a tx during Reserve, the pinned conn during Check). q must already be
// tenant-scoped (BeginTenantTx / RunInTenantConn) so RLS constrains the rows.
func (s *Service) computeWindows(ctx context.Context, q bun.IDB, payer identity.TenantSubjectID, invoker string, windows []BudgetWindow, requestedMillicents int64) ([]WindowStatus, bool, error) {
	now := s.now()
	tenantID := tenant.FromContextOrDefault(ctx).UUID()
	ownerID := payer.UUID()

	statuses := make([]WindowStatus, 0, len(windows))
	allAllowed := true

	for _, w := range windows {
		windowStart := now.Add(-time.Duration(w.WindowSeconds) * time.Second)

		// used = SUM(captured_millicents) of captured reservations in-window.
		// reserved = SUM(amount_millicents) of active reservations in-window.
		// oldest = MIN(created_at) of any active|captured reservation in-window,
		// for the rolling ResetAt.
		var agg struct {
			Used     int64        `bun:"used"`
			Reserved int64        `bun:"reserved"`
			Oldest   sql.NullTime `bun:"oldest"`
		}
		err := q.NewSelect().
			Model((*models.BudgetReservation)(nil)).
			ColumnExpr("COALESCE(SUM(captured_millicents) FILTER (WHERE status = 'captured'), 0) AS used").
			ColumnExpr("COALESCE(SUM(amount_millicents) FILTER (WHERE status = 'active'), 0) AS reserved").
			ColumnExpr("MIN(created_at) FILTER (WHERE status IN ('active','captured')) AS oldest").
			Where("tenant_id = ?", tenantID).
			Where("tenant_subject_id = ? AND invoker_id = ?", ownerID, invoker).
			Where("created_at >= ?", windowStart).
			Where("status IN ('active','captured')").
			Scan(ctx, &agg)
		if err != nil {
			return nil, false, err
		}

		remaining := w.LimitMillicents - agg.Used - agg.Reserved
		allowed := requestedMillicents <= remaining

		// ResetAt: when in-window reservations exist, the window frees up when the
		// oldest one ages out (its created_at + window). With none, the window is
		// empty now, so the soonest meaningful boundary is now + window.
		var resetAt time.Time
		if agg.Oldest.Valid {
			resetAt = agg.Oldest.Time.UTC().Add(time.Duration(w.WindowSeconds) * time.Second)
		} else {
			resetAt = now.Add(time.Duration(w.WindowSeconds) * time.Second)
		}

		st := WindowStatus{
			Key:           w.Key,
			WindowSeconds: w.WindowSeconds,
			Limit:         w.LimitMillicents,
			Used:          agg.Used,
			Reserved:      agg.Reserved,
			Remaining:     remaining,
			ResetAt:       resetAt,
			Allowed:       allowed,
		}
		if !allowed {
			allAllowed = false
			secs := int64(math.Ceil(resetAt.Sub(now).Seconds()))
			if secs < 0 {
				secs = 0
			}
			st.RetryAfterSeconds = secs
		}
		statuses = append(statuses, st)
	}

	return statuses, allAllowed, nil
}
