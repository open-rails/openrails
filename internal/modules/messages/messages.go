// Package messages sends customers their billing messages. Each billing fact
// (a notification event) queues one message per channel in the transaction
// that records it; a dispatcher queues each message's send job, and the job
// decides whether the message goes out, renders it and hands it to the
// deployment's email sender. The log keeps no body.
package messages

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/notificationinbox"
)

const (
	// RateCap is how many messages one customer is sent per RateWindow;
	// more are rate_limited.
	RateCap    = 10
	RateWindow = time.Hour
	// TestCap is how many test messages a merchant sends per TestWindow.
	TestCap    = 10
	TestWindow = time.Hour
)

// Where a message stands (customer_messages.status).
const (
	statusQueued          = string(billing.MessageQueued)
	statusSending         = string(billing.MessageSending)
	statusSent            = string(billing.MessageSent)
	statusDisabled        = string(billing.MessageDisabled)
	statusTestEnvironment = string(billing.MessageTestEnvironment)
	statusNoContact       = string(billing.MessageNoContact)
	statusRateLimited     = string(billing.MessageRateLimited)
	statusFailed          = string(billing.MessageFailed)
)

// Why an attempt did not send (customer_messages.last_error_code).
const (
	errSendFailed       = string(billing.MessageSendFailed)
	errRefused          = string(billing.MessageRefused)
	errRenderFailed     = string(billing.MessageRenderFailed)
	errInvalidRecipient = string(billing.MessageInvalidRecipient)
	errNoSender         = string(billing.MessageNoSender)
	errEventMissing     = string(billing.MessageEventMissing)
	errEventUnreadable  = string(billing.MessageEventUnreadable)
	errUnavailable      = string(billing.MessageUnavailable)
)

// defaults is whether each event type sends an email until the merchant
// says otherwise; every type listed has a built-in template. A type not
// listed sends none unless the merchant turns it on with a template.
// byDefault refines some of them by the fact.
var defaults = map[models.NotificationEventType]bool{
	models.NotificationOneOffPurchaseCompleted:       true,
	models.NotificationRefundCompleted:               true,
	models.NotificationPremiumStarted:                true,
	models.NotificationPaymentMethodFailed:           true,
	models.NotificationPaymentAuthenticationRequired: true,
	models.NotificationPaymentMethodUpdateRequired:   true,
	models.NotificationSubscriptionCanceled:          true,
	models.NotificationInvoiceCollectionStopped:      true,
	models.NotificationSubscriptionRepriceScheduled:  true,

	models.NotificationPremiumRenewed:                  false,
	models.NotificationPremiumEnded:                    false,
	models.NotificationPaymentMethodAutoUpdated:        false,
	models.NotificationAccountDelinquent:               false,
	models.NotificationAccountDelinquencyClosed:        false,
	models.NotificationUsageCharged:                    false,
	models.NotificationBalanceLow:                      false,
	models.NotificationBalanceDepleted:                 false,
	models.NotificationInvoiceDue:                      false,
	models.NotificationInvoiceOverdue:                  false,
	models.NotificationSubscriptionPlanChangeScheduled: false,
	models.NotificationSubscriptionChanged:             false,
	models.NotificationAgreementRevoked:                false,
}

// Known reports whether eventType is a notification event type a message
// can be about.
func Known(eventType string) bool {
	_, ok := defaults[models.NotificationEventType(eventType)]
	return ok
}

// EventTypes are the event types messages can be about, in order.
func EventTypes() []string {
	out := make([]string, 0, len(defaults))
	for t := range defaults {
		out = append(out, string(t))
	}
	slices.Sort(out)
	return out
}

// confirmedEndings are the reasons whose premium_ended confirms a
// cancellation: the customer's, failed payments' or staff's. An ending
// caused by a refund, a chargeback, the provider or the end of the paid term
// sends none by default.
var confirmedEndings = []subscriptions.PremiumEndReason{
	subscriptions.PremiumEndReasonUserCancel, subscriptions.PremiumEndReasonAdmin,
	subscriptions.PremiumEndReasonExpired, subscriptions.PremiumEndReasonNonRecoverable,
}

// checkoutSources are the payment issues a customer meets in checkout,
// where the page told them already; as the account inbox, they send none.
var checkoutSources = []string{"customer_checkout", "customer_subscription_change"}

// byDefault is whether a fact of eventType with data sends its message when
// the merchant does not say.
func byDefault(eventType string, data billing.NotificationData) bool {
	kind := models.NotificationEventType(eventType)
	switch {
	case kind == models.NotificationPremiumEnded:
		return slices.Contains(confirmedEndings, subscriptions.PremiumEndReason(data.Reason))
	case notificationinbox.IsPaymentIssue(eventType) && slices.Contains(checkoutSources, data.Source):
		return false
	case kind == models.NotificationPaymentMethodFailed && data.RecoveryAction == billing.NotificationRecoveryResubscribe:
		// The failure that ended a subscription: its ending confirms it.
		return false
	}
	return defaults[kind]
}

// Enabled reports whether s sends the message of a fact of eventType with
// data: the merchant's enabled, else the default.
func Enabled(s billing.MessageSettings, eventType string, data billing.NotificationData) bool {
	if e, ok := s.Events[eventType]; ok && e.Enabled != nil {
		return *e.Enabled
	}
	return byDefault(eventType, data)
}

// Service dispatches and sends customer messages.
type Service struct {
	DB *db.DB
	// Email is the deployment's sender; nil sends nothing.
	Email    config.EmailSender
	Contacts identity.Directory
	Profiles *merchantconfig.Store
	// Renderer is the host's (Deps.MessageRenderer); nil renders every
	// message from the templates.
	Renderer func(context.Context, billing.MessageData) (billing.MessageContent, bool, error)
	// Abuse counts test messages.
	Abuse *abusestate.Store
	Clock clockwork.Clock
	Log   *slog.Logger
}
