// Package alerting is the #736 metric-threshold alerting engine: in-code rule
// templates that compile to #733 metrics queries, a slow-cadence evaluator that
// edge-triggers on threshold crossings, and multi-channel delivery (in_app bell,
// email, outbound webhook). It reuses the metrics query engine wholesale — no
// new metric plumbing — and pushes to a MERCHANT-operator-facing notification
// store (distinct from the customer notification_queue).
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
	ChannelInApp   ChannelType = "in_app"
	ChannelEmail   ChannelType = "email"
	ChannelWebhook ChannelType = "webhook"
)

// ChannelRef is one entry in a rule's ordered channel set. A webhook ref names
// a merchant_webhooks row; in_app/email carry no ref.
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

// Rule is the API/domain view of an alert_rules row.
type Rule struct {
	ID              uuid.UUID      `json:"id"`
	MerchantID      uuid.UUID      `json:"merchant_id"`
	Name            string         `json:"name"`
	Template        string         `json:"template"`
	Params          map[string]any `json:"params"`
	Severity        Severity       `json:"severity"`
	Channels        []ChannelRef   `json:"channels"`
	Enabled         bool           `json:"enabled"`
	FiredAt         *time.Time     `json:"fired_at,omitempty"`
	ClearedAt       *time.Time     `json:"cleared_at,omitempty"`
	LastEvaluatedAt *time.Time     `json:"last_evaluated_at,omitempty"`
	LastValue       *float64       `json:"last_value,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// Webhook is the API/domain view of a merchant_webhooks row.
type Webhook struct {
	ID         uuid.UUID     `json:"id"`
	MerchantID uuid.UUID     `json:"merchant_id"`
	Name       string        `json:"name"`
	URL        string        `json:"url"`
	Format     WebhookFormat `json:"format"`
	Enabled    bool          `json:"enabled"`
	CreatedAt  time.Time     `json:"created_at"`
	UpdatedAt  time.Time     `json:"updated_at"`
}

// Notification is the API/domain view of a merchant_notifications row.
type Notification struct {
	ID        uuid.UUID  `json:"id"`
	Severity  Severity   `json:"severity"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Link      string     `json:"link,omitempty"`
	RuleID    *uuid.UUID `json:"rule_id,omitempty"`
	Data      any        `json:"data,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at,omitempty"`
}

// CreateRuleInput is the POST body for a new rule.
type CreateRuleInput struct {
	Name     string         `json:"name"`
	Template string         `json:"template"`
	Params   map[string]any `json:"params"`
	Severity Severity       `json:"severity"`
	Channels []ChannelRef   `json:"channels"`
	Enabled  *bool          `json:"enabled"`
}

// UpdateRuleInput is the PATCH body; nil fields are left unchanged.
type UpdateRuleInput struct {
	Name     *string        `json:"name"`
	Params   map[string]any `json:"params"`
	Severity *Severity      `json:"severity"`
	Channels []ChannelRef   `json:"channels"`
	Enabled  *bool          `json:"enabled"`
}

// CreateWebhookInput is the POST body for a new webhook sink.
type CreateWebhookInput struct {
	Name    string        `json:"name"`
	URL     string        `json:"url"`
	Format  WebhookFormat `json:"format"`
	Enabled *bool         `json:"enabled"`
}

// Alert is the payload delivered to every channel: the metric value, threshold,
// offending dims, severity, rule name, and a dashboard deep link.
type Alert struct {
	RuleID        uuid.UUID         `json:"rule_id"`
	RuleName      string            `json:"rule_name"`
	Template      string            `json:"template"`
	Severity      Severity          `json:"severity"`
	Metric        string            `json:"metric"`
	Value         float64           `json:"value"`
	Threshold     float64           `json:"threshold"`
	Unit          string            `json:"unit"`
	Dimensions    map[string]string `json:"dimensions,omitempty"`
	Summary       string            `json:"summary"`
	DashboardLink string            `json:"dashboard_link,omitempty"`
	Test          bool              `json:"test,omitempty"`
	FiredAt       time.Time         `json:"fired_at"`
}

// DeliveryResult records the outcome of one channel delivery (recorded, and
// returned by test-fire).
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
		return "invalid alert rule"
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
