package hostconfig

// The standalone server's own configuration: what it adds to the engine's
// (internal/config), shared by the packages it composes. The embedded engine
// links none of it.

import (
	"fmt"
	"strings"
)

// MerchantCreationConfig is the hosted policy for user-claimed merchant names.
// Reserved names, the pattern and the admission gate apply to
// Server.ProvisionMerchant with an owner and to renames.
type MerchantCreationConfig struct {
	// ReservedSlugs are reserved in addition to billing.ReservedMerchantSlugs.
	ReservedSlugs []string
	// ReservedEscalationRole names the root-group role whose holders may claim
	// reserved names. Empty: reserved names are never user-claimable.
	ReservedEscalationRole string
	// SlugPattern further restricts claimed names (an unanchored regexp).
	SlugPattern string
	// FreeAllowance is how many merchants a user may own before creating
	// another requires a vaulted payment method (server.Deps.HasVaultedPaymentMethod).
	// Zero admits every creation that passes the name policy.
	FreeAllowance int
}

// ConsoleIssuer is the authorization server the console signs staff in at,
// as an OAuth 2.0 public client (code flow with PKCE).
type ConsoleIssuer struct {
	// URL is the issuer: one of AuthKit's trusted issuers.
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

// ResolveConsoleIssuer checks console: an issuer URL, a client id, and the
// resource its tokens are for (auth.resource.id).
func ResolveConsoleIssuer(console *ConsoleIssuer, resource string) (issuer, name string, err error) {
	issuer = strings.TrimRight(strings.TrimSpace(console.URL), "/")
	switch {
	case issuer == "":
		return "", "", fmt.Errorf("admin console issuer: url is required")
	case strings.TrimSpace(console.ClientID) == "":
		return "", "", fmt.Errorf("admin console issuer %q: client_id is required", issuer)
	case strings.TrimSpace(resource) == "":
		return "", "", fmt.Errorf("admin console issuer %q: declare auth.resource.id, the resource its tokens are for", issuer)
	}
	name = strings.TrimSpace(console.Name)
	if name == "" {
		name = issuer
	}
	return issuer, name, nil
}
