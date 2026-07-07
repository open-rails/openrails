package nmi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db/gen"
)

// #348 probe-cooldown cache, reinstated at arm time (the #788 rail-layering
// refactor deleted the boot-time client map this used to hang off of, with no
// replacement). Verdicts persist in openrails.probe_verdicts (RLS-exempt,
// instance-level credential state — the table and its sqlc queries survived
// #788) and are consulted before every probe:
//
//   - a 'live' verdict younger than ProbeVerdictCooldown refuses the arm FROM
//     CACHE without re-probing;
//   - a 'simulated' verdict younger than the cooldown skips the probe;
//   - a different key (sha256 mismatch) or a stale verdict always re-probes;
//   - any cache read/write failure degrades to probing (a cache outage never
//     blocks or wrongly refuses an arm).
const ProbeVerdictCooldown = 12 * time.Hour

const (
	ProbeVerdictLive      = "live"
	ProbeVerdictSimulated = "simulated"
)

// ProbeCacheAction is what a cached verdict tells the caller to do.
type ProbeCacheAction int

const (
	// ProbeCacheMiss: no usable cache entry — probe as before.
	ProbeCacheMiss ProbeCacheAction = iota
	// ProbeCacheRefuseBoot: fresh 'live' verdict — refuse without paying
	// another declined auth.
	ProbeCacheRefuseBoot
	// ProbeCacheSkipProbe: fresh 'simulated' verdict — proceed without probing.
	ProbeCacheSkipProbe
)

// ProbeCacheDecision is the pure cooldown policy: only fresh, known verdicts
// short-circuit the probe.
func ProbeCacheDecision(verdict string, checkedAt, now time.Time) ProbeCacheAction {
	if now.Sub(checkedAt) >= ProbeVerdictCooldown || checkedAt.After(now) {
		return ProbeCacheMiss
	}
	switch verdict {
	case ProbeVerdictLive:
		return ProbeCacheRefuseBoot
	case ProbeVerdictSimulated:
		return ProbeCacheSkipProbe
	}
	return ProbeCacheMiss
}

// ProbeKeyHash is the cache identity of a credential: sha256 hex of the
// security key, so a rotated key never reuses an old verdict and the key
// itself is never stored.
func ProbeKeyHash(securityKey string) string {
	sum := sha256.Sum256([]byte(securityKey))
	return hex.EncodeToString(sum[:])
}

// LookupProbeVerdict reads a cached verdict for cacheKey — an opaque,
// caller-scoped identity (e.g. "<merchant_id>:<rail>:<environment>:<account_id>")
// so verdicts never leak across merchants or accounts. ok=false on miss OR any
// error (degrade to probing); errors are logged, never propagated.
func LookupProbeVerdict(q *gen.Queries, cacheKey, keyHash string) (verdict string, checkedAt time.Time, ok bool) {
	if q == nil {
		return "", time.Time{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row, err := q.GetProbeVerdict(ctx, gen.GetProbeVerdictParams{Rail: cacheKey, KeyHash: keyHash})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.WithError(err).WithField("probe_cache_key", cacheKey).
				Warn("probe verdict cache read failed; probing as usual (#348)")
		}
		return "", time.Time{}, false
	}
	return row.Verdict, row.CheckedAt, true
}

// StoreProbeVerdict persists a conclusive probe verdict. Failures only log —
// the cache is an optimization, never a correctness dependency.
func StoreProbeVerdict(q *gen.Queries, cacheKey, keyHash, verdict string) {
	if q == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.UpsertProbeVerdict(ctx, gen.UpsertProbeVerdictParams{Rail: cacheKey, KeyHash: keyHash, Verdict: verdict}); err != nil {
		log.WithError(err).WithField("probe_cache_key", cacheKey).
			Warn("probe verdict cache write failed; next check will re-probe (#348)")
	}
}

// ArmDecision is the outcome of checking whether an NMI account may be armed
// (booted / API-armed) under test_mode.
type ArmDecision struct {
	// Refuse is true when the account is conclusively PRODUCTION (live,
	// whether from cache or a fresh probe) — the arm must be refused.
	Refuse bool
	// Cached is true when Refuse (or a clean pass) came from a cached
	// verdict rather than a fresh probe this call.
	Cached bool
	// CheckedAt is the cached verdict's timestamp (zero when Cached=false).
	CheckedAt time.Time
	// ProbeErr is set when a fresh probe was indeterminate (transport or
	// credential failure). Refuse is always false in that case: an
	// indeterminate probe is NEVER fail-closed (#348) — only a conclusive
	// "live" verdict refuses. Callers should log this and let the arm
	// proceed.
	ProbeErr error
}

// CheckTestModeArm applies the #348 posture to arming one NMI account under
// test_mode: cache-first, then a live probe (client.ProbeTestMode). Call ONLY
// once the caller has confirmed test_mode is enabled and client.SecurityKey is
// non-empty — this performs a real (harmless-on-live, #348) network probe
// against the gateway.
func CheckTestModeArm(q *gen.Queries, client *NMIClient, cacheKey string) ArmDecision {
	keyHash := ProbeKeyHash(client.SecurityKey)
	if verdict, checkedAt, ok := LookupProbeVerdict(q, cacheKey, keyHash); ok {
		switch ProbeCacheDecision(verdict, checkedAt, time.Now()) {
		case ProbeCacheRefuseBoot:
			return ArmDecision{Refuse: true, Cached: true, CheckedAt: checkedAt}
		case ProbeCacheSkipProbe:
			return ArmDecision{Cached: true, CheckedAt: checkedAt}
		}
	}
	result, probeErr := client.ProbeTestMode()
	switch result {
	case ProbeLive:
		StoreProbeVerdict(q, cacheKey, keyHash, ProbeVerdictLive)
		return ArmDecision{Refuse: true}
	case ProbeSimulated:
		StoreProbeVerdict(q, cacheKey, keyHash, ProbeVerdictSimulated)
		return ArmDecision{}
	default:
		// Indeterminate verdicts are never cached: the next check re-probes
		// once the transport/credential issue clears.
		return ArmDecision{ProbeErr: probeErr}
	}
}
