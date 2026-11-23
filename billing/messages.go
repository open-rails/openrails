package billing

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
	// Reason qualifies the fact as NotificationData.Reason does: on
	// subscription_canceled and premium_ended, user_cancel (the customer
	// canceled), admin (staff did), expired (payments failed) and others; on
	// order_collection_stopped, schedule_exhausted or non_recoverable.
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
	Subject string `json:"subject"`
	HTML    string `json:"html,omitempty"`
	Text    string `json:"text,omitempty"`
}

// MessageLayout wraps every message's body: each part runs
// {{template "body" .}} where the body goes, over the same MessageData. A
// part left empty is the built-in one.
type MessageLayout struct {
	HTML string `json:"html,omitempty"`
	Text string `json:"text,omitempty"`
}
