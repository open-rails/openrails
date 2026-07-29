package intents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

// #732 anti-credential-compromise rate ceiling — the ULTIMATE hardcoded
// safeguard against a stolen provider credential or token-signing key. If an
// attacker can mint auth tokens (or reaches a destructive path some other way),
// they must NOT be able to cancel/refund/delete-payment-method for thousands of
// users in seconds. Cap destructive billing ops to a HANDFUL per rolling hour —
// globally and per-actor — so operators have time to NOTICE and ROTATE the bad
// credential before most users are harmed.
//
// This is a TIGHTER sibling of the #679 per-merchant volume breaker at the SAME
// chokepoint (the intents producer): #679 catches slow mass-drift over a day,
// #732 catches the fast credential-abuse burst over minutes. #679 stays; these
// two gates compose with it. It consumes the SAME destructive type set
// (DestructiveIntentTypes) — never a private copy.
//
// Constants, NEVER config: a knob an attacker can raise is not a safeguard. The
// counter is the durable rail_intents ledger itself (#674) — every destructive
// op posts a row BEFORE it executes, so the rolling-hour count already exists as
// queryable durable state; there is no parallel Redis counter to flush, evict,
// or lose on restart, and the gate + the op SHARE FATE (Postgres-down ⇒ the op
// can't post an intent anyway, so there is no fail-open gap).
const (
	// RateCeilingWindow is the rolling window both ceilings count over.
	RateCeilingWindow = time.Hour
	// PerActorHourlyCeiling caps ONE authenticated principal (admin user id or
	// self-service customer id). Root/owner INCLUDED — no bypass, deliberately.
	PerActorHourlyCeiling = 5
	// GlobalHourlyCeiling caps the WHOLE deployment (all actors, all merchants) —
	// the absolute frying-protection ceiling even if many actor identities are
	// forged.
	GlobalHourlyCeiling = 15
	// perActorWarnThreshold / globalWarnThreshold are the running op counts
	// (prior ops + this one) at which an op first crosses 50% of a ceiling. An
	// op in the warning band raises an EARLY-WARNING finding — operators see the
	// burst building BEFORE it hits the wall, which is what buys the
	// notice-and-rotate time.
	perActorWarnThreshold = 3 // ceil(PerActorHourlyCeiling * 0.5)
	globalWarnThreshold   = 8 // ceil(GlobalHourlyCeiling * 0.5)
)

// Finding types the ceiling raises on the operator dashboard (the same
// reconciliation_findings surface #679's held_bulk breaker uses). Must satisfy
// reconciliation_findings' type regex: (pull|derive|life|consistency).seg[.seg].
const (
	RateCeilingTrippedFindingType = "life.destructive_rate.tripped"
	RateCeilingWarningFindingType = "life.destructive_rate.warning"
)

// ceilingKind names which ceiling an event concerns.
type ceilingKind string

const (
	ceilingPerActor ceilingKind = "per_actor"
	ceilingGlobal   ceilingKind = "global"
)

// ErrRateCeilingTripped is the errors.Is sentinel for a ceiling refusal, so the
// HTTP boundary can map it to 429 without depending on the concrete type.
var ErrRateCeilingTripped = errors.New("destructive operation rate limit reached")

// RateCeilingError is the typed hard refusal returned when a destructive op
// would breach a ceiling. Message is client-safe; the fields are operator
// forensics. Fail-closed by construction: the caller (Store.Enqueue) never
// creates the write-ahead intent when this is returned, so the destructive op
// simply does not happen.
type RateCeilingError struct {
	Ceiling     ceilingKind
	Actor       string
	IntentType  string
	ActorCount  int64
	GlobalCount int64
}

func (e *RateCeilingError) Error() string {
	return "destructive operation rate limit reached — try later / contact support"
}

// Is lets errors.Is(err, ErrRateCeilingTripped) recognize any RateCeilingError.
func (e *RateCeilingError) Is(target error) bool { return target == ErrRateCeilingTripped }

// RateCeiling is the gate. It holds the ROOT pool-backed DB (independent of the
// producer's own connection/tx) because its counts must span ALL merchants and
// its findings must persist on an INDEPENDENT transaction — the operator alert
// has to survive the refused op rolling back the caller's transaction.
type RateCeiling struct {
	db *db.DB
}

// NewRateCeiling builds the gate from the root pool-backed DB. nil-safe: a nil
// gate (or nil DB) fails CLOSED at Check time.
func NewRateCeiling(d *db.DB) *RateCeiling { return &RateCeiling{db: d} }

// CheckParams is one destructive op awaiting admission.
type CheckParams struct {
	// Actor is the authenticated principal id (admin user id or self-service
	// customer id). Empty when unresolved — the per-actor ceiling is then
	// skipped but the GLOBAL ceiling still applies.
	Actor      string
	MerchantID uuid.UUID
	IntentType string
	Origin     Origin
}

// Check admits or refuses one destructive op BEFORE its write-ahead intent is
// created. Returns:
//   - nil: allowed (may have raised an early-warning finding as a side effect).
//   - *RateCeilingError: a ceiling tripped — hard refuse (op must not happen).
//   - other error: the gate itself could not evaluate — FAIL CLOSED (the caller
//     must refuse; a compromised path must never sail through a broken gate).
//
// Only destructive (DestructiveIntentTypes) user/admin-origin ops are gated;
// everything else returns nil immediately. origin='system' (automated dunning /
// decline cleanup) is #679's job and must NOT burn the anti-theft budget.
func (c *RateCeiling) Check(ctx context.Context, p CheckParams, now time.Time) error {
	// Non-gated ops always pass — never fail them closed on a gate misconfig.
	if !IsDestructiveIntentType(p.IntentType) {
		return nil
	}
	if p.Origin != OriginUser && p.Origin != OriginAdmin {
		return nil
	}
	// A gated op with a broken/absent gate FAILS CLOSED: a compromised path must
	// never sail through a gate that cannot evaluate.
	if c == nil || c.db == nil {
		return fmt.Errorf("rate ceiling: db not configured") // fail closed
	}

	since := now.Add(-RateCeilingWindow).UTC()
	types := DestructiveIntentTypes()
	// Counts MUST span all merchants — and the base pool CANNOT do that. It is
	// the same openrails_app role; dropping the GUC does not bypass rail_intents'
	// policy, it fails it, so the count came back 0 and this ceiling never
	// tripped (or#860). Both queries now call migration 0021's SECURITY DEFINER
	// readers, which RAISE if their owner cannot bypass RLS — so a mis-owned
	// schema fails loudly here instead of silently permitting the burst.
	// GenDirectory is still the right accessor: the definer needs no merchant
	// GUC, and a merchant-pinned connection would scope nothing.
	q := c.db.GenDirectory()

	globalCount, err := q.CountDestructiveIntentsGlobalSince(ctx, gen.CountDestructiveIntentsGlobalSinceParams{
		IntentTypes: types,
		Since:       since,
	})
	if err != nil {
		return fmt.Errorf("rate ceiling: global count: %w", err) // fail closed
	}
	var actorCount int64
	if p.Actor != "" {
		actorCount, err = q.CountDestructiveIntentsByActorSince(ctx, gen.CountDestructiveIntentsByActorSinceParams{
			Actor:       p.Actor,
			IntentTypes: types,
			Since:       since,
		})
		if err != nil {
			return fmt.Errorf("rate ceiling: actor count: %w", err) // fail closed
		}
	}

	// Trip: per-actor first (more specific/actionable), then the global wall.
	if p.Actor != "" && actorCount >= PerActorHourlyCeiling {
		return c.trip(ctx, p, ceilingPerActor, actorCount, globalCount, now)
	}
	if globalCount >= GlobalHourlyCeiling {
		return c.trip(ctx, p, ceilingGlobal, actorCount, globalCount, now)
	}

	// Early warning: this op crosses 50% of a ceiling (still below the wall).
	if p.Actor != "" && actorCount+1 >= perActorWarnThreshold {
		c.warn(ctx, p, ceilingPerActor, actorCount, globalCount, now)
	}
	if globalCount+1 >= globalWarnThreshold {
		c.warn(ctx, p, ceilingGlobal, actorCount, globalCount, now)
	}
	return nil
}

// trip raises the high-severity operator finding + a log.Error, then returns the
// typed refusal. The refusal is returned REGARDLESS of whether the finding
// write succeeded — the wall is the safety; the alert is best-effort.
func (c *RateCeiling) trip(ctx context.Context, p CheckParams, which ceilingKind, actorCount, globalCount int64, now time.Time) error {
	fields := log.Fields{
		"gate":          "destructive_rate_ceiling",
		"ceiling":       string(which),
		"actor":         p.Actor,
		"merchant_id":   p.MerchantID,
		"intent_type":   p.IntentType,
		"actor_count":   actorCount,
		"global_count":  globalCount,
		"per_actor_max": PerActorHourlyCeiling,
		"global_max":    GlobalHourlyCeiling,
		"window":        RateCeilingWindow.String(),
	}
	log.WithContext(ctx).WithFields(fields).Error(
		"destructive-operation rate ceiling TRIPPED — refusing op; possible credential compromise, verify and rotate")

	subjectKey, action := c.trippedSubjectAction(which, p, actorCount, globalCount)
	evidence := c.evidence(which, p, actorCount, globalCount, now, true)
	if err := c.emitFinding(ctx, p.MerchantID, RateCeilingTrippedFindingType, subjectKey, "critical", action, evidence); err != nil {
		log.WithContext(ctx).WithFields(fields).WithError(err).Error(
			"destructive-rate ceiling: failed to raise tripped finding (op still refused)")
	}
	return &RateCeilingError{
		Ceiling:     which,
		Actor:       p.Actor,
		IntentType:  p.IntentType,
		ActorCount:  actorCount,
		GlobalCount: globalCount,
	}
}

// warn raises the early-warning finding (best-effort) + a log.Warn. Never blocks.
func (c *RateCeiling) warn(ctx context.Context, p CheckParams, which ceilingKind, actorCount, globalCount int64, now time.Time) {
	subjectKey := c.subjectKey(which, p)
	action := fmt.Sprintf(
		"destructive-op burst crossing 50%% of the %s ceiling (per-actor %d/%d, global %d/%d in %s) — watch for credential compromise; approve (ack) to clear once verified normal",
		which, actorCount, PerActorHourlyCeiling, globalCount, GlobalHourlyCeiling, RateCeilingWindow)
	evidence := c.evidence(which, p, actorCount, globalCount, now, false)
	log.WithContext(ctx).WithFields(log.Fields{
		"gate": "destructive_rate_ceiling", "ceiling": string(which), "actor": p.Actor,
		"merchant_id": p.MerchantID, "actor_count": actorCount, "global_count": globalCount,
	}).Warn("destructive-operation rate ceiling: burst crossing 50% of a ceiling")
	if err := c.emitFinding(ctx, p.MerchantID, RateCeilingWarningFindingType, subjectKey, "high", action, evidence); err != nil {
		log.WithContext(ctx).WithError(err).Warn("destructive-rate ceiling: failed to raise early-warning finding")
	}
}

// subjectKey keeps ONE standing finding per (merchant, type, subject): per-actor
// events key on the actor; global events on a fixed "global" subject.
func (c *RateCeiling) subjectKey(which ceilingKind, p CheckParams) string {
	if which == ceilingPerActor {
		return "actor:" + p.Actor
	}
	return "global"
}

func (c *RateCeiling) trippedSubjectAction(which ceilingKind, p CheckParams, actorCount, globalCount int64) (subjectKey, action string) {
	subjectKey = c.subjectKey(which, p)
	if which == ceilingPerActor {
		action = fmt.Sprintf(
			"actor %s hit the per-actor destructive ceiling (%d in %s ≥ %d) — LIKELY CREDENTIAL COMPROMISE; verify and rotate, then approve (ack) to clear",
			p.Actor, actorCount, RateCeilingWindow, PerActorHourlyCeiling)
		return subjectKey, action
	}
	action = fmt.Sprintf(
		"the GLOBAL destructive ceiling tripped (%d in %s ≥ %d across all actors/merchants) — LIKELY CREDENTIAL COMPROMISE; verify and rotate, then approve (ack) to clear",
		globalCount, RateCeilingWindow, GlobalHourlyCeiling)
	return subjectKey, action
}

func (c *RateCeiling) evidence(which ceilingKind, p CheckParams, actorCount, globalCount int64, now time.Time, tripped bool) map[string]any {
	return map[string]any{
		"ceiling":             string(which),
		"tripped":             tripped,
		"actor":               p.Actor,
		"intent_type":         p.IntentType,
		"actor_count":         actorCount,
		"global_count":        globalCount,
		"per_actor_ceiling":   PerActorHourlyCeiling,
		"global_ceiling":      GlobalHourlyCeiling,
		"window_hours":        int(RateCeilingWindow / time.Hour),
		"observed_at":         now.UTC().Format(time.RFC3339),
		recommend.EvidenceKey: recommend.Recommendation{Action: recommend.ActionAckResume}.Map(),
	}
}

// emitFinding writes the operator finding on an INDEPENDENT, RLS-correct
// transaction (fresh pool connection with the merchant GUC pinned), so the
// alert PERSISTS even though the refused op rolls back the caller's transaction.
// A fresh background context (bounded) drops the caller's merchant-pinned
// connection so MerchantTx opens its own tx rather than nesting on the caller's.
func (c *RateCeiling) emitFinding(ctx context.Context, merchantID uuid.UUID, findingType, subjectKey, severity, action string, evidence map[string]any) error {
	ev, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("marshal finding evidence: %w", err)
	}
	fctx, cancel := context.WithTimeout(merchant.WithID(context.Background(), merchant.ID(merchantID)), 5*time.Second)
	defer cancel()
	return c.db.MerchantTx(fctx, func(txctx context.Context, tx pgx.Tx) error {
		_, err := gen.New(tx).UpsertReconciliationFinding(txctx, gen.UpsertReconciliationFindingParams{
			MerchantID:        merchantID,
			FindingType:       findingType,
			SubjectKey:        subjectKey,
			Severity:          severity,
			Status:            "requires_review",
			RecommendedAction: &action,
			Evidence:          ev,
			RunID:             nil,
		})
		return err
	})
}

// ResolveActor picks the actor for a producing enqueue: an explicit override
// wins, otherwise the ambient authenticated principal on the context (set by
// the auth middleware). Empty for system/background paths that carry no
// principal — the per-actor ceiling is then inert (global still applies).
func ResolveActor(ctx context.Context, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if uc, ok := billingauth.FromContext(ctx); ok {
		return uc.UserID
	}
	return ""
}
