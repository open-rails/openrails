package embed

import (
	"context"
	"fmt"
)

// Ready is the embedded engine's REAL, supported readiness probe (#748) —
// billing health endpoints are not exposed in embedded mode, so a host that
// wants billing readiness calls this (not the never-existent IsBillingReady)
// from its own /readyz/health-check handler.
//
// It delegates to the internal/app.Runtime checks shared with the standalone
// surface's /readyz, so both report the SAME posture: Postgres, Redis (only
// when configured), the merchant-secret backend (armed + live-reachable), and
// River producer presence. OpenRails-managed River additionally requires the
// local consumer to be running; a host-owned shared River client is outside
// this process-level check, and hosts use CheckJobProgress for its live fleet
// verdict. Returns a wrapped error naming the first failing dependency; nil
// when every dependency the running configuration actually depends on is
// healthy.
func (r *Runtime) Ready(ctx context.Context) error {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return fmt.Errorf("embedded: not initialized")
	}
	_, err := r.app.Runtime.Ready(ctx)
	return err
}
