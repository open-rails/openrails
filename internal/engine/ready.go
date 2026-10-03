package engine

import (
	"context"
)

// Ready is the standalone /readyz check: Postgres, the merchants service and
// River (a host-owned fleet must have bound RiverJobs; a managed one must be
// started). Optional providers never fail it; see Probes.
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
		out = append(out,
			Probe{Name: "openrails_vault", Check: rt.VaultProbe},
			Probe{Name: "openrails_solana_signer_identity", Check: func(context.Context) error { return rt.SignerIdentityState() }})
	}
	return append(out,
		Probe{Name: "openrails_psp_posture", Check: func(context.Context) error { return rt.PostureState() }},
		Probe{Name: "openrails_job_progress", Check: func(ctx context.Context) error {
			report, err := rt.RiverProgress(ctx)
			if err != nil {
				return err
			}
			return report.Err()
		}})
}
