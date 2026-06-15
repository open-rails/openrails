// Package budgets implements the fixed-window money-budget engine (#304, #337).
//
// A delegated user (actor) under a tenant subject is capped to a money budget
// over one or more FIXED windows with knowable reset boundaries (e.g. "$2 per
// 5h, $14 per 7d"). Windows are anchored to each user's OWN first charged
// request, so reset boundaries are naturally staggered across users — there is
// no global reset moment and no rolling lookback.
//
// Two cadences (per window, chosen by the caller):
//
//	"session": the window opens at the first charged request when no window is
//	  active and closes exactly WindowSeconds later; the next window opens on
//	  the user's next charged request after that.
//	"fixed":   boundaries tick at anchor + k*WindowSeconds forever (same
//	  wall-clock reset each period), anchored at the first charged request.
//
// For each window the engine computes used / reserved / remaining and an
// allow/deny decision plus an exact ResetAt (window_start + WindowSeconds —
// displayable as "your next reset is 4:30pm"). The windows are PASSED IN by
// the caller — this package does NOT read them from any tier table — so
// admission/tier-policy integration is left to the caller.
//
// State:
//   - openrails.budget_reservations (one row per in-flight or settled charge):
//     Reserve -> "active" (counts against `reserved` by amount_micros),
//     Capture -> "captured" (counts against `used` by captured_micros),
//     Release -> "released" (counts against neither).
//     A reservation counts against a window iff created_at >= that window's
//     current window_start.
//   - openrails.budget_window_state (one row per tenant/subject/actor/window
//     key, migration 005): the window anchor. Reserve locks it FOR UPDATE so
//     concurrent reserves around a boundary serialize; the rolling engine had
//     no such serialization point.
package budgets

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrCustomerRequired is returned when a budget operation is given a zero tenant subject
// id. Like the credits engine, the payer is supplied by the caller and is never
// synthesized.
var ErrCustomerRequired = errors.New("customer_id required")

// ErrReservationNotFound is returned by Capture/Release when no active
// reservation with the given id exists for the request tenant.
var ErrReservationNotFound = errors.New("budget_reservation_not_found")

// Window cadences. Empty defaults to session.
const (
	CadenceSession = "session"
	CadenceFixed   = "fixed"
)

// normalizeCadence maps "" to session and rejects unknown values.
func normalizeCadence(c string) (string, error) {
	switch strings.TrimSpace(strings.ToLower(c)) {
	case "", CadenceSession:
		return CadenceSession, nil
	case CadenceFixed:
		return CadenceFixed, nil
	default:
		return "", fmt.Errorf("unknown budget window cadence %q (want session|fixed)", c)
	}
}

// BudgetWindow is one fixed money-budget window, passed in by the caller.
type BudgetWindow struct {
	// Key is a stable identifier for the window (e.g. "5h", "7d"). Echoed back
	// in the matching WindowStatus and used as the window-state key.
	Key string
	// WindowSeconds is the window length in seconds.
	WindowSeconds int64
	// LimitMicros is the spend cap over the window, in micro-dollars.
	LimitMicros int64
	// Cadence is "session" (default) or "fixed"; see the package comment.
	Cadence string
}

// WindowStatus is the computed state of one window for a Check/Reserve.
//
// WindowStart/ResetAt are exact boundaries. When no window is active for the
// actor (nothing charged yet, or a session window expired), WindowStart is the
// zero time and ResetAt reports now+WindowSeconds — the boundary a window
// opened by a charge right now would have.
type WindowStatus struct {
	Key               string    `json:"key"`
	WindowSeconds     int64     `json:"window_seconds"`
	Cadence           string    `json:"cadence"`
	Limit             int64     `json:"limit_micros"`
	Used              int64     `json:"used_micros"`
	Reserved          int64     `json:"reserved_micros"`
	Remaining         int64     `json:"remaining_micros"`
	WindowStart       time.Time `json:"window_start"`
	ResetAt           time.Time `json:"reset_at"`
	Allowed           bool      `json:"allowed"`
	RetryAfterSeconds int64     `json:"retry_after_seconds,omitempty"`
}

// Service is the fixed-window money-budget engine over a tenant-scoped DB. The
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

// Check computes per-window used/reserved/remaining for an actor and returns the
// allow/deny decision for requestedMicros WITHOUT writing anything (window
// state is derived virtually; expired session windows read as fresh).
func (s *Service) Check(ctx context.Context, payer identity.CustomerID, actor string, windows []BudgetWindow, requestedMicros int64) ([]WindowStatus, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, fmt.Errorf("budgets service not initialized")
	}
	if payer.IsZero() {
		return nil, false, ErrCustomerRequired
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, false, fmt.Errorf("actor required")
	}

	var statuses []WindowStatus
	var allowed bool
	err := s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		var e error
		statuses, allowed, _, e = s.computeWindows(ctx, s.db.Qx(ctx), payer, actor, windows, requestedMicros, false)
		return e
	})
	if err != nil {
		return nil, false, err
	}
	return statuses, allowed, nil
}

// Reserve idempotently reserves amountMicros against the actor's windows.
//
// It is idempotent on (tenant, payer, actor, source, source_id): if a matching
// reservation row already exists it is returned as-is (allowed=true), regardless
// of the current window state. Otherwise it locks the actor's window-state rows
// FOR UPDATE, runs the window computation, and — when every window allows the
// request — opens/reopens windows as needed (session reopen rewrites
// window_start; a first-ever charge inserts the state row with anchor=now) and
// inserts an "active" reservation, all in one transaction. Denied requests
// write nothing: a denied first request does NOT start a user's window.
func (s *Service) Reserve(ctx context.Context, payer identity.CustomerID, actor string, windows []BudgetWindow, amountMicros int64, source, sourceID string, ttl time.Duration) (uuid.UUID, []WindowStatus, bool, error) {
	if s == nil || s.db == nil {
		return uuid.Nil, nil, false, fmt.Errorf("budgets service not initialized")
	}
	if payer.IsZero() {
		return uuid.Nil, nil, false, ErrCustomerRequired
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return uuid.Nil, nil, false, fmt.Errorf("actor required")
	}
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if source == "" || sourceID == "" {
		return uuid.Nil, nil, false, fmt.Errorf("source and source_id required")
	}
	if amountMicros < 0 {
		return uuid.Nil, nil, false, fmt.Errorf("amount must be non-negative")
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		return uuid.Nil, nil, false, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()

	var (
		reservationID uuid.UUID
		statuses      []WindowStatus
		allowed       bool
	)
	err = s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)

		// Materialize the payable customers row so the budget_reservations /
		// budget_window_state FKs are satisfied on a subject's first reservation (#317).
		if _, err := repo.EnsureCustomerID(ctx, tx, tenantID, payerID.String()); err != nil {
			return err
		}

		// Idempotency: a replayed Reserve returns the existing row verbatim.
		existing, gerr := q.GetBudgetReservationByCoords(ctx, gen.GetBudgetReservationByCoordsParams{
			MerchantID: tenantID, CustomerID: payerID,
			InvokerID: actor, Source: source, SourceID: sourceID,
		})
		if gerr == nil {
			// Report the current window state alongside the existing reservation; the
			// reservation already exists, so the decision is allowed.
			sts, _, _, cerr := s.computeWindows(ctx, tx, payer, actor, windows, amountMicros, false)
			if cerr != nil {
				return cerr
			}
			reservationID, statuses, allowed = existing.ID, sts, true
			return nil
		}
		if !errors.Is(gerr, pgx.ErrNoRows) {
			return gerr
		}

		sts, ok, opens, err := s.computeWindows(ctx, tx, payer, actor, windows, amountMicros, true)
		if err != nil {
			return err
		}
		if !ok {
			// No insert when denied; the tx commits cleanly with nothing written.
			statuses, allowed = sts, false
			return nil
		}

		now := s.now()

		// Open / reopen windows for the charge we are about to write.
		for _, op := range opens {
			if op.insert != nil {
				if err := q.InsertBudgetWindowStateIfAbsent(ctx, gen.InsertBudgetWindowStateIfAbsentParams{
					ID:            op.insert.ID,
					MerchantID:    tenantID,
					CustomerID:    payerID,
					InvokerID:     op.insert.Actor,
					WindowKey:     op.insert.WindowKey,
					Cadence:       op.insert.Cadence,
					WindowSeconds: op.insert.WindowSeconds,
					Anchor:        op.insert.Anchor,
					WindowStart:   op.insert.WindowStart,
					CreatedAt:     op.insert.CreatedAt,
					UpdatedAt:     op.insert.UpdatedAt,
				}); err != nil {
					return err
				}
				continue
			}
			if err := q.ReopenBudgetWindowState(ctx, gen.ReopenBudgetWindowStateParams{
				ID:            op.updateID,
				WindowStart:   op.newStart,
				Cadence:       op.cadence,
				WindowSeconds: op.windowSeconds,
				UpdatedAt:     now,
			}); err != nil {
				return err
			}
		}

		res := &models.BudgetReservation{
			ID:           uuidutil.NewV7(),
			MerchantID:   tenantID,
			CustomerID:   payerID,
			Actor:        actor,
			AmountMicros: amountMicros,
			Status:       "active",
			Source:       source,
			SourceID:     sourceID,
			CreatedAt:    now,
		}
		if ttl > 0 {
			exp := now.Add(ttl)
			res.ExpiresAt = &exp
		}
		if err := q.InsertBudgetReservation(ctx, gen.InsertBudgetReservationParams{
			ID:           res.ID,
			MerchantID:   res.MerchantID,
			CustomerID:   res.CustomerID,
			InvokerID:    res.Actor,
			AmountMicros: res.AmountMicros,
			Status:       res.Status,
			Source:       res.Source,
			SourceID:     res.SourceID,
			ExpiresAt:    res.ExpiresAt,
			CreatedAt:    res.CreatedAt,
		}); err != nil {
			return err
		}
		// Recompute so the returned statuses reflect the just-inserted reservation
		// and the just-opened windows (the caller sees this reservation already
		// counted against `reserved` and the exact ResetAt of its window).
		sts, _, _, err = s.computeWindows(ctx, tx, payer, actor, windows, amountMicros, false)
		if err != nil {
			return err
		}
		reservationID, statuses, allowed = res.ID, sts, true
		return nil
	})
	if err != nil {
		return uuid.Nil, nil, false, err
	}
	return reservationID, statuses, allowed, nil
}

// Capture settles an active reservation: status -> "captured", captured_micros
// -> actualMicros. After capture the reservation counts against `used` (by
// actualMicros) instead of `reserved`.
func (s *Service) Capture(ctx context.Context, reservationID uuid.UUID, actualMicros int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("budgets service not initialized")
	}
	if actualMicros < 0 {
		return fmt.Errorf("captured amount must be non-negative")
	}
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		n, err := s.db.Gen(ctx).CaptureBudgetReservation(ctx, gen.CaptureBudgetReservationParams{
			ID: reservationID, CapturedMicros: actualMicros,
		})
		if err != nil {
			return err
		}
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
		n, err := s.db.Gen(ctx).ReleaseBudgetReservation(ctx, reservationID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrReservationNotFound
		}
		return nil
	})
}

// windowOpen is a pending window-state write produced by computeWindows under
// lock=true and applied by Reserve only when the request is allowed (a denied
// request never starts or advances a user's window).
type windowOpen struct {
	// insert is set for a first-ever charge on this window key.
	insert *models.BudgetWindowState
	// updateID/newStart reopen an expired session window (and opportunistically
	// refresh cadence/window_seconds when the caller's policy changed).
	updateID      uuid.UUID
	newStart      time.Time
	cadence       string
	windowSeconds int64
}

// effectiveStart resolves the current window for the PASSED-IN window config
// against a stored state row (nil = never charged). The caller's
// WindowSeconds/Cadence win over the stored row (policy is the source of
// truth); the row contributes only its anchor/window_start times.
func effectiveStart(st *models.BudgetWindowState, cadence string, windowSeconds int64, now time.Time) (time.Time, bool) {
	if st == nil {
		return time.Time{}, false
	}
	dur := time.Duration(windowSeconds) * time.Second
	if cadence == CadenceFixed {
		anchor := st.Anchor.UTC()
		if !now.After(anchor) {
			return anchor, true
		}
		k := now.Sub(anchor) / dur
		return anchor.Add(k * dur), true
	}
	start := st.WindowStart.UTC()
	if now.Before(start.Add(dur)) {
		return start, true
	}
	return time.Time{}, false
}

// windowStateFromGen maps the generated row onto the domain model.
func windowStateFromGen(r gen.OpenrailsBudgetWindowState) *models.BudgetWindowState {
	return &models.BudgetWindowState{
		ID:            r.ID,
		MerchantID:    r.MerchantID,
		CustomerID:    r.CustomerID,
		Actor:         r.InvokerID,
		WindowKey:     r.WindowKey,
		Cadence:       r.Cadence,
		WindowSeconds: r.WindowSeconds,
		Anchor:        r.Anchor,
		WindowStart:   r.WindowStart,
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// computeWindows runs the per-window state resolution + aggregation on the
// supplied queryable handle (a tx during Reserve, the pinned conn during
// Check). qx must already be tenant-scoped (TenantTx / RunInTenantConn) so
// RLS constrains the rows.
//
// With lock=true (Reserve) the window-state rows are read FOR UPDATE in
// deterministic (window key) order — the serialization point for concurrent
// reserves around a boundary — and the returned []windowOpen describes the
// state writes to apply if the request proceeds. With lock=false (Check,
// post-insert echo) nothing is locked and opens is nil.
func (s *Service) computeWindows(ctx context.Context, qx gen.DBTX, payer identity.CustomerID, actor string, windows []BudgetWindow, requestedMicros int64, lock bool) ([]WindowStatus, bool, []windowOpen, error) {
	now := s.now()
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, nil, err
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()
	q := gen.New(qx)

	// Deterministic lock order regardless of caller's window order.
	order := make([]int, len(windows))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return windows[order[a]].Key < windows[order[b]].Key })

	statuses := make([]WindowStatus, len(windows))
	var opens []windowOpen
	allAllowed := true

	for _, idx := range order {
		w := windows[idx]
		cadence, err := normalizeCadence(w.Cadence)
		if err != nil {
			return nil, false, nil, err
		}
		if w.WindowSeconds <= 0 {
			return nil, false, nil, fmt.Errorf("window %q: window_seconds must be positive", w.Key)
		}
		dur := time.Duration(w.WindowSeconds) * time.Second

		var st *models.BudgetWindowState
		stateKey := gen.GetBudgetWindowStateParams{
			MerchantID: tenantID, CustomerID: payerID,
			InvokerID: actor, WindowKey: w.Key,
		}
		var row gen.OpenrailsBudgetWindowState
		var serr error
		if lock {
			row, serr = q.GetBudgetWindowStateForUpdate(ctx, gen.GetBudgetWindowStateForUpdateParams(stateKey))
		} else {
			row, serr = q.GetBudgetWindowState(ctx, stateKey)
		}
		switch {
		case serr == nil:
			st = windowStateFromGen(row)
		case errors.Is(serr, pgx.ErrNoRows):
			st = nil
		default:
			return nil, false, nil, serr
		}

		start, active := effectiveStart(st, cadence, w.WindowSeconds, now)

		var used, reserved int64
		if active {
			agg, err := q.AggregateBudgetWindow(ctx, gen.AggregateBudgetWindowParams{
				MerchantID: tenantID, CustomerID: payerID, InvokerID: actor,
				WindowStart: start,
			})
			if err != nil {
				return nil, false, nil, err
			}
			used, reserved = agg.Used, agg.Reserved
		}

		remaining := w.LimitMicros - used - reserved
		allowed := requestedMicros <= remaining

		// ResetAt is exact: the active window's end, or — with no active
		// window — the end a window opened by a charge right now would have.
		var resetAt time.Time
		if active {
			resetAt = start.Add(dur)
		} else {
			resetAt = now.Add(dur)
		}

		ws := WindowStatus{
			Key:           w.Key,
			WindowSeconds: w.WindowSeconds,
			Cadence:       cadence,
			Limit:         w.LimitMicros,
			Used:          used,
			Reserved:      reserved,
			Remaining:     remaining,
			WindowStart:   start,
			ResetAt:       resetAt,
			Allowed:       allowed,
		}
		if !allowed {
			allAllowed = false
			if requestedMicros > w.LimitMicros {
				// Larger than the whole window: no reset will ever allow it.
				ws.RetryAfterSeconds = 0
			} else {
				secs := int64(math.Ceil(resetAt.Sub(now).Seconds()))
				if secs < 0 {
					secs = 0
				}
				ws.RetryAfterSeconds = secs
			}
		}
		statuses[idx] = ws

		if lock {
			switch {
			case st == nil:
				// First-ever charge on this key: open at now, anchor at now.
				opens = append(opens, windowOpen{insert: &models.BudgetWindowState{
					ID:            uuidutil.NewV7(),
					Actor:         actor,
					WindowKey:     w.Key,
					Cadence:       cadence,
					WindowSeconds: w.WindowSeconds,
					Anchor:        now,
					WindowStart:   now,
					CreatedAt:     now,
					UpdatedAt:     now,
				}})
			case !active:
				// Expired session window: reopen at now (fixed cadence is always
				// active and derives its start, so it never lands here).
				opens = append(opens, windowOpen{
					updateID:      st.ID,
					newStart:      now,
					cadence:       cadence,
					windowSeconds: w.WindowSeconds,
				})
			}
		}
	}

	return statuses, allAllowed, opens, nil
}
