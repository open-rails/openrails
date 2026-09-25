package embed

import (
	"context"
	"fmt"
)

// Ready is the embedded engine's readiness (#748), the same check the
// standalone /readyz runs: Postgres, the merchants service and River. Optional
// providers (Redis, Vault, PSP posture) never fail it; see Probes.
func (r *Runtime) Ready(ctx context.Context) error {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return fmt.Errorf("embedded: not initialized")
	}
	_, err := r.app.Runtime.Ready(ctx)
	return err
}

// Probe checks one optional provider the runtime depends on.
type Probe struct {
	// Name is stable for dashboards: "openrails_vault", "openrails_psp_posture",
	// "openrails_solana_signer_identity".
	Name string
	// Check returns nil while the provider is usable; it honors ctx.
	Check func(context.Context) error
}

// Probes lists the optional providers this runtime reconnects to in the
// background, for registration with the host's dependency supervisor as
// optional dependencies:
//
//	for _, p := range rt.Probes() {
//		sup.Add(p.Name, deps.Optional, p.Check, nil)
//	}
//
// "openrails_vault" (only when the runtime logs in to Vault itself) is a live
// token self-lookup. "openrails_psp_posture" reads cached verdicts and fails
// while any loaded PSP is unverified or disarmed.
// "openrails_solana_signer_identity" (with Vault) fails while a Transit
// signer key no longer matches its stored Solana identity; the Solana rail
// refuses (503) until an operator approves the new identity
// (operator.ApproveSolanaSigner). Alert on it.
// Redis is the host's own client and is not listed.
func (r *Runtime) Probes() []Probe {
	if r == nil || r.app == nil || r.app.Runtime == nil {
		return nil
	}
	rt := r.app.Runtime
	var out []Probe
	if rt.UsesVault() {
		out = append(out,
			Probe{Name: "openrails_vault", Check: rt.VaultProbe},
			Probe{Name: "openrails_solana_signer_identity", Check: func(context.Context) error { return rt.SignerIdentityState() }})
	}
	return append(out, Probe{Name: "openrails_psp_posture", Check: func(context.Context) error { return rt.PostureState() }})
}
