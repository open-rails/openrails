package app

import (
	"context"
	"fmt"
)

// ReadinessDependency is one boot-dependency probed by Runtime.Ready (#748).
type ReadinessDependency struct {
	// Name identifies the dependency (e.g. "postgres", "garnet", "merchant_secrets", "river").
	Name string
	// Available is true when the dependency is configured (where relevant) and reachable.
	Available bool
	// Err is nil when Available; otherwise the reason (missing config or a live probe failure).
	Err error
}

// Ready runs the #748 canonical dependency probes shared by EVERY host
// surface — standalone (/readyz, internal/http/routes_public.go) and embedded
// (pkg/embedded.Embedded.Ready) both call this so neither can report healthy
// while the other reports degraded.
//
// Checks: Postgres (always required), Redis (probed ONLY when configured —
// Redis is optional everywhere else in OpenRails, so its absence must not
// fail readiness; when configured, an unreachable Redis DOES fail it), the
// merchant-secret backend (armed at boot, AND live-reachable when armed with
// a Vault-backed store — a paused/unreachable Vault fails this even though
// arming succeeded earlier), and River producer presence.
//
// Returns every dependency's status (for verbose diagnostics) plus a single
// wrapped error naming the FIRST failing one; nil when everything the running
// configuration actually depends on is healthy.
func (r *Runtime) Ready(ctx context.Context) ([]ReadinessDependency, error) {
	if r == nil {
		dep := ReadinessDependency{Name: "runtime", Err: fmt.Errorf("not initialized")}
		return []ReadinessDependency{dep}, fmt.Errorf("readiness: %s: %w", dep.Name, dep.Err)
	}

	var deps []ReadinessDependency
	probe := func(name string, err error) {
		deps = append(deps, ReadinessDependency{Name: name, Available: err == nil, Err: err})
	}

	var pgErr error
	if r.DB == nil || r.DB.Pool() == nil {
		pgErr = fmt.Errorf("not configured")
	} else {
		pgErr = r.DB.Pool().Ping(ctx)
	}
	probe("postgres", pgErr)

	// Redis (garnet) is optional everywhere else in OpenRails — only probe it,
	// and only fail readiness on it, when the runtime actually holds a client.
	if r.RedisClient != nil {
		probe("garnet", r.RedisClient.Ping(ctx).Err())
	}

	probe("merchant_secrets", r.merchantSecretsReady(ctx))

	var riverErr error
	if r.RiverProducer == nil {
		riverErr = fmt.Errorf("river producer not initialized")
	}
	probe("river", riverErr)

	for _, d := range deps {
		if !d.Available {
			return deps, fmt.Errorf("readiness: %s: %w", d.Name, d.Err)
		}
	}
	return deps, nil
}

// merchantSecretsReady checks the #699/#723/#748 arming contract: the
// merchants service (or, in MODE 1, the manifest plane) must be armed, and —
// when arming built a live backend (Vault) — that backend must still answer.
func (r *Runtime) merchantSecretsReady(ctx context.Context) error {
	if r.Config != nil && r.Config.IsManifestMerchantSource() {
		if r.ManifestSecrets == nil {
			return fmt.Errorf("manifest secret plane not armed (#723)")
		}
		return nil
	}
	if r.Merchants == nil {
		return fmt.Errorf("merchants service not armed (secret-store build failed at boot, #699/#748)")
	}
	if r.MerchantSecretPing != nil {
		if err := r.MerchantSecretPing(ctx); err != nil {
			return fmt.Errorf("backend unreachable: %w", err)
		}
	}
	return nil
}
