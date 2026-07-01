package routesurface

import (
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
)

// ProviderRoutes describes provider-specific public routes mounted for one
// runtime surface. Generic billing routes are controlled by RouteSet.
type ProviderRoutes struct {
	StripePortal bool
	Solana       bool // one-off Solana (config, solana-pay) — buyer signs, needs only a recipient
	// SolanaSigning gates routes where OpenRails itself must sign (recurring
	// enroll, on-chain cancel/tier-change). Requires Vault Transit or a local key
	// (#661); false drops those routes with a boot warning.
	SolanaSigning bool
	Webhooks      bool
	// SecretWrite gates the provider-config WRITE surface (provider-account PUT,
	// secret push). False hides it (a write route with no write capability only 403s).
	SecretWrite bool
}

// RuntimeCapabilities is the advisory, boot-probed view of what OpenRails can
// actually do (#661) — never authorization, only feature-gating.
type RuntimeCapabilities struct {
	SolanaCanSign bool // Vault Transit OR a local Solana key is available
	SecretWrite   bool // merchant-secret writes / config-push are possible
}

func AllProviderRoutes() ProviderRoutes {
	return ProviderRoutes{StripePortal: true, Solana: true, SolanaSigning: true, Webhooks: true, SecretWrite: true}
}

// ProviderRoutesFromRails derives the surface from configured rails WITHOUT
// capability gating — signing + write are left enabled (back-compat for callers
// that have no probed capabilities). Prefer ProviderRoutesFromRailsWithCapabilities.
func ProviderRoutesFromRails(rails config.ProviderAccountSet) ProviderRoutes {
	r := providerRoutesFromRails(rails)
	r.SolanaSigning = r.Solana
	r.SecretWrite = true
	return r
}

// ProviderRoutesFromRailsWithCapabilities derives the surface from configured
// rails AND probed capabilities: Solana-signing routes and the secret-write
// surface only mount when OpenRails can actually perform them (#661).
func ProviderRoutesFromRailsWithCapabilities(rails config.ProviderAccountSet, caps RuntimeCapabilities) ProviderRoutes {
	r := providerRoutesFromRails(rails)
	r.SolanaSigning = r.Solana && caps.SolanaCanSign
	r.SecretWrite = caps.SecretWrite
	return r
}

func providerRoutesFromRails(rails config.ProviderAccountSet) ProviderRoutes {
	return ProviderRoutes{
		StripePortal: rails.GetStripeRail() != nil,
		Solana:       rails.GetSolanaRail() != nil,
		Webhooks: len(rails.ActiveRailKeysByType(models.RailStripe)) > 0 ||
			len(rails.ActiveRailKeysByType(models.RailNMI)) > 0 ||
			len(rails.ActiveRailKeysByType(models.RailCCBill)) > 0,
	}
}

func (r ProviderRoutes) Map() map[string]bool {
	return map[string]bool{
		"billing_portal": r.StripePortal,
		"solana":         r.Solana,
		"solana_signing": r.SolanaSigning,
		"webhooks":       r.Webhooks,
		"secret_write":   r.SecretWrite,
	}
}
