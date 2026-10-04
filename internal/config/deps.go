package config

import (
	"context"
	"io/fs"
	"net"
	"net/http"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit/iam"
	"github.com/redis/go-redis/v9"

	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/cache"
)

// Deps is everything an embedded engine reaches outside its own process:
// connections, credentials and the host's hooks. Config is plain data.
type Deps struct {
	// Postgres is the host's pool; its role owns OpenRails' tables (Migrate).
	// Nil opens one from Config.DB.
	Postgres *pgxpool.Pool
	// Redis backs rate limits and the shared cache; nil keeps them in memory.
	Redis *redis.Client
	// Cache replaces the Redis-backed cache.
	Cache cache.Cache
	// Vault is a borrowed, authenticated client for Config.SecretBackend
	// vault. The host owns its renewal; OpenRails never revokes it.
	Vault *vaultapi.Client
	// ProviderCredentials are snapshot credentials for existing PSPs.
	ProviderCredentials []ProviderCredentialSnapshot

	// AuthKit authenticates callers with the host's AuthKit: its
	// *authkit.Client (or an authkit Verifier for the host's audiences).
	// OpenRails derives authentication, live authorization and the recent
	// sign-in check from it. Mutually exclusive with Authenticate.
	AuthKit billingauth.Verifier
	// CustomerFor maps an AuthKit caller to the customer who pays (a canonical
	// UUID; "" for none). Default: a user pays for themselves, so the customer
	// is the AuthKit user ID.
	CustomerFor func(ctx context.Context, caller billingauth.Identity) (customerID string, err error)
	// AuthorityFor names the AuthKit group and permission that authorize a
	// staff operation. Required, with AuthKit, for the staff and machine route
	// groups: only the host knows which group holds its billing staff.
	AuthorityFor func(ctx context.Context, required billingauth.Requirement) (billingauth.Authority, error)

	// Authenticate, Authorize and RecentSignIn are for hosts with other auth.
	// Authenticate says who is calling; return ErrUnauthenticated when the
	// request carries no valid credential. Required (or AuthKit) for any
	// published route group except provider webhooks.
	Authenticate func(*http.Request) (billingauth.Identity, error)
	// Authorize checks live, for the exact operation and target, that an
	// authenticated identity holds a staff permission. Return ErrForbidden to
	// refuse. Required for the staff and machine route groups.
	Authorize func(*http.Request, billingauth.Identity, billingauth.Requirement) error
	// RecentSignIn reports whether the request's user signed in recently
	// enough to move money or grant access. Nil refuses those operations to
	// native users.
	RecentSignIn func(*http.Request) error

	// UserExists and UserEmail let OpenRails address billing notices; nil
	// sends none.
	UserExists func(ctx context.Context, userID string) (bool, error)
	UserEmail  func(ctx context.Context, userID string) (username, email string, ok bool, err error)
	// ResolveUsername maps a provider-supplied username to a user ID (the
	// CCBill username bridge).
	ResolveUsername func(ctx context.Context, username string) (userID string, err error)

	// ConsoleAssets is a host-built admin console (web/admin's Vite build,
	// rooted at index.html). Nil uses the build embedded in this module, when
	// the binary was built with one.
	ConsoleAssets fs.FS

	// EmailSender and SMSSender deliver the control plane's AuthKit messages.
	EmailSender EmailSender
	SMSSender   SMSSender
	// HasVaultedPaymentMethod answers whether a user has a payment method on
	// file, unlocking merchant creation beyond the free allowance.
	HasVaultedPaymentMethod func(ctx context.Context, userID string) (bool, error)

	// Test seams, refused with Config.TestMode live. StripeTransport and
	// NMITransport replace the provider wires; DNSResolver answers api_host
	// proofs; Clock drives renewal dates, retries and entitlement windows.
	StripeTransport http.RoundTripper
	NMITransport    http.RoundTripper
	DNSResolver     *net.Resolver
	Clock           clockwork.Clock
}

// EmailSender delivers AuthKit email (AuthKit's adapters satisfy it).
type EmailSender interface {
	Send(context.Context, iam.EmailMessage) error
	CheckHealth(context.Context) error
}

// SMSSender delivers AuthKit text messages.
type SMSSender interface {
	Send(context.Context, iam.SMSMessage) error
	CheckHealth(context.Context) error
}
