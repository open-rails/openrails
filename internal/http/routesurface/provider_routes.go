package routesurface

// ProviderRoutes describes provider-specific public routes mounted for one
// runtime surface. Generic billing routes are controlled by RouteSet.
type ProviderRoutes struct {
	StripePortal bool
	Solana       bool // one-off Solana (config, solana-pay) — buyer signs, needs only a recipient
	// SolanaSigning gates routes where OpenRails itself must sign (recurring
	// enroll, on-chain cancel/tier-change). Requires a Vault connection or a local
	// key (#661); false drops those routes with a boot warning.
	SolanaSigning bool
	Webhooks      bool
	// SecretWrite gates the provider-config WRITE surface (PSP PUT,
	// secret push). False hides it (a write route with no write capability only 403s).
	SecretWrite bool
}

// RuntimeCapabilities is the advisory, boot-probed view of what OpenRails can
// actually do (#661) — never authorization, only feature-gating.
type RuntimeCapabilities struct {
	SolanaCanSign bool // a Vault connection OR a local Solana key is available
	SecretWrite   bool // merchant-secret writes / config-push are possible
}

func AllProviderRoutes() ProviderRoutes {
	return ProviderRoutes{StripePortal: true, Solana: true, SolanaSigning: true, Webhooks: true, SecretWrite: true}
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
