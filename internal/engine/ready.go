package engine

import (
	"context"
)

// Ready is the standalone /readyz check: Postgres, the merchants service and
// River (the host's fleet bound to RiverJobs, or OpenRails' own running since
// Start). Optional providers never fail it; see
// Probes.
func (e *Engine) Ready(ctx context.Context) error {
	_, err := e.App.Runtime.Ready(ctx)
	return err
}

// Probe checks one optional dependency.
type Probe struct {
	Name  string
	Check func(context.Context) error
}

// Probes lists the optional dependencies the engine reconnects to in the
// background, for a host's dependency supervisor.
func (e *Engine) Probes() []Probe {
	rt := e.App.Runtime
	var out []Probe
	if rt.UsesVault() {
		out = append(out, Probe{Name: "openrails_vault", Check: rt.VaultProbe})
	}
	return append(out,
		Probe{Name: "openrails_psp_posture", Check: func(context.Context) error { return rt.PostureState() }},
		Probe{Name: "openrails_configuration", Check: func(context.Context) error { return rt.RefusalState() }},
		Probe{Name: "openrails_solana_crankers", Check: rt.SolanaCrankersCheck},
		Probe{Name: "openrails_job_progress", Check: rt.JobProgress})
}
