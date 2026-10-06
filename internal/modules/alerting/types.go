// Package alerting delivers immediate merchant notifications and manages encrypted
// outbound webhook destinations.
package alerting

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
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

// Webhook is a merchant_webhooks row with its credential version.
type Webhook struct {
	ID              uuid.UUID
	MerchantID      uuid.UUID
	Name            *string
	DestinationHost string
	secretVersion   int
	Format          WebhookFormat
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// API is the webhook as the merchant API answers it.
func (w Webhook) API() billing.AlertWebhook {
	return billing.AlertWebhook{
		ID: billing.AlertWebhookID(w.ID), Name: w.Name, DestinationHost: w.DestinationHost,
		Format: billing.AlertWebhookFormat(w.Format), Enabled: w.Enabled, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

// Notification is a merchant inbox entry being written. A non-nil ID makes
// the write idempotent.
type Notification struct {
	ID       uuid.UUID
	Severity Severity
	Title    string
	Body     string
	Link     string
	Data     any
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
