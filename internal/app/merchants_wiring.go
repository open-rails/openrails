package app

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/destructive"
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
// read-only provider manifest, alongside encrypted managed webhook URLs.
//
// A configured backend that cannot be initialized fails construction in every
// environment; returning an unusable merchant service is not a fallback.
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
		backend, err := merchantsecrets.BuildManifest(ctx, r.Config, r.ManifestSecrets, r.DB.DataPool())
		if err != nil {
			return r.armingFailure(err)
		}
		store, ping = backend.Secrets, backend.Ping
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
	// or#858: the merchant purge answers to the same #836 kill switch and #835
	// per-merchant policy as a mass cancellation. Unwired, the service refuses to
	// purge at all — so this is the wiring that makes the gate real rather than
	// decorative, not a wiring that makes the purge reachable (Service.Delete
	// still has no route and no CLI; see merchants.PurgeInventory).
	svc.WithDestructivePolicy(destructive.New(r.DB))
	r.ArmMerchantsService(svc, store)
	r.MerchantSecretPing = ping
	return nil
}

func (r *Runtime) armingFailure(err error) error {
	return fmt.Errorf("initialize merchant services: %w", err)
}

// ArmMerchantsService installs the merchants service AND wires its store into
// the request-time credential consumers (checkout NMI-sale resolution, vault
// saves) the way the standalone server does — embedded hosts otherwise served
// those from the boot-config plane only, which in MODE 1 (#723) is empty.
func (r *Runtime) ArmMerchantsService(svc *merchants.Service, store merchants.MerchantSecretReader) {
	if r == nil || svc == nil {
		return
	}
	if r.MerchantGroupResolver != nil {
		// or#914: a control plane attached before this service existed left
		// its rename-forwarding seam on the runtime; apply it now.
		svc.WithGroupSlugResolver(r.MerchantGroupResolver).WithGroupIDResolver(r.MerchantGroupCanonicalResolver).WithGroupSearchResolver(r.MerchantGroupSearchResolver)
	}
	r.Merchants = svc
	if r.AlertService != nil {
		r.AlertService.SetMerchantSecretStore(svc.Secrets())
	}
	if r.CheckoutSessionService != nil {
		r.CheckoutSessionService.SetMerchantSecretStore(store)
	}
	if r.CheckoutService != nil {
		r.CheckoutService.SetMerchantSecretStore(store)
		r.CheckoutService.SetPSPSecretResolver(svc)
	}
	if r.RailPaymentMethodService != nil {
		r.RailPaymentMethodService.SetMerchantSecretStore(store)
		r.RailPaymentMethodService.SetPSPSecretResolver(svc)
	}
}
