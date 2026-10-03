package config

import (
	"context"
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

	// Authenticate says who is calling. Return ErrUnauthenticated when the
	// request carries no valid credential. Required for any published route
	// group except provider webhooks.
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
