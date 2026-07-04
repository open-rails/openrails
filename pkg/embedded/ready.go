package embedded

import (
	"context"
	"fmt"
)

// Ready is the embedded engine's REAL, supported readiness probe (#748) —
// what the HTTPHandlerOptions doc above used to point hosts at under the name
// IsBillingReady, which never existed. Call this from the host's own
// /readyz/health-check handler.
//
// It delegates to the internal/app.Runtime checks shared with the standalone
// surface's /readyz, so both report the SAME posture: Postgres, Redis (only
// when configured), the merchant-secret backend (armed + live-reachable), and
// River producer presence. Returns a wrapped error naming the first failing
// dependency; nil when every dependency the running configuration actually
// depends on is healthy.
func (e *Embedded) Ready(ctx context.Context) error {
	if e == nil || e.app == nil || e.app.Runtime == nil {
		return fmt.Errorf("embedded: not initialized")
	}
	_, err := e.app.Runtime.Ready(ctx)
	return err
}
