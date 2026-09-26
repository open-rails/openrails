package app

import (
	"context"
	"fmt"
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
// Runtime.Ready. Only Postgres and River are required. Redis, Vault and PSP
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

	var riverErr error
	if r.hostRiver && !r.hostRiverBound.Load() {
		riverErr = fmt.Errorf("host-owned River is not bound; compose RiverJobs with riverhelpers.New")
	} else if r.RiverProducer == nil {
		riverErr = fmt.Errorf("river producer not initialized")
	}
	add("river", false, riverErr)
	if !r.hostRiver && !r.externalRiverClient {
		var consumerErr error
		if !r.workerConsumerRunning.Load() {
			consumerErr = fmt.Errorf("managed River worker consumer is not running")
		}
		add("river_consumer", false, consumerErr)
	}

	if r.RedisClient != nil {
		if observed, err := r.redisState.observed(); observed {
			add("redis", true, err)
		}
	}
	if r.MerchantSecretBackend != nil && r.MerchantSecretBackend.VaultAuth != nil {
		add("vault", true, r.MerchantSecretBackend.State())
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
	if r == nil || r.MerchantSecretBackend == nil {
		return nil
	}
	return r.MerchantSecretBackend.Probe(ctx)
}

// UsesVault reports whether this runtime supervises its own Vault login.
func (r *Runtime) UsesVault() bool {
	return r != nil && r.MerchantSecretBackend != nil && r.MerchantSecretBackend.VaultAuth != nil
}

// PostureState is the cached PSP posture: nil when every loaded PSP is
// verified and armed. It never contacts a provider.
func (r *Runtime) PostureState() error {
	if r == nil {
		return nil
	}
	return r.postureState()
}
