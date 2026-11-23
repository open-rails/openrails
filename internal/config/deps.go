package config

import (
	"context"

	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/openrails/billing"
)

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
