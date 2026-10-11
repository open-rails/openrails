package config

import (
	"context"
	"net/http"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit/userinfo"
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
	// Redis is optional shared storage for rate limits and abuse statistics.
	Redis *redis.Client
	// Vault is a borrowed, authenticated client; Config.Vault still names its
	// mounts. The host owns its renewal; OpenRails never revokes it.
	Vault *vaultapi.Client

	// Email delivers OpenRails' email: billing receipts and alerts. Nil uses
	// Config.SMTP.
	Email EmailSender

	// UserInfo is the host's directory (authkit/userinfo.Lookup, AuthKit's
	// Client.UserInfo()): who each customer is, asked whenever OpenRails emails
	// a customer or shows one, and never copied. Nil keeps the copy SCIM
	// pushes fill instead (RouteGroups.Programmatic); without either,
	// customers get no email.
	UserInfo UserInfo

	// Test seams, refused with Config.TestMode live. StripeTransport and
	// NMITransport replace the provider wires; FXTransport replaces the FX
	// source's, which each process reads its rates from into memory; Clock
	// drives renewal dates, retries and entitlement windows.
	StripeTransport http.RoundTripper
	NMITransport    http.RoundTripper
	FXTransport     http.RoundTripper
	Clock           clockwork.Clock
}

// UserInfo is authkit/userinfo's Lookup: the host's directory, read in
// process.
type UserInfo = userinfo.Lookup

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
