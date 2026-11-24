// Package alerting delivers immediate merchant notifications and manages the
// merchant's alert webhooks.
package alerting

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/modules/alerting/alertjob"
)

// Severity routes an alert to channels and colors the notification. warning is
// the low bar (in_app by default); critical (VAMP / dunning spike) fans out.
type Severity string

const (
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// ChannelType is a delivery sink kind; ChannelRef names one destination.
type (
	ChannelType = alertjob.ChannelType
	ChannelRef  = alertjob.ChannelRef
)

const (
	ChannelEmail   = alertjob.ChannelEmail
	ChannelWebhook = alertjob.ChannelWebhook
)

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

// Webhook is one of the merchant document's alert webhooks.
type Webhook struct {
	ID              uuid.UUID
	MerchantID      uuid.UUID
	Name            *string
	DestinationHost string
	url             string
	Format          WebhookFormat
	Enabled         bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// API is the webhook as the admin API answers it.
func (w Webhook) API() billing.AlertWebhook {
	return billing.AlertWebhook{
		ID: billing.AlertWebhookID(w.ID), Name: w.Name, DestinationHost: w.DestinationHost,
		Format: billing.AlertWebhookFormat(w.Format), Enabled: w.Enabled, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

// Alert is an immediate operational notification, without a configurable rule.
type Alert struct {
	Title         string    `json:"title"`
	Severity      Severity  `json:"severity"`
	Summary       string    `json:"summary"`
	DashboardLink string    `json:"dashboard_link,omitempty"`
	FiredAt       time.Time `json:"fired_at"`
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
