// Package messages sends customers their billing messages. Each billing fact
// (a notification event) queues one message per channel in the transaction
// that records it; a dispatcher queues each message's send job, and the job
// decides whether the message goes out, renders it and hands it to the
// deployment's email sender. The log keeps no body.
package messages

import (
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
)

const (
	// RateCap is how many messages one customer is sent per RateWindow;
	// more are rate_limited.
	RateCap    = 10
	RateWindow = time.Hour
)

// Where a message stands (customer_messages.status).
const (
	statusQueued          = "queued"
	statusSending         = "sending"
	statusSent            = "sent"
	statusDisabled        = "disabled"
	statusTestEnvironment = "test_environment"
	statusNoContact       = "no_contact"
	statusRateLimited     = "rate_limited"
	statusFailed          = "failed"
)

// Why an attempt did not send (customer_messages.last_error_code).
const (
	errSendFailed       = "send_failed"
	errRefused          = "refused"
	errRenderFailed     = "render_failed"
	errInvalidRecipient = "invalid_recipient"
	errNoSender         = "no_sender"
	errEventMissing     = "event_missing"
	errEventUnreadable  = "event_unreadable"
	errUnavailable      = "unavailable"
)

// defaults is whether each event type sends an email until the merchant
// says otherwise. A type not listed sends none.
var defaults = map[models.NotificationEventType]bool{
	models.NotificationOneOffPurchaseCompleted:       true,
	models.NotificationPremiumStarted:                true,
	models.NotificationPaymentMethodFailed:           true,
	models.NotificationPaymentAuthenticationRequired: true,
	models.NotificationPaymentMethodUpdateRequired:   true,
	models.NotificationSubscriptionCanceled:          true,
	models.NotificationOrderCollectionStopped:        true,
	models.NotificationSubscriptionRepriceScheduled:  true,

	models.NotificationPremiumRenewed:                  false,
	models.NotificationPremiumEnded:                    false,
	models.NotificationPaymentMethodAutoUpdated:        false,
	models.NotificationAccountDelinquent:               false,
	models.NotificationAccountDelinquencyClosed:        false,
	models.NotificationUsageCharged:                    false,
	models.NotificationBalanceLow:                      false,
	models.NotificationBalanceDepleted:                 false,
	models.NotificationOrderDue:                        false,
	models.NotificationOrderOverdue:                    false,
	models.NotificationSubscriptionPlanChangeScheduled: false,
	models.NotificationSubscriptionChanged:             false,
	models.NotificationAgreementRevoked:                false,
}

// Enabled reports whether eventType sends an email.
func Enabled(eventType string) bool { return defaults[models.NotificationEventType(eventType)] }

// Service dispatches and sends customer messages.
type Service struct {
	DB *db.DB
	// Email is the deployment's sender; nil sends nothing.
	Email    config.EmailSender
	Contacts identity.Directory
	Profiles *merchantconfig.Store
	Renderer Renderer
	Clock    clockwork.Clock
}
