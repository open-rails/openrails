// Package alerting delivers immediate merchant notifications and manages encrypted
// outbound webhook destinations.
package alerting

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Severity routes an alert to channels and colors the notification. warning is
// the low bar (in_app by default); critical (VAMP / dunning spike) fans out.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

func (s Severity) valid() bool { return s == SeverityWarning || s == SeverityCritical }

// ChannelType is a delivery sink kind.
type ChannelType string

const (
	ChannelEmail   ChannelType = "email"
	ChannelWebhook ChannelType = "webhook"
)

// ChannelRef identifies a notification destination. A webhook ref names
// a merchant_webhooks row; email carries no reference.
type ChannelRef struct {
	Type      ChannelType `json:"type"`
	WebhookID *uuid.UUID  `json:"webhook_id,omitempty"`
}

// WebhookFormat shapes the outbound POST body.
type WebhookFormat string

const (
	FormatGeneric WebhookFormat = "generic"
	FormatDiscord WebhookFormat = "discord"
	FormatSlack   WebhookFormat = "slack"
)

func (f WebhookFormat) valid() bool {
	return f == FormatGeneric || f == FormatDiscord || f == FormatSlack
}

// Webhook is the API/domain view of a merchant_webhooks row.
type Webhook struct {
	ID              uuid.UUID `json:"id"`
	MerchantID      uuid.UUID `json:"merchant_id"`
	Name            string    `json:"name"`
	DestinationHost string    `json:"destination_host"`
	secretVersion   int
	Format          WebhookFormat `json:"format"`
	Enabled         bool          `json:"enabled"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// Notification is the API/domain view of a notifications row.
type Notification struct {
	ID        uuid.UUID  `json:"id"`
	Severity  Severity   `json:"severity"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Link      string     `json:"link,omitempty"`
	Data      any        `json:"data,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

// CreateWebhookInput is the POST body for a new webhook sink.
type CreateWebhookInput struct {
	Name    string        `json:"name"`
	URL     string        `json:"url"`
	Format  WebhookFormat `json:"format"`
	Enabled *bool         `json:"enabled"`
}

// RotateWebhookURLInput replaces a destination credential without changing the
// webhook identity. URL is write-only.
type RotateWebhookURLInput struct {
	URL string `json:"url"`
}

// Alert is an immediate operational notification, without a configurable rule.
type Alert struct {
	Title         string    `json:"title"`
	Severity      Severity  `json:"severity"`
	Summary       string    `json:"summary"`
	DashboardLink string    `json:"dashboard_link,omitempty"`
	FiredAt       time.Time `json:"fired_at"`
}

// DeliveryResult records the outcome of one external delivery.
type DeliveryResult struct {
	Channel  string `json:"channel"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
}

// FieldError is one corrective validation failure (LLM-legibility house style:
// name the bad param, why, and the valid set / bounds).
type FieldError struct {
	Param   string   `json:"param"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Valid   []string `json:"valid,omitempty"`
}

// ValidationError aggregates field errors so a bad create/update returns ALL of
// them at once.
type ValidationError struct {
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Errors) == 0 {
		return "invalid webhook"
	}
	parts := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		parts[i] = fe.Message
	}
	return strings.Join(parts, "; ")
}

func (e *ValidationError) add(param, code, msg string, valid ...string) {
	e.Errors = append(e.Errors, FieldError{Param: param, Code: code, Message: msg, Valid: valid})
}

func (e *ValidationError) orNil() *ValidationError {
	if e == nil || len(e.Errors) == 0 {
		return nil
	}
	return e
}

func singleFieldError(param, code, msg string, valid ...string) *ValidationError {
	return &ValidationError{Errors: []FieldError{{Param: param, Code: code, Message: msg, Valid: valid}}}
}

func percentf(ratio float64) string { return fmt.Sprintf("%.2f%%", ratio*100) }
