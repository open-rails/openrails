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
// MODE 1 (#723, merchant_source=manifest): the store is the runtime's
// in-memory manifest plane — no DB/Vault store is ever constructed.
//
// Failure is a loud degradation, not a boot error: the pull plane falls back
// to the boot-config rails plane only. Hosts that provision secrets still hit
// the hard #667 encryption-posture gate on their provisioning path.
func (r *Runtime) EnsureMerchantsService(ctx context.Context) {
	if r == nil || r.Merchants != nil || r.DB == nil || r.Config == nil {
		return
	}
	var store merchants.MerchantSecretStore
	if r.Config.IsManifestMerchantSource() {
		if r.ManifestSecrets == nil {
			log.Warn("merchant_source=manifest but the manifest secret plane is missing; provider pulls arm from boot-config rails only (#723)")
			return
		}
		store = r.ManifestSecrets
	} else {
		backend, err := merchantsecrets.Build(ctx, r.Config, r.DB.DataPool())
		if err != nil {
			log.WithError(err).Warn("merchant secret store unavailable; provider pulls arm from boot-config rails only (#699)")
			return
		}
		store = backend.Secrets
	}
	svc, err := merchants.NewService(r.DB.DataPool(), store, config.ExpectedProviderEnvironment(r.Config.IsTestMode()))
	if err != nil {
		log.WithError(err).Warn("merchants service unavailable; provider pulls arm from boot-config rails only (#699)")
		return
	}
	r.ArmMerchantsService(svc, store)
}

// ArmMerchantsService installs the merchants service AND wires its store into
// the request-time credential consumers (checkout NMI-sale resolution, vault
// saves) the way the standalone server does — embedded hosts otherwise served
// those from the boot-config plane only, which in MODE 1 (#723) is empty.
func (r *Runtime) ArmMerchantsService(svc *merchants.Service, store merchants.MerchantSecretReader) {
	if r == nil || svc == nil {
		return
	}
	r.Merchants = svc
	if r.CheckoutService != nil {
		r.CheckoutService.SetMerchantSecretStore(store)
		r.CheckoutService.SetRailMerchantAccountSecretResolver(svc)
	}
	if r.VaultService != nil {
		r.VaultService.SetMerchantSecretStore(store)
		r.VaultService.SetRailMerchantAccountSecretResolver(svc)
	}
}
