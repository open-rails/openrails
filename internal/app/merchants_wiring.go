package app

import (
	"context"
	"fmt"

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
// Failure posture (#748, mirrors #667's encryption-posture gate): outside
// development a failure to arm is a boot ERROR, not a loud degradation —
// silently falling back to boot-config-only rails means webhooks 503 forever
// behind a green /readyz, exactly the "degraded state that boots healthy"
// #748 closes. Development keeps the original warn-and-continue so a
// from-scratch dev boot with no Vault/master key still runs. Either way, the
// armed/unarmed state (and, once armed, live backend reachability) is
// surfaced by Runtime.Ready (#748) — never invisible after this returns.
func (r *Runtime) EnsureMerchantsService(ctx context.Context) error {
	if r == nil || r.Merchants != nil || r.DB == nil || r.Config == nil {
		return nil
	}
	var store merchants.MerchantSecretStore
	var ping func(context.Context) error
	if r.Config.IsManifestMerchantSource() {
		if r.ManifestSecrets == nil {
			return r.armingFailure(fmt.Errorf("merchant_source=manifest but the manifest secret plane is missing (#723)"))
		}
		store = r.ManifestSecrets
	} else {
		backend, err := merchantsecrets.Build(ctx, r.Config, r.DB.DataPool())
		if err != nil {
			return r.armingFailure(fmt.Errorf("merchant secret store unavailable (#699): %w", err))
		}
		store = backend.Secrets
		ping = backend.Ping
	}
	svc, err := merchants.NewService(r.DB.DataPool(), store, config.ExpectedProviderEnvironment(r.Config.IsTestMode()))
	if err != nil {
		return r.armingFailure(fmt.Errorf("merchants service unavailable (#699): %w", err))
	}
	r.ArmMerchantsService(svc, store)
	r.MerchantSecretPing = ping
	return nil
}

// armingFailure applies the #667-style environment gate to a merchants-arming
// failure: a boot error everywhere except development, where it warns and lets
// the pull plane fall back to the boot-config rails plane only.
func (r *Runtime) armingFailure(err error) error {
	if r.Config.IsDev() {
		log.WithError(err).Warn("merchants service failed to arm; provider pulls fall back to boot-config rails only (#699) — development only, refuses boot outside development (#748)")
		return nil
	}
	return fmt.Errorf("merchants service failed to arm outside development (#748): %w", err)
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
	if r.RailPaymentMethodService != nil {
		r.RailPaymentMethodService.SetMerchantSecretStore(store)
		r.RailPaymentMethodService.SetRailMerchantAccountSecretResolver(svc)
	}
}
