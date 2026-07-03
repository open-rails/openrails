package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// #348 probe-cooldown cache. The NMI test-mode boot probe costs one declined
// authorization per boot on a live account; a crash-looping supervisor pays it
// on every restart and risks card-testing flags at the gateway. Verdicts are
// therefore persisted (openrails.probe_verdicts, migration 009 — RLS-exempt,
// instance-level credential state) and consulted before probing:
//
//   - a 'live' verdict younger than probeVerdictCooldown refuses to boot FROM
//     CACHE without re-probing;
//   - a 'simulated' verdict younger than the cooldown skips the probe;
//   - a different key (sha256 mismatch) or a stale verdict always re-probes;
//   - any cache read/write failure degrades to today's behavior (probe).

const probeVerdictCooldown = 12 * time.Hour

const (
	probeVerdictLive      = "live"
	probeVerdictSimulated = "simulated"
)

// probeCacheAction is what a cached verdict tells the boot sequence to do.
type probeCacheAction int

const (
	// probeCacheMiss: no usable cache entry — probe as before.
	probeCacheMiss probeCacheAction = iota
	// probeCacheRefuseBoot: fresh 'live' verdict — refuse to boot without
	// paying another declined auth.
	probeCacheRefuseBoot
	// probeCacheSkipProbe: fresh 'simulated' verdict — boot without probing.
	probeCacheSkipProbe
)

// probeCacheDecision is the pure cooldown policy: only fresh, known verdicts
// short-circuit the probe.
func probeCacheDecision(verdict string, checkedAt, now time.Time) probeCacheAction {
	if now.Sub(checkedAt) >= probeVerdictCooldown || checkedAt.After(now) {
		return probeCacheMiss
	}
	switch verdict {
	case probeVerdictLive:
		return probeCacheRefuseBoot
	case probeVerdictSimulated:
		return probeCacheSkipProbe
	}
	return probeCacheMiss
}

// probeKeyHash is the cache identity of a credential: sha256 hex of the
// security key, so a rotated key never reuses an old verdict and the key
// itself is never stored.
func probeKeyHash(securityKey string) string {
	sum := sha256.Sum256([]byte(securityKey))
	return hex.EncodeToString(sum[:])
}

// lookupProbeVerdict reads a cached verdict. ok=false on miss OR any error
// (degrade to probing); errors are logged, never propagated.
func lookupProbeVerdict(database *db.DB, rail, keyHash string) (verdict string, checkedAt time.Time, ok bool) {
	if database == nil {
		return "", time.Time{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	row, err := database.Gen(ctx).GetProbeVerdict(ctx, gen.GetProbeVerdictParams{
		Rail:    rail,
		KeyHash: keyHash,
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			log.WithError(err).WithField("rail", rail).
				Warn("probe verdict cache read failed; probing as usual (#348)")
		}
		return "", time.Time{}, false
	}
	return row.Verdict, row.CheckedAt, true
}

// storeProbeVerdict persists a conclusive probe verdict. Failures only log —
// the cache is an optimization, never a correctness dependency.
func storeProbeVerdict(database *db.DB, rail, keyHash, verdict string) {
	if database == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := database.Gen(ctx).UpsertProbeVerdict(ctx, gen.UpsertProbeVerdictParams{
		Rail:    rail,
		KeyHash: keyHash,
		Verdict: verdict,
	}); err != nil {
		log.WithError(err).WithField("rail", rail).
			Warn("probe verdict cache write failed; next boot will re-probe (#348)")
	}
}
