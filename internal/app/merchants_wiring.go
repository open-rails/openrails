package app

import (
	"context"

	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/merchantsecrets"
)

// EnsureMerchantsService builds the per-merchant secrets service when the
// composition root hasn't (#699). Embedded hosts that never construct the
// standalone HTTP server (which builds its own, with Vault capability probing
// for route gating, and sets Runtime.Merchants first) still need it so the
// pull plane — provider refresh, unknown-cohort resolution, per-sub probes —
// can arm per merchant from the merchant-secrets store the manifest seeds.
//
// Failure is a loud degradation, not a boot error: the pull plane falls back
// to the boot-config rails plane only. Hosts that provision secrets still hit
// the hard #667 encryption-posture gate on their provisioning path.
func (r *Runtime) EnsureMerchantsService(ctx context.Context) {
	if r == nil || r.Merchants != nil || r.DB == nil || r.Config == nil {
		return
	}
	backend, err := merchantsecrets.Build(ctx, r.Config, r.DB.DataPool())
	if err != nil {
		log.WithError(err).Warn("merchant secret store unavailable; provider pulls arm from boot-config rails only (#699)")
		return
	}
	svc, err := merchants.NewService(r.DB.DataPool(), backend.Secrets, config.ExpectedProviderEnvironment(r.Config.IsTestMode()))
	if err != nil {
		log.WithError(err).Warn("merchants service unavailable; provider pulls arm from boot-config rails only (#699)")
		return
	}
	r.Merchants = svc
}
