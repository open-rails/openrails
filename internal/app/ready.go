package app

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/retry"
)

// ReadinessDependency is one dependency reported by Runtime.Ready (#748).
type ReadinessDependency struct {
	// Name identifies the dependency (e.g. "postgres", "river", "redis", "vault").
	Name string
	// Available is true when the dependency is usable.
	Available bool
	// Optional dependencies never fail readiness; while unavailable only the
	// features that need them answer 503.
	Optional bool
	// Err is nil when Available; otherwise the reason.
	Err error
}

// Ready is the readiness shared by the standalone /readyz and embedded
// Runtime.Ready. Postgres, River (the host's fleet bound to RiverJobs, or
// OpenRails' own running) and a declared catalog (applied) are required. Redis, Vault and PSP
// posture are reported from cached background state as optional (degraded)
// entries; Ready never contacts them.
func (r *Runtime) Ready(ctx context.Context) ([]ReadinessDependency, error) {
	if r == nil || r.riverClosed.Load() {
		dep := ReadinessDependency{Name: "runtime", Err: fmt.Errorf("not initialized")}
		return []ReadinessDependency{dep}, fmt.Errorf("readiness: %s: %w", dep.Name, dep.Err)
	}

	var deps []ReadinessDependency
	add := func(name string, optional bool, err error) {
		deps = append(deps, ReadinessDependency{Name: name, Available: err == nil, Optional: optional, Err: err})
	}

	var pgErr error
	if r.DB == nil || r.DB.Pool() == nil {
		pgErr = fmt.Errorf("not configured")
	} else {
		pgErr = r.DB.Pool().Ping(ctx)
	}
	add("postgres", false, pgErr)

	var merchantsErr error
	if r.Merchants == nil {
		merchantsErr = fmt.Errorf("merchants service not armed (#699)")
	}
	add("merchants", false, merchantsErr)
	if observed, err := r.declaredCatalog.observed(); observed {
		add("catalog", false, err)
	}

	var riverErr error
	if !r.RiverBound() {
		riverErr = fmt.Errorf("River is not running: call Client.Start")
	}
	add("river", false, riverErr)
	if riverErr == nil && !r.externalRiverClient {
		var consumerErr error
		if !r.workerConsumerRunning.Load() {
			consumerErr = fmt.Errorf("OpenRails' River is bound but not running: call Client.Start")
		}
		add("river_consumer", false, consumerErr)
	}

	// A declared Redis that does not answer is degraded: abuse state is in
	// this process's memory meanwhile.
	if r.AbuseState.UsesRedis() {
		add("redis", true, r.AbuseState.RedisErr())
	}
	if r.UsesVault() {
		add("vault", true, r.Vault.State())
	}
	add("psp_posture", true, r.postureState())

	for _, d := range deps {
		if !d.Available && !d.Optional {
			return deps, fmt.Errorf("readiness: %s: %w", d.Name, d.Err)
		}
	}
	return deps, nil
}

// VaultProbe is the live Vault check for a host dependency supervisor; nil
// when this runtime owns no Vault login.
func (r *Runtime) VaultProbe(ctx context.Context) error {
	if r == nil || r.Vault == nil {
		return nil
	}
	return r.Vault.Probe(ctx)
}

// UsesVault reports whether this runtime supervises its own Vault login.
func (r *Runtime) UsesVault() bool {
	return r != nil && r.Vault != nil && r.Vault.Auth != nil
}

// PostureState is the cached PSP posture: nil when every loaded PSP is
// verified and armed. It never contacts a provider.
func (r *Runtime) PostureState() error {
	if r == nil {
		return nil
	}
	return r.postureState()
}

// startRedisMonitor observes optional Redis health without delaying startup:
// a request that finds Redis failing leaves it for memory, and this loop
// hears it answer again. The runtime waits for it to stop before closing
// Redis.
func (r *Runtime) startRedisMonitor() {
	if !r.AbuseState.UsesRedis() {
		return
	}
	r.Go("redis health", func(ctx context.Context) {
		for attempt := 0; ; {
			pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := r.AbuseState.Probe(pingCtx)
			cancel()
			if ctx.Err() != nil {
				return
			}
			wait := 10 * time.Second
			if err != nil {
				wait = retry.Backoff(attempt, retry.Base, retry.Max)
				attempt++
			} else {
				attempt = 0
			}
			if !retry.Sleep(ctx, wait) {
				return
			}
		}
	})
}
