package config

import (
	"context"
	"net/http"

	vaultapi "github.com/hashicorp/vault/api"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/authkit/userinfo"
	"github.com/open-rails/openrails/billing"
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
	// MerchantGroups reads live ownership metadata for explicit bound merchant
	// declarations. It authenticates no request and is never closed by the engine.
	// Required only when a declaration supplies Binding.
	MerchantGroups billing.MerchantGroupReader
	// Redis is optional shared storage for rate limits and abuse statistics:
	// a single-node, failover (Sentinel) or cluster client. The host owns it;
	// OpenRails never closes it.
	Redis redis.UniversalClient
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

	// Notifications is the optional account-inbox channel. It records notices
	// or durable remote delivery in the billing transaction. Nil disables this
	// channel only; retained billing events are still recorded.
	Notifications *NotificationChannel

	// Test seams, refused beside a live PSP. StripeTransport and
	// NMITransport replace the provider wires; FXTransport replaces the FX
	// source's, which each process reads its rates from into memory; Clock
	// drives renewal dates, retries and entitlement windows.
	StripeTransport http.RoundTripper
	// BasisTheoryTransport substitutes the Basis Theory wire; test custody only.
	BasisTheoryTransport http.RoundTripper
	NMITransport         http.RoundTripper
	FXTransport          http.RoundTripper
	Clock                clockwork.Clock
}

// NotificationRecipient is a source identity, not proof that the account may
// receive a notice. The host maps the exact issuer and customer UUID.
type NotificationRecipient struct {
	MerchantID billing.MerchantID
	CustomerID billing.CustomerID
	Issuer     string
}

// NotificationChannel connects billing to an account inbox. Recipient returns
// false for identities intentionally outside the destination's namespace. An
// error leaves the billing transaction retryable. The sink must not perform
// network delivery inside ApplyTx: a remote implementation enqueues locally.
type NotificationChannel struct {
	Sink      notify.Publisher
	Recipient func(context.Context, NotificationRecipient) (accountID string, deliver bool, err error)
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
	Name    string `yaml:"name" json:"name"`
	Address string `yaml:"address" json:"address"`
}
