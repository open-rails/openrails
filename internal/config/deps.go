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
)

// Deps is everything an embedded engine reaches outside its own process:
// connections, credentials and test seams. Config is plain data. Auth is not
// here: it is supplied where routes are mounted (Routes.Auth); the engine
// itself authenticates nobody.
type Deps struct {
	// Postgres is the host's pool; its role owns OpenRails' tables (Migrate).
	// Nil opens one from Config.DB.
	Postgres *pgxpool.Pool
	// Redis is optional shared storage for rate limits, FX rates and abuse statistics.
	Redis *redis.Client
	// Vault is a borrowed, authenticated client for Config.SecretBackend
	// vault. The host owns its renewal; OpenRails never revokes it.
	Vault *vaultapi.Client

	// ConsoleAssets is a host-built admin console (web/admin's Vite build,
	// rooted at index.html). Nil uses the build embedded in this module, when
	// the binary was built with one.
	ConsoleAssets fs.FS

	// Email delivers OpenRails' email: billing receipts and alerts, and the
	// control plane's AuthKit messages. Nil uses Config.SendGrid.
	Email EmailSender
	// SMS delivers the control plane's AuthKit text messages.
	SMS SMSSender
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

// EmailSender delivers rendered email and reports whether it can.
type EmailSender interface {
	Send(context.Context, Email) error
	CheckHealth(context.Context) error
}

// Email is one rendered message.
type Email struct {
	// From is empty for the deployment's own mail; the sender then uses its
	// own address. A field left empty is the sender's.
	From    EmailAddress
	To      string
	Subject string
	Text    string
	HTML    string
	// Auth is the AuthKit message a control-plane email renders, for a sender
	// with its own templates; nil for billing email.
	Auth *iam.EmailMessage
}

// EmailAddress is a mailbox and its display name.
type EmailAddress struct {
	Name    string
	Address string
}

// SMSSender delivers AuthKit text messages.
type SMSSender interface {
	Send(context.Context, iam.SMSMessage) error
	CheckHealth(context.Context) error
}
