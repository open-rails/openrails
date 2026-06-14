package budgets

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Budget scopes (#473). A budget POLICY is {scope, owner, windows[]}; at admit
// time a request (subject, actor, roles[]) is gated by EVERY matching scope's
// windows, composed into one verdict over the ONE payer balance.
//
// Each scope yields a distinct, immutable KEY string that is stored verbatim in
// the budget_reservations / budget_window_state `actor` column (an opaque text
// key the engine never interprets — the column name predates the invoker
// rename, #491). Existing (subject, invoker) windows keep the bare invoker
// string as their key, so this is backward compatible:
//
//	ScopeInvoker -> the invoker string (the per-end-user attribution key)
//	ScopeSubject -> "@subject"                  (payer total, one per subject)
//	ScopeRole    -> "@role:<role-uuid>"         (shared pool for a role)
//
// The subject id is already carried by customer_id, so the subject scope
// needs no per-key suffix; the role scope keys on the immutable role uuid
// (never a slug/name — #475/#476 convention).
const (
	ScopeSubject = "subject"
	// ScopeInvoker is the canonical per-invoker (per-end-user) abuse-attribution
	// scope (#491; renamed from "actor"). Scoped (merchant, customer, invoker).
	ScopeInvoker = "invoker"
	ScopeRole    = "role"

	// ScopeActor is the pre-#491 stored/wire value, still accepted on input and
	// normalized to ScopeInvoker. Deprecated: use ScopeInvoker.
	ScopeActor = "actor"
)

// NormalizeScope maps the deprecated "actor" scope value to the canonical
// "invoker" (#491), passing every other value through unchanged. Applied on
// writes/reads so old wire/stored values keep working transiently.
func NormalizeScope(scope string) string {
	if strings.TrimSpace(scope) == ScopeActor {
		return ScopeInvoker
	}
	return scope
}

// scopeKey derives the immutable reservation key for a scope. For the invoker
// scope the key is the caller-supplied invoker string (preserving pre-#473
// rows); for subject/role it is a reserved, collision-resistant "@"-prefixed
// form so a real invoker string can never alias a subject/role bucket.
func scopeKey(scope, invoker string, roleID uuid.UUID) (string, error) {
	switch NormalizeScope(scope) {
	case ScopeInvoker:
		invoker = strings.TrimSpace(invoker)
		if invoker == "" {
			return "", fmt.Errorf("invoker scope requires an invoker")
		}
		return invoker, nil
	case ScopeSubject:
		return "@subject", nil
	case ScopeRole:
		if roleID == uuid.Nil {
			return "", fmt.Errorf("role scope requires a role uuid")
		}
		return "@role:" + roleID.String(), nil
	default:
		return "", fmt.Errorf("unknown budget scope %q (want subject|invoker|role)", scope)
	}
}

// ScopeReservation is one scope's windows to reserve against in a composed
// admit. Key is the pre-derived reservation key (via scopeKey); Windows are the
// fixed-interval windows for that scope.
type ScopeReservation struct {
	// Scope is "subject" | "invoker" | "role" (informational; carried back on the
	// blocking ScopeStatus so the caller can report which authority blocked).
	Scope string
	// Owner is "platform" | "subject" (informational; lets the caller surface
	// whether a platform cap or a subject cap blocked).
	Owner string
	// Key is the immutable reservation key (scopeKey output).
	Key string
	// Windows are the fixed-interval windows for this scope.
	Windows []BudgetWindow
}

// ScopeStatus carries one scope's per-window state plus its scope/owner labels.
type ScopeStatus struct {
	Scope    string
	Owner    string
	Key      string
	Windows  []WindowStatus
	Reserved uuid.UUID // the reservation id placed for this scope (when allowed)
}

// MakeScopeReservation builds a ScopeReservation, deriving the immutable key
// from (scope, invoker, roleID). Empty windows yield a no-op scope (skipped).
// The stored Scope is normalized to the canonical value (#491).
func MakeScopeReservation(scope, owner, invoker string, roleID uuid.UUID, windows []BudgetWindow) (ScopeReservation, error) {
	key, err := scopeKey(scope, invoker, roleID)
	if err != nil {
		return ScopeReservation{}, err
	}
	return ScopeReservation{Scope: NormalizeScope(scope), Owner: owner, Key: key, Windows: windows}, nil
}

// ReserveScopes composes the multi-scope admit verdict (#473): in ONE
// transaction it reserves amountMicros against EVERY scope's windows, and
// allows only if EVERY window of EVERY scope allows. If any window breaches it
// reserves NOTHING (all-or-nothing) and reports the blocking scope+windows.
//
// Each scope is reserved as a separate budget_reservations row keyed by its
// scope key in the `actor` column, idempotent on (tenant, subject, key, source,
// source_id) — a replay returns the existing rows. Budgets are GATES over the
// single payer balance: these rows do NOT debit; the credit ledger places the
// one hold separately. Sub-budgets MAY over-subscribe; the balance is the floor.
//
// Returns the per-scope statuses (with each scope's reservation id when
// allowed), the overall allow decision, and the blocking scope index (-1 when
// allowed or when nothing was reserved on a deny).
func (s *Service) ReserveScopes(ctx context.Context, payer identity.CustomerID, scopes []ScopeReservation, amountMicros int64, source, sourceID string, ttl time.Duration) ([]ScopeStatus, bool, int, error) {
	if s == nil || s.db == nil {
		return nil, false, -1, fmt.Errorf("budgets service not initialized")
	}
	if payer.IsZero() {
		return nil, false, -1, ErrCustomerRequired
	}
	source = strings.TrimSpace(source)
	sourceID = strings.TrimSpace(sourceID)
	if source == "" || sourceID == "" {
		return nil, false, -1, fmt.Errorf("source and source_id required")
	}
	if amountMicros < 0 {
		return nil, false, -1, fmt.Errorf("amount must be non-negative")
	}

	// Drop scopes with no windows (nothing to gate).
	active := make([]ScopeReservation, 0, len(scopes))
	for _, sc := range scopes {
		if len(sc.Windows) == 0 || strings.TrimSpace(sc.Key) == "" {
			continue
		}
		active = append(active, sc)
	}
	if len(active) == 0 {
		return nil, true, -1, nil
	}

	tid, terr := merchant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return nil, false, -1, terr
	}
	tenantID := tid.UUID()
	payerID := payer.UUID()

	out := make([]ScopeStatus, len(active))
	allowed := true
	blockIdx := -1

	err := s.db.TenantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if _, err := repo.EnsureCustomerID(ctx, tx, tenantID, payerID.String()); err != nil {
			return err
		}

		// PASS 1 — evaluate every scope under lock; if any breaches, abort with
		// nothing written (all-or-nothing). Locking all scopes' window-state rows
		// before any insert keeps the composed verdict atomic + serialized.
		type pending struct {
			sts   []WindowStatus
			ok    bool
			opens []windowOpen
		}
		pends := make([]pending, len(active))
		// Lock window-state rows in deterministic key order across all scopes so
		// concurrent composed reserves can't deadlock.
		for _, i := range sortScopesByKey(active) {
			sc := active[i]
			// Idempotency: a replay returns the existing reservation for this scope.
			existing, gerr := q.GetBudgetReservationByCoords(ctx, gen.GetBudgetReservationByCoordsParams{
				MerchantID: tenantID, CustomerID: payerID,
				Actor: sc.Key, Source: source, SourceID: sourceID,
			})
			if gerr == nil {
				sts, _, _, cerr := s.computeWindows(ctx, tx, payer, sc.Key, sc.Windows, amountMicros, false)
				if cerr != nil {
					return cerr
				}
				out[i] = ScopeStatus{Scope: sc.Scope, Owner: sc.Owner, Key: sc.Key, Windows: sts, Reserved: existing.ID}
				pends[i] = pending{sts: sts, ok: true}
				continue
			}
			if !errors.Is(gerr, pgx.ErrNoRows) {
				return gerr
			}
			sts, ok, opens, cerr := s.computeWindows(ctx, tx, payer, sc.Key, sc.Windows, amountMicros, true)
			if cerr != nil {
				return cerr
			}
			out[i] = ScopeStatus{Scope: sc.Scope, Owner: sc.Owner, Key: sc.Key, Windows: sts}
			pends[i] = pending{sts: sts, ok: ok, opens: opens}
			if !ok && allowed {
				allowed = false
				blockIdx = i
			}
		}

		if !allowed {
			// Deny: write nothing. The blocking scope is the first that breached.
			return nil
		}

		// PASS 2 — every scope allowed: open windows + insert one reservation row
		// per scope.
		now := s.now()
		for i, sc := range active {
			if out[i].Reserved != uuid.Nil {
				continue // idempotent replay; already reserved.
			}
			for _, op := range pends[i].opens {
				if op.insert != nil {
					if err := q.InsertBudgetWindowStateIfAbsent(ctx, gen.InsertBudgetWindowStateIfAbsentParams{
						ID:            op.insert.ID,
						MerchantID:    tenantID,
						CustomerID:    payerID,
						Actor:         op.insert.Actor,
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
				Actor:        sc.Key,
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
				Actor:        res.Actor,
				AmountMicros: res.AmountMicros,
				Status:       res.Status,
				Source:       res.Source,
				SourceID:     res.SourceID,
				ExpiresAt:    res.ExpiresAt,
				CreatedAt:    res.CreatedAt,
			}); err != nil {
				return err
			}
			sts, _, _, cerr := s.computeWindows(ctx, tx, payer, sc.Key, sc.Windows, amountMicros, false)
			if cerr != nil {
				return cerr
			}
			out[i] = ScopeStatus{Scope: sc.Scope, Owner: sc.Owner, Key: sc.Key, Windows: sts, Reserved: res.ID}
		}
		return nil
	})
	if err != nil {
		return nil, false, -1, err
	}
	return out, allowed, blockIdx, nil
}

// CaptureByCoords settles ALL budget reservations for a request (every scope
// reserved under one (source, source_id)) to "captured" with actualMicros.
// Idempotent: a replay (already captured/released rows) is a no-op. Used at
// settlement so the composed multi-scope hold settles in one call (#473).
func (s *Service) CaptureByCoords(ctx context.Context, payer identity.CustomerID, source, sourceID string, actualMicros int64) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("budgets service not initialized")
	}
	if payer.IsZero() {
		return ErrCustomerRequired
	}
	if actualMicros < 0 {
		return fmt.Errorf("captured amount must be non-negative")
	}
	tid, terr := merchant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		_, err := s.db.Gen(ctx).CaptureBudgetReservationsByCoords(ctx, gen.CaptureBudgetReservationsByCoordsParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
			Source: strings.TrimSpace(source), SourceID: strings.TrimSpace(sourceID),
			CapturedMicros: actualMicros,
		})
		return err
	})
}

// ReleaseByCoords frees ALL active budget reservations for a request across
// scopes (status -> "released"). Idempotent. Used at settlement when a request
// fails/aborts so every reserved scope is freed in one call (#473).
func (s *Service) ReleaseByCoords(ctx context.Context, payer identity.CustomerID, source, sourceID string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("budgets service not initialized")
	}
	if payer.IsZero() {
		return ErrCustomerRequired
	}
	tid, terr := merchant.Require(ctx) // #336/#474: no default tenant
	if terr != nil {
		return terr
	}
	tenantID := tid.UUID()
	return s.db.RunInTenantConn(ctx, func(ctx context.Context) error {
		_, err := s.db.Gen(ctx).ReleaseBudgetReservationsByCoords(ctx, gen.ReleaseBudgetReservationsByCoordsParams{
			MerchantID: tenantID, CustomerID: payer.UUID(),
			Source: strings.TrimSpace(source), SourceID: strings.TrimSpace(sourceID),
		})
		return err
	})
}

// sortScopesByKey returns scope indices in deterministic key order so composed
// reserves across requests lock window-state rows in a stable order (deadlock
// avoidance), matching computeWindows' per-scope ordering.
func sortScopesByKey(scopes []ScopeReservation) []int {
	order := make([]int, len(scopes))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool { return scopes[order[a]].Key < scopes[order[b]].Key })
	return order
}
