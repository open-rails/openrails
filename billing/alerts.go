package billing

import (
	"time"

	"github.com/google/uuid"
)

// AlertWebhookID names an alert webhook; on the wire "awh_<uuid>".
type AlertWebhookID uuid.UUID

// NotificationID names one notification, in the merchant's inbox or a
// customer's; on the wire "ntf_<uuid>".
type NotificationID uuid.UUID

const (
	AlertWebhookIDPrefix = "awh_"
	NotificationIDPrefix = "ntf_"
)

func ParseAlertWebhookID(s string) (AlertWebhookID, error) {
	u, err := parsePrefixedID("alert webhook", AlertWebhookIDPrefix, s)
	return AlertWebhookID(u), err
}

func ParseNotificationID(s string) (NotificationID, error) {
	u, err := parsePrefixedID("notification", NotificationIDPrefix, s)
	return NotificationID(u), err
}

func (id AlertWebhookID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id NotificationID) UUID() uuid.UUID { return uuid.UUID(id) }
func (id AlertWebhookID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id NotificationID) IsZero() bool    { return uuid.UUID(id) == uuid.Nil }
func (id AlertWebhookID) String() string {
	return formatPrefixedID(AlertWebhookIDPrefix, uuid.UUID(id))
}
func (id NotificationID) String() string {
	return formatPrefixedID(NotificationIDPrefix, uuid.UUID(id))
}
func (id AlertWebhookID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id NotificationID) MarshalText() ([]byte, error) {
	return []byte(id.String()), nil
}
func (id *AlertWebhookID) UnmarshalText(b []byte) error {
	v, err := ParseAlertWebhookID(string(b))
	*id = v
	return err
}
func (id *NotificationID) UnmarshalText(b []byte) error {
	v, err := ParseNotificationID(string(b))
	*id = v
	return err
}

// AlertSeverity is how urgent a merchant alert is.
type AlertSeverity string

const (
	AlertSeverityWarning  AlertSeverity = "warning"
	AlertSeverityCritical AlertSeverity = "critical"
)

// AlertWebhookFormat shapes the body OpenRails posts to an alert webhook.
type AlertWebhookFormat string

const (
	AlertWebhookGeneric AlertWebhookFormat = "generic"
	AlertWebhookDiscord AlertWebhookFormat = "discord"
	AlertWebhookSlack   AlertWebhookFormat = "slack"
)

// AlertWebhook is a destination OpenRails posts the merchant's operational
// alerts to. Its URL is a credential: written, never read back.
type AlertWebhook struct {
	ID              AlertWebhookID     `json:"id"`
	Name            string             `json:"name"`
	DestinationHost string             `json:"destination_host"`
	Format          AlertWebhookFormat `json:"format"`
	Enabled         bool               `json:"enabled"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
}

// CreateAlertWebhookParams adds an alert webhook. Format defaults to
// generic; Enabled to true.
type CreateAlertWebhookParams struct {
	Name    string             `json:"name"`
	URL     string             `json:"url"`
	Format  AlertWebhookFormat `json:"format"`
	Enabled *bool              `json:"enabled"`
}

// SetAlertWebhookURLParams replaces a webhook's URL, keeping the webhook.
type SetAlertWebhookURLParams struct {
	URL string `json:"url"`
}

// MerchantNotification is one operational alert in the merchant's inbox.
type MerchantNotification struct {
	ID        NotificationID `json:"id"`
	Severity  AlertSeverity  `json:"severity"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Link      *string        `json:"link"`
	CreatedAt time.Time      `json:"created_at"`
	ReadAt    *time.Time     `json:"read_at"`
}

// MerchantNotificationListParams pages the merchant's inbox, newest first.
type MerchantNotificationListParams struct {
	PageRequest
	// UnreadOnly leaves out read notifications.
	UnreadOnly bool
}

// UnreadCount is how many notifications are unread.
type UnreadCount struct {
	UnreadCount int64 `json:"unread_count"`
}

// WorkerHealth is one background job kind's recent runs. LastError is the
// job's error text, which only the platform operator sees: it can name another
// merchant's records.
type WorkerHealth struct {
	WorkerKind            string     `json:"worker_kind"`
	RegisteredAt          time.Time  `json:"registered_at"`
	ExpectedPeriodSeconds *int64     `json:"expected_period_seconds"`
	LastSuccessAt         *time.Time `json:"last_success_at"`
	LastErrorAt           *time.Time `json:"last_error_at"`
	LastError             *string    `json:"last_error"`
	ConsecutiveFailures   int32      `json:"consecutive_failures"`
	LastAlertedAt         *time.Time `json:"last_alerted_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}
