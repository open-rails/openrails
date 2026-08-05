package intents

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db"
)

// The ceiling is a hardcoded safeguard, NOT config — pin the numbers so a
// silent drift (or an "attacker raises the knob" change) fails the build.
func TestRateCeiling_ConstantsAreWirePinned(t *testing.T) {
	if RateCeilingWindow != time.Hour {
		t.Fatalf("RateCeilingWindow = %s, want 1h", RateCeilingWindow)
	}
	if PerActorHourlyCeiling != 5 {
		t.Fatalf("PerActorHourlyCeiling = %d, want 5", PerActorHourlyCeiling)
	}
	// or#866: the anti-theft wall is PER MERCHANT. The number is unchanged; a
	// shared deployment budget let one merchant's ordinary traffic refuse every
	// other merchant's destructive ops (cross-tenant DoS).
	if PerMerchantHourlyCeiling != 15 {
		t.Fatalf("PerMerchantHourlyCeiling = %d, want 15", PerMerchantHourlyCeiling)
	}
	// Early-warning thresholds are ceil(50% of the ceiling).
	if perActorWarnThreshold != 3 {
		t.Fatalf("perActorWarnThreshold = %d, want 3", perActorWarnThreshold)
	}
	if perMerchantWarnThreshold != 8 {
		t.Fatalf("perMerchantWarnThreshold = %d, want 8", perMerchantWarnThreshold)
	}
	// or#842: the automated leg. Its own window, disjoint from the anti-theft
	// one — automation must never burn a budget that bounds a stolen credential.
	if PerMerchantSystemHourlyCeiling != 50 {
		t.Fatalf("PerMerchantSystemHourlyCeiling = %d, want 50", PerMerchantSystemHourlyCeiling)
	}
	if systemMerchantWarnThreshold != 25 {
		t.Fatalf("systemMerchantWarnThreshold = %d, want 25", systemMerchantWarnThreshold)
	}
}

// The finding types must satisfy reconciliation_findings' type CHECK regex, or
// the upsert (and thus the operator alert) fails silently.
func TestRateCeiling_FindingTypesMatchSchemaRegex(t *testing.T) {
	re := regexp.MustCompile(`^(pull|derive|life|consistency)\.[a-z0-9_]+(\.[a-z0-9_]+)?$`)
	for _, ft := range []string{RateCeilingTrippedFindingType, RateCeilingWarningFindingType} {
		if !re.MatchString(ft) {
			t.Fatalf("finding type %q does not match reconciliation_findings type regex", ft)
		}
	}
}

// A gate that cannot evaluate (nil DB / broken pool) must REFUSE the gated op,
// never allow it through — and the refusal is NOT a trip (it is a hard error a
// caller must surface as a failure, not a client "rate limited").
func TestRateCeiling_GateEvalErrorFailsClosed(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	destructive := DestructiveIntentTypes()[0]

	// nil gate.
	var nilGate *RateCeiling
	if err := nilGate.Check(ctx, CheckParams{
		Actor: "a", MerchantID: uuid.New(), IntentType: destructive, Origin: OriginUser,
	}, now); err == nil {
		t.Fatal("nil gate must fail closed for a destructive user op")
	}

	// present gate, unusable DB (no pool) => count query errors => fail closed.
	broken := NewRateCeiling(db.NewWithPgxTx(nil))
	err := broken.Check(ctx, CheckParams{
		Actor: "a", MerchantID: uuid.New(), IntentType: destructive, Origin: OriginUser,
	}, now)
	if err == nil {
		t.Fatal("broken gate must fail closed for a destructive user op")
	}
	if errors.Is(err, ErrRateCeilingTripped) {
		t.Fatal("a gate-eval error must NOT masquerade as a ceiling trip")
	}
	var rce *RateCeilingError
	if errors.As(err, &rce) {
		t.Fatal("a gate-eval error must not be a *RateCeilingError")
	}

	// or#842: system origin is gated too, and fails closed the same way. It
	// used to return nil here — the gate was absent for exactly the paths that
	// queue the most irreversible work with no human in the loop.
	if err := broken.Check(ctx, CheckParams{
		Actor: "", MerchantID: uuid.New(), IntentType: destructive, Origin: OriginSystem,
	}, now); err == nil {
		t.Fatal("broken gate must fail closed for a destructive SYSTEM op")
	}
	// An op with no merchant has no window to count in — on EITHER leg, now
	// that the anti-theft wall is per-merchant too (or#866). Fail closed rather
	// than fall back to some deployment-wide count.
	if err := NewRateCeiling(nil).Check(ctx, CheckParams{
		MerchantID: uuid.Nil, IntentType: destructive, Origin: OriginSystem,
	}, now); err == nil {
		t.Fatal("an unscoped system destructive op must fail closed")
	}
	if err := NewRateCeiling(nil).Check(ctx, CheckParams{
		Actor: "a", MerchantID: uuid.Nil, IntentType: destructive, Origin: OriginUser,
	}, now); err == nil {
		t.Fatal("an unscoped user destructive op must fail closed")
	}
}

// The gate is inert for non-gated ops even when its DB is unusable: a broken
// gate must not fail-close ordinary traffic (only destructive ops).
func TestRateCeiling_InertForNonGatedOps(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	broken := NewRateCeiling(db.NewWithPgxTx(nil))

	// non-destructive user op (e.g. a payment-source update) is not gated.
	if err := broken.Check(ctx, CheckParams{
		Actor: "a", MerchantID: uuid.New(), IntentType: "nmi_payment_source_update", Origin: OriginUser,
	}, now); err != nil {
		t.Fatalf("non-destructive op must pass even a broken gate: %v", err)
	}
}

func TestResolveActor(t *testing.T) {
	// explicit override wins.
	if got := ResolveActor(context.Background(), "explicit"); got != "explicit" {
		t.Fatalf("ResolveActor explicit = %q", got)
	}
	// no principal, no override => empty (system/background paths).
	if got := ResolveActor(context.Background(), ""); got != "" {
		t.Fatalf("ResolveActor empty = %q, want empty", got)
	}
}
