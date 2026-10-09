package config

import (
	"fmt"
	"strings"
)

// Routes selects the HTTP surface Client.Routes returns and the adapters
// mount on the root router. Processor webhooks and GET /v1/capabilities are
// always mounted.
type Routes struct {
	// Prefix is where the API is mounted: "/billing" serves /billing/v1/*.
	// Empty is the root.
	Prefix string
	// Storefront mounts what a buyer's browser needs without signing in:
	// products, prices, currencies, checkout config, checkout sessions (read
	// and pay), Solana Pay and the captcha.
	Storefront bool
	// Customers mounts signed-in customers' own billing at /v1/me/*,
	// authenticated by Deps.AuthKit or Deps.Authenticate. CustomersNone, the
	// zero value, mounts none.
	Customers CustomerHTTPScope
	// Merchant mounts the merchant API (/v1/merchant/*) for staff and
	// machines, each route gated by its merchant permission. It needs
	// Deps.AuthKit with Deps.AuthorityFor, or Deps.Authenticate with
	// Deps.Authorize.
	Merchant bool
	// CatalogEdits adds the merchant API's catalog-write routes; it needs
	// Merchant. The Go client edits the catalog either way. Every mount of the
	// merchant API in one process must agree.
	CatalogEdits bool
	// CustomerProfiles mount further customer surfaces: another prefix,
	// another merchant, or Delegated authentication.
	CustomerProfiles []CustomerRoutes
	// CookieOrigin admits cookie-authenticated requests from this exact origin
	// (https, or http on loopback); unsafe ones must carry it as Origin.
	// Empty strips ambient cookies: credentials are explicit headers.
	CookieOrigin string
	// AdminConsole serves the merchant admin console, the staff dashboard
	// over the merchant API; it needs Merchant. Nil, the default, mounts no
	// console routes at all.
	AdminConsole *AdminConsole
}

// AdminConsole is Routes.AdminConsole. The console is a static app that
// calls the merchant API at Routes.Prefix and AuthKit on its own origin.
type AdminConsole struct {
	// Path is where the console is served: an absolute path without a
	// trailing slash, outside Prefix's API. Empty is "/admin".
	Path string
	// AuthBaseURL is the AuthKit JSON API staff sign in through. Empty is
	// Deps.AuthKit's (its APIBase, "/api/v1" by default) or, with
	// Config.ControlPlane, the control plane's.
	AuthBaseURL string
	// Extensions is the host's data for the console extensions it builds in
	// (scripts/build-admin-console.sh --extensions), keyed by extension id and
	// served verbatim in config.json. OpenRails never reads it; a plain
	// console build leaves it empty.
	Extensions map[string]any
	// Issuer signs staff in at a trusted issuer instead of the control
	// plane's own accounts. It needs Config.ControlPlane's ResourceServer.
	Issuer *ConsoleIssuer
}

// ConsoleIssuer is the authorization server the console signs staff in at,
// as an OAuth 2.0 public client (code flow with PKCE and DPoP).
type ConsoleIssuer struct {
	// URL is the issuer: one of the resource server's trusted issuers.
	URL string
	// ClientID is the console's public client there, registered with the
	// console path plus /callback as a redirect URI.
	ClientID string
	// Name is shown on the sign-in button; empty is the trusted issuer's.
	Name string
	// Scope is what the console asks for; empty is ConsoleScope. An issuer
	// that grants refresh tokens only for offline_access needs it added.
	Scope string
}

// ConsoleScope is what the console asks a trusted issuer for by default.
const ConsoleScope = "openid profile email openrails:merchant"

// ResolveConsoleIssuer checks console against the resource server: its URL
// must be a trusted issuer's, and it needs a client id.
func ResolveConsoleIssuer(console *ConsoleIssuer, rs *ResourceServerConfig) (issuer, name, resource string, err error) {
	if rs == nil {
		return "", "", "", fmt.Errorf("admin console issuer: declare resource_server, which trusts it")
	}
	url := strings.TrimRight(strings.TrimSpace(console.URL), "/")
	for _, is := range rs.TrustedIssuers {
		if strings.TrimRight(strings.TrimSpace(is.Issuer), "/") != url {
			continue
		}
		if strings.TrimSpace(console.ClientID) == "" {
			return "", "", "", fmt.Errorf("admin console issuer %q: client_id is required", url)
		}
		name = strings.TrimSpace(console.Name)
		if name == "" {
			name = strings.TrimSpace(is.Name)
		}
		if name == "" {
			name = url
		}
		return strings.TrimSpace(is.Issuer), name, strings.TrimSpace(rs.Identifier), nil
	}
	return "", "", "", fmt.Errorf("admin console issuer %q is not one of resource_server.trusted_issuers", url)
}

// CheckoutConfig is Config.Checkout: the shared payment page. The zero value
// is the single-site case: the app renders billing-ui's <Checkout> itself
// against the session routes, with no payment page and no frame.
type CheckoutConfig struct {
	// PageURL is where the shared payment page (billing-ui's <CheckoutPage>)
	// is served; a minted session's URL is PageURL#<id>. Every app selling
	// through the page sets it.
	PageURL string
	// EmbedOrigins are the sites (scheme://host[:port]) allowed to frame the
	// page this host serves. Only the payment host sets it.
	EmbedOrigins []string
}

// CustomerHTTPScope selects the routes of a customer surface.
type CustomerHTTPScope uint8

const (
	// CustomersNone mounts no customer routes.
	CustomersNone CustomerHTTPScope = iota
	// CustomerSelfService is the full customer self-service API.
	CustomerSelfService
	// CustomerSubscriptionManagement is cancellation, resumption, subscription
	// payment-method changes and invoice collection-method selection only.
	CustomerSubscriptionManagement
	// CustomerBillingManagement adds billing history, purchased access, saved
	// methods and payment recovery, without checkout or plan purchases.
	CustomerBillingManagement
)

// CustomerRoutes is one further customer surface (Routes.CustomerProfiles).
// It never includes merchant administration, provider callbacks or
// credential management.
type CustomerRoutes struct {
	// Prefix is the surface's base relative to the mount; default /v1/me.
	// Whole-segment {parameters} reach Deps.AuthenticateCustomer through
	// Request.PathValue.
	Prefix string
	// Merchant is the merchant slug native customers buy from; default
	// Config.Merchant.Slug.
	Merchant string
	// Scope selects the surface's routes; it is required.
	Scope CustomerHTTPScope
	// Delegated authenticates this surface with Deps.AuthenticateCustomer,
	// which names an explicit merchant and paying customer, instead of
	// Deps.AuthKit or Deps.Authenticate.
	Delegated bool
}
