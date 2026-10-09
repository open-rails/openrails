package config

import (
	"context"
	"io/fs"
	"net"
	"net/http"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/helpers/contacts"
	"github.com/redis/go-redis/v9"
)

// Deps is everything an embedded engine reaches outside its own process:
// connections, credentials and test seams. Config is plain data. Auth is not
// here: it is supplied where routes are mounted (Routes.Auth); the engine
// itself authenticates nobody.
type Deps struct {
	// Postgres is the host's pool; its role owns OpenRails' tables, which New
	// creates or upgrades. Nil opens one from Config.DB.
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

	// Email delivers OpenRails' email: billing receipts and alerts. Nil uses
	// Config.SendGrid.
	Email EmailSender

	// Contacts is the host's directory (helpers/contacts.Source, which
	// *authkit.Client implements): who each customer is, asked whenever
	// OpenRails emails a customer or shows one, and never copied. Nil keeps
	// the copy Routes.Provisioning's SCIM pushes fill instead; without either,
	// customers get no email.
	Contacts Contacts

	// Test seams, refused with Config.TestMode live. StripeTransport and
	// NMITransport replace the provider wires; DNSResolver answers api_host
	// proofs; Clock drives renewal dates, retries and entitlement windows.
	StripeTransport http.RoundTripper
	NMITransport    http.RoundTripper
	DNSResolver     *net.Resolver
	Clock           clockwork.Clock
}

// Contacts is helpers/contacts' Source: the host's directory, read in
// process.
type Contacts = contacts.Source

// Contact is how to reach a person, as Contacts answers it.
type Contact = contacts.Contact

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
}

// EmailAddress is a mailbox and its display name.
type EmailAddress struct {
	Name    string
	Address string
}
