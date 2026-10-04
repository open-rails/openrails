package config

import "github.com/open-rails/openrails/billing"

// RiverOwnership says who runs OpenRails' River job fleet. The fleet is where
// money moves (renewals, dunning, invoices, provider intents), so it always
// runs somewhere.
type RiverOwnership string

const (
	// RiverManaged: OpenRails builds its own River client in RiverSchema and
	// Client.Start runs it. The zero value.
	RiverManaged RiverOwnership = "managed"
	// RiverHostOwned: the host composes Client.RiverJobs into its one fleet
	// (riverhelpers.New) and starts that fleet itself.
	RiverHostOwned RiverOwnership = "host"
)

// HTTPConfig selects the HTTP surface Client.Routes publishes. Provider
// webhooks and capability discovery are always included; every other group is
// opt-in and independent of in-process Client access.
type HTTPConfig struct {
	// CustomerRoutes publishes customer self-service profiles (/v1/me/*).
	CustomerRoutes []CustomerRoutesConfig
	// Checkout publishes products, prices, checkout config and the checkout
	// session read and pay routes (the mint route needs a CustomerSelfService
	// profile); nil publishes none.
	Checkout *CheckoutConfig
	// Merchant publishes the merchant API (/v1/merchant/*, /v1/import/* and
	// creator-owned /v1/catalog/*), each route gated by its merchant
	// permission; it requires Deps.Authenticate and Deps.Authorize.
	Merchant bool
	// CookieOrigin admits cookie-authenticated requests from this exact origin
	// (https, or http on loopback); unsafe ones must carry it as Origin.
	// Empty strips ambient cookies: credentials are explicit headers.
	CookieOrigin string
}

// CheckoutConfig configures checkout sessions. The zero value is the single-site
// case: the app renders billing-ui's <Checkout> itself against the session
// routes, with no payment page and no frame.
type CheckoutConfig struct {
	// PageURL is where the shared payment page (billing-ui's <CheckoutPage>)
	// is served; a minted session's URL is PageURL#<id>. Every app selling
	// through the page sets it.
	PageURL string
	// EmbedOrigins are the sites (scheme://host[:port]) allowed to frame the
	// page this host serves. Only the payment host sets it.
	EmbedOrigins []string
}

// CustomerHTTPScope selects a customer route profile.
type CustomerHTTPScope uint8

const (
	// CustomerSelfService is the full customer self-service API.
	CustomerSelfService CustomerHTTPScope = iota
	// CustomerSubscriptionManagement is cancellation, resumption, subscription
	// payment-method changes and invoice collection-method selection only.
	CustomerSubscriptionManagement
	// CustomerBillingManagement adds billing history, purchased access, saved
	// methods and payment recovery, without checkout or plan purchases.
	CustomerBillingManagement
)

// CustomerRoutesConfig publishes one customer profile. It never includes
// merchant administration, provider callbacks or credential management.
type CustomerRoutesConfig struct {
	// Prefix is the profile's base relative to the mount; default /v1/me.
	// Whole-segment {parameters} reach Deps.AuthenticateCustomer through
	// Request.PathValue.
	Prefix string
	// Merchant is the merchant slug native customers buy from; default
	// Config.Merchant.Slug.
	Merchant string
	// Scope selects the profile's routes. Zero is CustomerSelfService.
	Scope CustomerHTTPScope
	// Delegated authenticates this profile with Deps.AuthenticateCustomer,
	// which names an explicit merchant and paying customer, instead of
	// Deps.Authenticate.
	Delegated bool
}

// ProviderCredentialSnapshot supplies immutable host-owned credentials for an
// existing PSP without changing its metadata or arming it. The environment
// follows Config.TestMode. Values are never persisted.
type ProviderCredentialSnapshot struct {
	MerchantID  billing.MerchantID
	Rail        string
	AccountID   string
	Credentials map[string]string
}
