package routesurface

// ProviderRoutes describes provider-specific public routes mounted for one
// runtime surface.
type ProviderRoutes struct {
	StripePortal bool
	Solana       bool // one-off Solana (config, solana-pay) — buyer signs, needs only a recipient
	// SolanaSigning gates routes where OpenRails itself must sign (recurring
	// enroll, on-chain cancel/tier-change).
	SolanaSigning bool
	Webhooks      bool
}

func AllProviderRoutes() ProviderRoutes {
	return ProviderRoutes{StripePortal: true, Solana: true, SolanaSigning: true, Webhooks: true}
}
