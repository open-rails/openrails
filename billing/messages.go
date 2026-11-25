package billing

import (
	"time"

	"github.com/google/uuid"
)

// MessageDataVersion is the version of MessageData that templates and
// Deps.MessageRenderer read. Within a version fields are only added; removing
// or changing one is a new version.
const MessageDataVersion = 1

// MessageData is what a customer message renders from, version 1: who it is
// to and from, the fact it is about and where it sends the customer. Every
// field is text formatted for the customer, empty when the fact or the
// merchant's profile has none.
type MessageData struct {
	// Version is MessageDataVersion.
	Version int
	// EventType is the notification event type the message is about.
	EventType string
	// Language is the language the message is written in (en, fr, pt-BR).
	Language string
	Customer MessageCustomer
	Merchant MessageMerchant
	Event    MessageEvent
	Links    MessageLinks
}

// MessageCustomer is who a message is to.
type MessageCustomer struct {
	// ID is the customer's id.
	ID string
	// Name is the customer's name in the directory, else their username.
	Name string
}

// MessageMerchant is who a message is from: the merchant's profile.
type MessageMerchant struct {
	// Name is the merchant's display name.
	Name                string
	LogoURL             string
	SupportURL          string
	BillingURL          string
	SubscribeURL        string
	TermsURL            string
	PrivacyURL          string
	StatementDescriptor string
}

// MessageLinks are where a message sends the customer, from the merchant's
// profile. Each falls back to its SupportURL.
type MessageLinks struct {
	// ManageBilling is where the customer manages or cancels: BillingURL.
	ManageBilling string
	// UpdatePaymentMethod is where they change their card: BillingURL.
	UpdatePaymentMethod string
	// Resubscribe is where they subscribe again: SubscribeURL, else
	// BillingURL.
	Resubscribe string
	// Pay is where they pay what they owe or confirm a payment: BillingURL.
	Pay string
}

// MessageEvent is the fact a message is about, formatted: amounts with their
// currency at its registered scale ("12.50 USD"), instants as dates in UTC
// ("January 2, 2006"), counts in digits. Each event type fills the fields its
// NotificationData has, of the same names.
type MessageEvent struct {
	// OccurredAt is when the fact happened.
	OccurredAt string
	// Reason qualifies the fact as NotificationData.Reason does. On
	// subscription_canceled and premium_ended: user_cancel (the customer
	// canceled), admin (staff did), expired (payments failed past dunning),
	// non_recoverable (the card can no longer be charged), refund,
	// chargeback, rail_cancel (the provider or a system action), access_ended
	// (access reached its end, no cause recorded) or unknown. On
	// order_collection_stopped: schedule_exhausted or non_recoverable.
	Reason string
	// ProductName is what the fact is about: what an order bought, else the
	// subscription's product.
	ProductName      string
	FromProductName  string
	ToProductName    string
	Amount           string
	OldAmount        string
	NewAmount        string
	AmountDue        string
	OverdueAmount    string
	AvailableAmount  string
	OwedAmount       string
	LowBalanceAmount string
	EndedAt          string
	EffectiveAt      string
	PeriodStartsAt   string
	PeriodEndsAt     string
	DueAt            string
	NextAttemptAt    string
	RunwayEndsAt     string
	OverdueStartedAt string
	OrderNumber      string
	OverdueOrders    string
	Quantity         string
	AccessEnded      bool
	DowngradeApplied bool
	// Voided is a refund_completed that canceled the charge before it
	// settled: the customer was not charged.
	Voided           bool
	RetryStatus      NotificationRetryStatus
	RecoveryAction   NotificationRecoveryAction
	CollectionMethod InvoiceCollectionMethod
	// The ids the fact names, prefixed (ord_, pay_, sub_).
	OrderID           string
	PaymentID         string
	RefundedPaymentID string
	SubscriptionID    string
}

// MessageContent is a rendered message: a subject and a text body, an HTML
// body or both.
type MessageContent struct {
	Subject string
	Text    string
	HTML    string
}

// MessageSettings is a merchant's messages document: which event types send
// the customer a message, and templates that replace the built-in ones. An
// event type it leaves unset keeps its default.
type MessageSettings struct {
	// DefaultLanguage is the language messages are written in, a BCP 47 tag;
	// empty is en.
	DefaultLanguage string `json:"default_language,omitempty"`
	// Events are the settings of each event type, by event type.
	Events map[string]MessageEventSettings `json:"events,omitempty"`
	// Layouts wrap every message's body, by language.
	Layouts map[string]MessageLayout `json:"layouts,omitempty"`
}

// MessageEventSettings are one event type's message settings.
type MessageEventSettings struct {
	// Enabled sends the event type's message; nil keeps its default.
	Enabled *bool `json:"enabled,omitempty"`
	// Templates replace the built-in message, by language.
	Templates map[string]MessageTemplate `json:"templates,omitempty"`
}

// MessageTemplate is one message's templates over MessageData: Subject and
// Text are Go text/template, HTML is html/template. Subject and a body are
// required; a body left empty is not sent.
type MessageTemplate struct {
	// Subject is the subject line's template.
	Subject string `json:"subject"`
	// HTML is the HTML body's template.
	HTML string `json:"html,omitempty"`
	// Text is the plain-text body's template.
	Text string `json:"text,omitempty"`
}

// MessageLayout wraps every message's body: each part runs
// {{template "body" .}} where the body goes, over the same MessageData. A
// part left empty is the built-in one.
type MessageLayout struct {
	// HTML is the HTML layout template.
	HTML string `json:"html,omitempty"`
	// Text is the plain-text layout template.
	Text string `json:"text,omitempty"`
}

// CustomerMessageID names one customer message; on the wire "msg_<uuid>".
type CustomerMessageID uuid.UUID

const customerMessageIDPrefix = "msg_"

func ParseCustomerMessageID(s string) (CustomerMessageID, error) {
	u, err := parsePrefixedID("customer message", customerMessageIDPrefix, s)
	return CustomerMessageID(u), err
}

func (id CustomerMessageID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id CustomerMessageID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id CustomerMessageID) String() string {
	return formatPrefixedID(customerMessageIDPrefix, uuid.UUID(id))
}
func (id CustomerMessageID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *CustomerMessageID) UnmarshalText(text []byte) error {
	parsed, err := ParseCustomerMessageID(string(text))
	*id = parsed
	return err
}

// MessageStatus is where a customer message stands. Pending, queued and
// sending are on the way; the others are where it ended.
type MessageStatus string

const (
	// MessagePending: recorded with its fact; its send job is not queued yet.
	MessagePending MessageStatus = "pending"
	// MessageQueued: its send job is queued, or waits to retry.
	MessageQueued MessageStatus = "queued"
	// MessageSending: its job is handing it to the sender.
	MessageSending MessageStatus = "sending"
	// MessageSent: the sender accepted it.
	MessageSent MessageStatus = "sent"
	// MessageDisabled: the merchant does not send it: its event type is
	// turned off, or off by default for this fact.
	MessageDisabled MessageStatus = "disabled"
	// MessageTestEnvironment: its fact is on a PSP that moves no real money.
	MessageTestEnvironment MessageStatus = "test_environment"
	// MessageNoContact: the customer has no address.
	MessageNoContact MessageStatus = "no_contact"
	// MessageRateLimited: the customer was sent the hourly cap already.
	MessageRateLimited MessageStatus = "rate_limited"
	// MessageFailed: it will not be sent; LastError says why.
	MessageFailed MessageStatus = "failed"
)

// MessageError is why a message's last attempt did not send.
type MessageError string

const (
	// MessageSendFailed: the sender failed, or the mail server refused it
	// other than for its recipient; the job retries it, and the last attempt
	// fails it.
	MessageSendFailed MessageError = "send_failed"
	// MessageRefused: the mail server refused the recipient for good (a 5xx
	// to RCPT TO).
	MessageRefused MessageError = "refused"
	// MessageRenderFailed: neither the host's renderer nor a template gave
	// it a subject and a body.
	MessageRenderFailed MessageError = "render_failed"
	// MessageInvalidRecipient: the customer's address is not an address.
	MessageInvalidRecipient MessageError = "invalid_recipient"
	// MessageNoSender: the deployment has no email sender.
	MessageNoSender MessageError = "no_sender"
	// MessageEventMissing: its fact was pruned before it was sent.
	MessageEventMissing MessageError = "event_missing"
	// MessageEventUnreadable: its fact's data does not decode.
	MessageEventUnreadable MessageError = "event_unreadable"
	// MessageUnavailable: the database or the directory failed its last
	// attempt.
	MessageUnavailable MessageError = "unavailable"
)

// MessageChannel is how a message reaches the customer.
type MessageChannel string

// MessageEmail is email through the deployment's sender.
const MessageEmail MessageChannel = "email"

// CustomerMessage is one message to a customer about one billing fact on one
// channel, and what became of it: the delivery log a dispute response cites.
// It keeps no body.
type CustomerMessage struct {
	ID         CustomerMessageID `json:"id"`
	CustomerID CustomerID        `json:"customer_id"`
	// EventID is the fact's id in the event log, kept after the fact is
	// pruned.
	EventID   NotificationID `json:"event_id"`
	EventType string         `json:"event_type"`
	Channel   MessageChannel `json:"channel"`
	Status    MessageStatus  `json:"status"`
	// Attempts is how many times it was handed to the sender.
	Attempts int `json:"attempts"`
	// LastError is why the last attempt did not send; null after a send.
	LastError *MessageError `json:"last_error"`
	// Recipient is the address it was handed to.
	Recipient *string    `json:"recipient"`
	SentAt    *time.Time `json:"sent_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// CustomerMessageListParams pages a customer's message log, newest first.
type CustomerMessageListParams struct {
	PageRequest
}

// PreviewMessageParams renders an event type's message against sample data:
// the merchant's own profile, a sample customer and fact.
type PreviewMessageParams struct {
	// EventType is the notification event type the message is about.
	EventType string `json:"event_type"`
	// Language is the customer's language, a BCP 47 tag; empty is the
	// merchant's default.
	Language string `json:"language"`
	// Template, when set, is a candidate for EventType's template in
	// Language (the default language when empty), rendered in place of the
	// merchant's. It is not saved.
	Template *MessageTemplate `json:"template"`
}

// MessagePreview is a message rendered from the templates.
type MessagePreview struct {
	EventType string `json:"event_type"`
	// Language is the language it is written in: the first of the requested
	// language, the default language and en that has a template.
	Language string `json:"language"`
	Subject  string `json:"subject"`
	// Text and HTML are the bodies; null is one the template has none of.
	Text *string `json:"text"`
	HTML *string `json:"html"`
}

// CreateTestMessageParams sends a sample of an event type's message to the
// merchant's alert email address, as PreviewMessageParams renders it.
type CreateTestMessageParams struct {
	EventType string           `json:"event_type"`
	Language  string           `json:"language"`
	Template  *MessageTemplate `json:"template"`
}

// TestMessage is a sample message the sender accepted.
type TestMessage struct {
	// Recipient is the merchant's alert email address, the only address a
	// test is sent to.
	Recipient string         `json:"recipient"`
	Message   MessagePreview `json:"message"`
}
