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

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile/recommend"
	"github.com/open-rails/openrails/internal/shared/cadence"
)

// Rate ceiling against a stolen provider credential or token-signing key: an
// attacker who reaches a destructive path must not cancel, refund or delete
// payment methods for thousands of users in seconds. Destructive ops are
// capped to a handful per rolling hour, per merchant and per actor, so
// operators can notice and rotate the credential.
//
// It gates the producer against fast bursts; the volume breaker gates
// execution against slow drift over a day. Both use DestructiveIntentTypes.
// Constants, never config: a knob an attacker can raise is not a safeguard.
// The counter is provider_intents itself: every destructive op posts its row
// before executing, so the gate and the op share fate (no fail-open gap).
const (
	// RateCeilingWindow is the rolling window both ceilings count over.
	RateCeilingWindow = time.Hour
	// PerActorHourlyCeiling caps ONE authenticated principal (admin user id or
	// self-service customer id). Root/owner INCLUDED — no bypass, deliberately.
	PerActorHourlyCeiling = 5
	// PerMerchantHourlyCeiling caps one merchant's human-originated
	// (user/admin) destructive ops per rolling hour; it holds even when many
	// actor identities are forged. Per merchant, not deployment-wide, so one
	// busy merchant cannot deny every other merchant's cancellations.
	PerMerchantHourlyCeiling = 15
	// PerMerchantSystemHourlyCeiling caps one merchant's automated
	// (origin='system') destructive queueing per rolling hour: above any
	// legitimate burst (a convergence pass cancels at most
	// reconcile.DefaultMaxCancelsPerPass), far below thousands in seconds.
	PerMerchantSystemHourlyCeiling = 50
	// The warn thresholds are the running op counts (prior ops + this one) at
	// which an op crosses 50% of a ceiling and raises an early-warning finding,
	// so operators see a burst before it hits the wall.
	perActorWarnThreshold       = 3  // ceil(PerActorHourlyCeiling * 0.5)
	perMerchantWarnThreshold    = 8  // ceil(PerMerchantHourlyCeiling * 0.5)
	systemMerchantWarnThreshold = 25 // ceil(PerMerchantSystemHourlyCeiling * 0.5)
)

// Finding types the ceiling raises on the operator dashboard (the
// reconciliation_findings surface the volume breaker also uses). Must satisfy
// reconciliation_findings' type regex: (pull|derive|life|consistency).seg[.seg].
const (
	RateCeilingTrippedFindingType = "life.destructive_rate.tripped"
	RateCeilingWarningFindingType = "life.destructive_rate.warning"
)

// ceilingKind names which ceiling an event concerns.
type ceilingKind string

const (
	ceilingPerActor       ceilingKind = "per_actor"
	ceilingPerMerchant    ceilingKind = "per_merchant"
	ceilingSystemMerchant ceilingKind = "system_merchant"
)

// ceilingCounts are the rolling-window counts an admission decision saw. Which
// legs are populated depends on the origin: user/admin ops count per-actor
// (across merchants) and per-merchant, system ops count per-merchant.
type ceilingCounts struct {
	actor          int64
	merchant       int64
	systemMerchant int64
}

// antiTheftOrigins is the human-originated origin set the anti-theft walls
// count. origin='system' is deliberately excluded — no principal produced those,
// so they must not burn a budget that exists to bound a stolen credential; they
// have their own per-merchant window (checkSystem).
var antiTheftOrigins = []string{string(OriginUser), string(OriginAdmin)}

// ErrRateCeilingTripped is the errors.Is sentinel for a ceiling refusal, so the
// HTTP boundary can map it to 429 without depending on the concrete type.
var ErrRateCeilingTripped = errors.New("destructive operation rate limit reached")

// RateCeilingError is the typed refusal when a destructive op would breach a
// ceiling. Message is client-safe; the fields are operator forensics.
// Store.Enqueue never creates the intent when this is returned.
type RateCeilingError struct {
	Ceiling             ceilingKind
	Actor               string
	IntentType          string
	ActorCount          int64
	MerchantCount       int64
	SystemMerchantCount int64
}

func (e *RateCeilingError) Error() string {
	return "destructive operation rate limit reached — try later / contact support"
}

// Is lets errors.Is(err, ErrRateCeilingTripped) recognize any RateCeilingError.
func (e *RateCeilingError) Is(target error) bool { return target == ErrRateCeilingTripped }

// RateCeiling is the gate. It holds the ROOT pool-backed DB (independent of the
// producer's own connection/tx) because the per-actor count must span merchants
// and its findings must persist on an INDEPENDENT transaction — the operator
// alert has to survive the refused op rolling back the caller's transaction.
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
	// skipped but the per-merchant ceiling still applies.
	Actor      string
	MerchantID uuid.UUID
	IntentType string
	Origin     Origin
}

// Check admits or refuses one destructive op before its write-ahead intent is
// created: nil allows it (possibly raising an early-warning finding);
// *RateCeilingError refuses it; any other error means the gate could not
// evaluate, and the caller must refuse (fail closed).
//
// User/admin ops count against the anti-theft ceilings (per actor across
// merchants, and per merchant); system ops against the per-merchant automation
// ceiling. Non-destructive types pass; an unknown origin fails closed.
func (c *RateCeiling) Check(ctx context.Context, p CheckParams, now time.Time) error {
	// Non-gated ops always pass — never fail them closed on a gate misconfig.
	if !IsDestructiveIntentType(p.IntentType) {
		return nil
	}
	switch p.Origin {
	case OriginUser, OriginAdmin:
	case OriginSystem:
		return c.checkSystem(ctx, p, now)
	default:
		return fmt.Errorf("rate ceiling: destructive op has unknown origin %q", p.Origin) // fail closed
	}
	// A gated op with a broken/absent gate FAILS CLOSED: a compromised path must
	// never sail through a gate that cannot evaluate.
	if c == nil || c.db == nil {
		return fmt.Errorf("rate ceiling: db not configured") // fail closed
	}
	// An op with no merchant has no window to count in: fail closed.
	if p.MerchantID == uuid.Nil {
		return fmt.Errorf("rate ceiling: destructive op has no merchant to scope its ceiling to") // fail closed
	}

	since := now.Add(-RateCeilingWindow).UTC()
	types := DestructiveIntentTypes()
	// Both counts take their scope (merchant or actor) as an argument, so this
	// root pool needs no openrails.merchant_id; GenDirectory is the right accessor.
	q := c.db.GenDirectory()

	merchantCount, err := q.CountDestructiveIntentsForMerchantSince(ctx, gen.CountDestructiveIntentsForMerchantSinceParams{
		MerchantID:  p.MerchantID,
		Origins:     antiTheftOrigins,
		IntentTypes: types,
		Since:       since,
	})
	if err != nil {
		return fmt.Errorf("rate ceiling: merchant count: %w", err) // fail closed
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

	counts := ceilingCounts{actor: actorCount, merchant: merchantCount}
	// Trip: per-actor first (more specific/actionable), then the merchant wall.
	if p.Actor != "" && actorCount >= PerActorHourlyCeiling {
		return c.trip(ctx, p, ceilingPerActor, counts, now)
	}
	if merchantCount >= PerMerchantHourlyCeiling {
		return c.trip(ctx, p, ceilingPerMerchant, counts, now)
	}

	// Early warning: this op crosses 50% of a ceiling (still below the wall).
	if p.Actor != "" && actorCount+1 >= perActorWarnThreshold {
		c.warn(ctx, p, ceilingPerActor, counts, now)
	}
	if merchantCount+1 >= perMerchantWarnThreshold {
		c.warn(ctx, p, ceilingPerMerchant, counts, now)
	}
	return nil
}

// checkSystem is the origin='system' leg: one merchant's automated destructive
// queueing per rolling hour, on its own window, so automation and the
// anti-theft budget never burn each other. No principal produced these ops, so
// there is no per-actor leg.
func (c *RateCeiling) checkSystem(ctx context.Context, p CheckParams, now time.Time) error {
	// Fail closed like the anti-theft legs. An unscoped op has no window to
	// count in; counting it deployment-wide is what this ceiling avoids.
	if c == nil || c.db == nil {
		return fmt.Errorf("rate ceiling: db not configured") // fail closed
	}
	if p.MerchantID == uuid.Nil {
		return fmt.Errorf("rate ceiling: system-origin destructive op has no merchant to scope its ceiling to") // fail closed
	}

	// The anti-theft leg's query with the system origin set. It takes the
	// merchant as an argument, so the root pool needs no openrails.merchant_id.
	count, err := c.db.GenDirectory().CountDestructiveIntentsForMerchantSince(ctx,
		gen.CountDestructiveIntentsForMerchantSinceParams{
			MerchantID:  p.MerchantID,
			Origins:     []string{string(OriginSystem)},
			IntentTypes: DestructiveIntentTypes(),
			Since:       now.Add(-RateCeilingWindow).UTC(),
		})
	if err != nil {
		return fmt.Errorf("rate ceiling: system merchant count: %w", err) // fail closed
	}

	counts := ceilingCounts{systemMerchant: count}
	if count >= PerMerchantSystemHourlyCeiling {
		return c.trip(ctx, p, ceilingSystemMerchant, counts, now)
	}
	if count+1 >= systemMerchantWarnThreshold {
		c.warn(ctx, p, ceilingSystemMerchant, counts, now)
	}
	return nil
}

// trip raises the high-severity operator finding + a log.Error, then returns the
// typed refusal. The refusal is returned REGARDLESS of whether the finding
// write succeeded — the wall is the safety; the alert is best-effort.
func (c *RateCeiling) trip(ctx context.Context, p CheckParams, which ceilingKind, counts ceilingCounts, now time.Time) error {
	fields := log.Fields{
		"gate":                  "destructive_rate_ceiling",
		"ceiling":               string(which),
		"actor":                 p.Actor,
		"merchant_id":           p.MerchantID,
		"intent_type":           p.IntentType,
		"origin":                string(p.Origin),
		"actor_count":           counts.actor,
		"merchant_count":        counts.merchant,
		"system_merchant_count": counts.systemMerchant,
		"per_actor_max":         PerActorHourlyCeiling,
		"per_merchant_max":      PerMerchantHourlyCeiling,
		"system_merchant_max":   PerMerchantSystemHourlyCeiling,
		"window":                cadence.FormatDuration(RateCeilingWindow),
	}
	log.WithContext(ctx).WithFields(fields).Error(
		"destructive-operation rate ceiling TRIPPED — refusing op; possible credential compromise or runaway automation, verify before resuming")

	subjectKey, action := c.trippedSubjectAction(which, p, counts)
	evidence := c.evidence(which, p, counts, now, true)
	if err := c.emitFinding(ctx, p.MerchantID, RateCeilingTrippedFindingType, subjectKey, "critical", action, evidence); err != nil {
		log.WithContext(ctx).WithFields(fields).WithError(err).Error(
			"destructive-rate ceiling: failed to raise tripped finding (op still refused)")
	}
	return &RateCeilingError{
		Ceiling:             which,
		Actor:               p.Actor,
		IntentType:          p.IntentType,
		ActorCount:          counts.actor,
		MerchantCount:       counts.merchant,
		SystemMerchantCount: counts.systemMerchant,
	}
}

// warn raises the early-warning finding (best-effort) + a log.Warn. Never blocks.
func (c *RateCeiling) warn(ctx context.Context, p CheckParams, which ceilingKind, counts ceilingCounts, now time.Time) {
	subjectKey := c.subjectKey(which, p)
	action := fmt.Sprintf(
		"destructive-op burst on merchant %s crossing 50%% of the %s ceiling (per-actor %d/%d, this merchant %d/%d, this merchant's automation %d/%d in %s) — watch for credential compromise or runaway automation; approve (ack) to clear once verified normal",
		p.MerchantID, which, counts.actor, PerActorHourlyCeiling, counts.merchant, PerMerchantHourlyCeiling,
		counts.systemMerchant, PerMerchantSystemHourlyCeiling, RateCeilingWindow)
	evidence := c.evidence(which, p, counts, now, false)
	log.WithContext(ctx).WithFields(log.Fields{
		"gate": "destructive_rate_ceiling", "ceiling": string(which), "actor": p.Actor,
		"merchant_id": p.MerchantID, "actor_count": counts.actor, "merchant_count": counts.merchant,
		"system_merchant_count": counts.systemMerchant,
	}).Warn("destructive-operation rate ceiling: burst crossing 50% of a ceiling")
	if err := c.emitFinding(ctx, p.MerchantID, RateCeilingWarningFindingType, subjectKey, "high", action, evidence); err != nil {
		log.WithContext(ctx).WithError(err).Warn("destructive-rate ceiling: failed to raise early-warning finding")
	}
}

// subjectKey keeps ONE standing finding per (merchant, type, subject):
// per-actor events key on the actor, the two merchant-wide walls on the
// merchant as separate subjects (a human burst and a runaway automation are
// different investigations).
func (c *RateCeiling) subjectKey(which ceilingKind, p CheckParams) string {
	switch which {
	case ceilingPerActor:
		return "actor:" + p.Actor
	case ceilingSystemMerchant:
		return "system:" + p.MerchantID.String()
	default:
		return "merchant:" + p.MerchantID.String()
	}
}

func (c *RateCeiling) trippedSubjectAction(which ceilingKind, p CheckParams, counts ceilingCounts) (subjectKey, action string) {
	subjectKey = c.subjectKey(which, p)
	switch which {
	case ceilingPerActor:
		action = fmt.Sprintf(
			"actor %s hit the per-actor destructive ceiling (%d in %s ≥ %d) — LIKELY CREDENTIAL COMPROMISE; verify and rotate, then approve (ack) to clear",
			p.Actor, counts.actor, RateCeilingWindow, PerActorHourlyCeiling)
	case ceilingSystemMerchant:
		action = fmt.Sprintf(
			"this merchant's AUTOMATION hit the system destructive ceiling (%d in %s ≥ %d) — no human asked for these; a convergence pass is running away or acting on bad provider data. Nothing was queued. Investigate the roster/dunning input, then approve (ack) to clear",
			counts.systemMerchant, RateCeilingWindow, PerMerchantSystemHourlyCeiling)
	default:
		action = fmt.Sprintf(
			"merchant %s hit its destructive ceiling (%d in %s ≥ %d across all of this merchant's actors) — LIKELY CREDENTIAL COMPROMISE on this merchant; verify and rotate, then approve (ack) to clear. Other merchants are unaffected: this wall is per-merchant",
			p.MerchantID, counts.merchant, RateCeilingWindow, PerMerchantHourlyCeiling)
	}
	return subjectKey, action
}

func (c *RateCeiling) evidence(which ceilingKind, p CheckParams, counts ceilingCounts, now time.Time, tripped bool) map[string]any {
	return map[string]any{
		"ceiling":                 string(which),
		"tripped":                 tripped,
		"actor":                   p.Actor,
		"origin":                  string(p.Origin),
		"intent_type":             p.IntentType,
		"merchant_id":             p.MerchantID.String(),
		"actor_count":             counts.actor,
		"merchant_count":          counts.merchant,
		"system_merchant_count":   counts.systemMerchant,
		"per_actor_ceiling":       PerActorHourlyCeiling,
		"per_merchant_ceiling":    PerMerchantHourlyCeiling,
		"system_merchant_ceiling": PerMerchantSystemHourlyCeiling,
		"window_hours":            int(RateCeilingWindow / time.Hour),
		"observed_at":             now.UTC().Format(time.RFC3339),
		recommend.EvidenceKey:     recommend.Recommendation{Action: recommend.ActionAckResume}.Map(),
	}
}

// emitFinding writes the operator finding on an INDEPENDENT merchant-scoped
// transaction (fresh pool connection with the merchant GUC pinned), so the
// alert PERSISTS even though the refused op rolls back the caller's transaction.
// A fresh background context (bounded) drops the caller's merchant-pinned
// connection so MerchantTx opens its own tx rather than nesting on the caller's.
func (c *RateCeiling) emitFinding(ctx context.Context, merchantID uuid.UUID, findingType, subjectKey, severity, action string, evidence map[string]any) error {
	ev, err := json.Marshal(evidence)
	if err != nil {
		return fmt.Errorf("marshal finding evidence: %w", err)
	}
	fctx, cancel := context.WithTimeout(merchant.WithID(context.Background(), billing.MerchantID(merchantID)), 5*time.Second)
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
// wins, otherwise the invoker the route gate admitted (a merchant route's
// staff, a customer route's customer), keyed as limits key it. Empty for
// system/background paths: the per-invoker ceiling is then inert (per-merchant
// still applies).
func ResolveActor(ctx context.Context, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if who, ok := admitted(ctx); ok {
		return billingauth.InvokerKey(who)
	}
	return ""
}

// admitted is who the route gate admitted on ctx.
func admitted(ctx context.Context) (billingauth.Identity, bool) {
	if staff, ok := billingauth.StaffFromContext(ctx); ok {
		return staff.Identity, true
	}
	return billingauth.IdentityFromContext(ctx)
}
