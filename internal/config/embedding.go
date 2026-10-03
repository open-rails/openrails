package config

import (
	"net/http"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

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

// HostOwned reports whether the host runs the fleet.
func (o RiverOwnership) HostOwned() bool { return o == RiverHostOwned }

// HTTPConfig selects the HTTP surface Client.Routes publishes. Provider
// webhooks and capability discovery are always included; every other group is
// opt-in and independent of in-process Client access.
type HTTPConfig struct {
	// CustomerRoutes publishes customer self-service profiles (/v1/me/*).
	CustomerRoutes []CustomerRoutesConfig
	// Checkout publishes products, prices, checkout sessions and checkout config.
	Checkout bool
	// MerchantAdmin, Catalog, MerchantConfig and MerchantAPI publish the staff
	// and machine surfaces; they require Deps.Authenticate and Deps.Authorize.
	MerchantAdmin  bool
	Catalog        bool
	MerchantConfig bool
	MerchantAPI    bool
	// CookieOrigin admits cookie-authenticated requests from this exact origin
	// (https, or http on loopback); unsafe ones must carry it as Origin.
	// Empty strips ambient cookies: credentials are explicit headers.
	CookieOrigin string
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
	// Whole-segment {parameters} reach Authenticate through Request.PathValue.
	Prefix string
	// Merchant is the merchant slug native customers buy from; default
	// Config.Merchant.Slug.
	Merchant string
	// Treasury adds the separately permission-gated /v1/customers group.
	Treasury bool
	Scope    CustomerHTTPScope
	// Authenticate maps a request to an explicit merchant and paying subject
	// for this profile instead of Deps.Authenticate.
	Authenticate func(*http.Request) (*billingauth.DelegatedPrincipal, error)
}

// ProviderCredentialSnapshot supplies immutable host-owned credentials for an
// existing PSP without changing its metadata or arming it. The environment
// follows Config.TestMode. Values are never persisted.
type ProviderCredentialSnapshot struct {
	MerchantID  merchant.ID
	Rail        string
	AccountID   string
	Credentials map[string]string
}
