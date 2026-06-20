package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/modules/payments/processors"
	"github.com/open-rails/openrails/internal/shared/sigverify"
)

// WebhookEventType is the normalized, processor-agnostic classification of an
// incoming webhook. Each processor handler maps its native event type onto one
// of these so the dispatcher, deduplication, and ledger/subscription logic can
// reason about events without a per-processor switch.
type WebhookEventType string

const (
	WebhookEventUnknown                   WebhookEventType = "unknown"
	WebhookEventPaymentSucceeded          WebhookEventType = "payment_succeeded"
	WebhookEventPaymentFailed             WebhookEventType = "payment_failed"
	WebhookEventSubscriptionCreated       WebhookEventType = "subscription_created"
	WebhookEventSubscriptionRenewed       WebhookEventType = "subscription_renewed"
	WebhookEventSubscriptionRenewalFailed WebhookEventType = "subscription_renewal_failed"
	WebhookEventSubscriptionUpdated       WebhookEventType = "subscription_updated"
	WebhookEventSubscriptionCanceled      WebhookEventType = "subscription_canceled"
	WebhookEventSubscriptionExpired       WebhookEventType = "subscription_expired"
	WebhookEventSubscriptionReactivated   WebhookEventType = "subscription_reactivated"
	WebhookEventRefund                    WebhookEventType = "refund"
	WebhookEventVoid                      WebhookEventType = "void"
	WebhookEventChargeback                WebhookEventType = "chargeback"
	WebhookEventCheckoutCompleted         WebhookEventType = "checkout_completed"
	WebhookEventCheckoutExpired           WebhookEventType = "checkout_expired"
	WebhookEventCustomerUpdated           WebhookEventType = "customer_updated"
)

// WebhookEvent is the unified, processor-agnostic representation of an incoming
// webhook, produced by WebhookHandler.Normalize. Type, RawType, ProcessorRef,
// and Raw are always populated; the remaining fields are best-effort — a handler
// fills them when the native payload makes them cheaply available and leaves
// them zero otherwise.
type WebhookEvent struct {
	Processor       string           // "stripe", "ccbill", or the NMI processor name
	Type            WebhookEventType // normalized classification
	RawType         string           // native processor event type, verbatim
	ProcessorRef    string           // processor's event id / primary reference
	SubscriptionRef string           // processor subscription id (best-effort)
	Amount          int64            // minor units / cents (best-effort, 0 if unknown)
	Currency        string           // ISO currency, upper-cased (best-effort)
	OccurredAt      time.Time        // event time (best-effort, zero if unknown)
	Raw             []byte           // original payload, verbatim
}

// WebhookHandler is the per-processor contract. Adding a new processor's
// webhooks becomes "implement this interface + register it" rather than adding
// a branch to the dispatcher. Verify authenticates the raw message; Normalize
// parses it into a unified WebhookEvent.
type WebhookHandler interface {
	Processor() string
	Verify(msg *WebhookMessage) error
	Normalize(msg *WebhookMessage) (WebhookEvent, error)
	// Apply runs the processor-specific business logic (subscription/ledger
	// updates, dedup, notifications) for the message. The dispatcher resolves the
	// handler by processor and calls Apply, so adding a processor is "implement
	// the interface + register" rather than adding a branch. The apply logic
	// remains processor-specific by necessity — it consumes native payload fields
	// the unified WebhookEvent does not carry, and each processor owns its own
	// deduplication semantics.
	Apply(ctx context.Context, d *WebhookDispatcher, msg *WebhookMessage) error
}

// WebhookHandlerRegistry resolves a WebhookHandler by processor name. NMI-backed
// gateway aliases all resolve to the single registered "nmi" handler.
type WebhookHandlerRegistry struct {
	handlers map[string]WebhookHandler
}

// NewWebhookHandlerRegistry returns an empty registry.
func NewWebhookHandlerRegistry() *WebhookHandlerRegistry {
	return &WebhookHandlerRegistry{handlers: map[string]WebhookHandler{}}
}

// Register adds (or replaces) the handler for its processor key.
func (r *WebhookHandlerRegistry) Register(h WebhookHandler) {
	if r == nil || h == nil {
		return
	}
	r.handlers[strings.ToLower(strings.TrimSpace(h.Processor()))] = h
}

// Handler resolves the handler for a processor name, mapping NMI gateway aliases
// (e.g. "nmi", "mobius") onto the registered "nmi" handler.
func (r *WebhookHandlerRegistry) Handler(processor string) (WebhookHandler, bool) {
	if r == nil {
		return nil, false
	}
	key := strings.ToLower(strings.TrimSpace(processor))
	if h, ok := r.handlers[key]; ok {
		return h, true
	}
	if processors.IsNMIBacked(key) {
		if h, ok := r.handlers["nmi"]; ok {
			return h, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// Stripe
// ---------------------------------------------------------------------------

// StripeWebhookHandler verifies + normalizes Stripe webhooks.
type StripeWebhookHandler struct {
	Secret    string
	Tolerance time.Duration
}

func (StripeWebhookHandler) Processor() string { return "stripe" }

func (h StripeWebhookHandler) Verify(msg *WebhookMessage) error {
	if msg == nil {
		return fmt.Errorf("nil webhook message")
	}
	if strings.TrimSpace(h.Secret) == "" {
		return fmt.Errorf("stripe webhook secret not configured")
	}
	// Delegate to the canonical verifier (single source of truth). Tolerance is
	// the configured replay window; the queued path may legitimately pass 0 to
	// skip it for delayed/retried jobs while still checking the HMAC.
	return sigverify.VerifyStripe(h.Secret, msg.Signature, msg.Payload, h.Tolerance)
}

func (h StripeWebhookHandler) Normalize(msg *WebhookMessage) (WebhookEvent, error) {
	if msg == nil {
		return WebhookEvent{}, fmt.Errorf("nil webhook message")
	}
	var evt stripeEvent
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return WebhookEvent{}, fmt.Errorf("parse stripe webhook: %w", err)
	}
	out := WebhookEvent{
		Processor:    "stripe",
		Type:         mapStripeEventType(evt.Type),
		RawType:      evt.Type,
		ProcessorRef: evt.ID,
		Raw:          msg.Payload,
	}
	if evt.Created > 0 {
		out.OccurredAt = time.Unix(evt.Created, 0).UTC()
	}
	if len(evt.Data.Object) > 0 {
		var obj struct {
			ID           string `json:"id"`
			Subscription string `json:"subscription"`
			Currency     string `json:"currency"`
			Amount       int64  `json:"amount"`
			AmountPaid   int64  `json:"amount_paid"`
			AmountTotal  int64  `json:"amount_total"`
		}
		_ = json.Unmarshal(evt.Data.Object, &obj)
		out.Currency = strings.ToUpper(strings.TrimSpace(obj.Currency))
		switch {
		case strings.TrimSpace(obj.Subscription) != "":
			out.SubscriptionRef = obj.Subscription
		case strings.HasPrefix(evt.Type, "customer.subscription."):
			out.SubscriptionRef = obj.ID
		}
		switch {
		case obj.AmountPaid != 0:
			out.Amount = obj.AmountPaid
		case obj.AmountTotal != 0:
			out.Amount = obj.AmountTotal
		default:
			out.Amount = obj.Amount
		}
	}
	return out, nil
}

func mapStripeEventType(t string) WebhookEventType {
	switch t {
	case "invoice.paid", "invoice_payment.paid", "charge.succeeded", "checkout.session.async_payment_succeeded":
		return WebhookEventPaymentSucceeded
	case "invoice.payment_failed", "checkout.session.async_payment_failed":
		return WebhookEventPaymentFailed
	case "checkout.session.completed":
		return WebhookEventCheckoutCompleted
	case "checkout.session.expired":
		return WebhookEventCheckoutExpired
	case "customer.subscription.updated":
		return WebhookEventSubscriptionUpdated
	case "customer.subscription.deleted":
		return WebhookEventSubscriptionCanceled
	case "refund.created", "refund.updated", "charge.refunded":
		return WebhookEventRefund
	case "charge.dispute.created", "charge.dispute.closed":
		return WebhookEventChargeback
	case "payment_method.attached":
		return WebhookEventCustomerUpdated
	default:
		return WebhookEventUnknown
	}
}

// ---------------------------------------------------------------------------
// NMI (and NMI-backed gateways, e.g. Mobius)
// ---------------------------------------------------------------------------

// NMIWebhookHandler verifies + normalizes NMI webhooks. SecretFor resolves the
// signing secret for the concrete processor name on the message (NMI deployments
// can run multiple gateway aliases, each with its own secret).
type NMIWebhookHandler struct {
	SecretFor func(processor string) string
}

func (NMIWebhookHandler) Processor() string { return "nmi" }

func (h NMIWebhookHandler) Verify(msg *WebhookMessage) error {
	if msg == nil {
		return fmt.Errorf("nil webhook message")
	}
	secret := strings.TrimSpace(msg.SigningSecret)
	if secret == "" && h.SecretFor != nil {
		secret = h.SecretFor(msg.Processor)
	}
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("nmi webhook secret not configured")
	}
	// Delegate to the canonical verifier with tolerance 0: the queued re-verify
	// authenticates the HMAC but does not re-impose the replay window, since a
	// job may be processed well after the original delivery.
	return sigverify.VerifyNMI(secret, msg.Signature, msg.Payload, 0)
}

func (h NMIWebhookHandler) Normalize(msg *WebhookMessage) (WebhookEvent, error) {
	if msg == nil {
		return WebhookEvent{}, fmt.Errorf("nil webhook message")
	}
	var evt NMIWebhookEvent
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return WebhookEvent{}, fmt.Errorf("parse nmi webhook: %w", err)
	}
	rawType := string(evt.EventType)
	out := WebhookEvent{
		Processor:    strings.TrimSpace(msg.Processor),
		Type:         mapNMIEventType(rawType),
		RawType:      rawType,
		ProcessorRef: evt.EventID,
		Raw:          msg.Payload,
	}
	switch {
	case strings.HasPrefix(rawType, "recurring."):
		var body NMIRecurringEventBody
		if err := json.Unmarshal(evt.EventBody, &body); err == nil {
			out.SubscriptionRef = body.SubscriptionID.Trimmed()
		}
	case strings.HasPrefix(rawType, "transaction."):
		var body NMITransactionEventBody
		if err := json.Unmarshal(evt.EventBody, &body); err == nil {
			out.SubscriptionRef = body.OrderID.Trimmed()
			out.Currency = strings.ToUpper(body.Currency.Trimmed())
		}
	}
	return out, nil
}

func mapNMIEventType(t string) WebhookEventType {
	switch t {
	case EventTypeNMIAddSubscription:
		return WebhookEventSubscriptionCreated
	case EventTypeNMIUpdateSubscription:
		return WebhookEventSubscriptionUpdated
	case EventTypeNMIDeleteSubscription:
		return WebhookEventSubscriptionCanceled
	case EventTypeNMITransactionSuccess:
		return WebhookEventPaymentSucceeded
	case EventTypeNMITransactionFailure:
		return WebhookEventPaymentFailed
	case EventTypeNMIRefundSuccess:
		return WebhookEventRefund
	case EventTypeNMIVoidSuccess:
		return WebhookEventVoid
	case EventTypeNMIACUUpdated, EventTypeNMIACUContactCustomer, EventTypeNMIACUClosedAccount:
		return WebhookEventCustomerUpdated
	case EventTypeNMIChargebackComplete:
		return WebhookEventChargeback
	default:
		// refund.failure / void.failure and anything unmapped.
		return WebhookEventUnknown
	}
}

// ---------------------------------------------------------------------------
// CCBill
// ---------------------------------------------------------------------------

// CCBillWebhookHandler normalizes CCBill webhooks. CCBill events are
// authenticated at ingestion (account-number match), reflected in
// WebhookMessage.SignatureValid, so there is no per-message HMAC to re-check.
type CCBillWebhookHandler struct{}

func (CCBillWebhookHandler) Processor() string { return "ccbill" }

func (h CCBillWebhookHandler) Verify(msg *WebhookMessage) error {
	if msg == nil {
		return fmt.Errorf("nil webhook message")
	}
	return nil
}

func (h CCBillWebhookHandler) Normalize(msg *WebhookMessage) (WebhookEvent, error) {
	if msg == nil {
		return WebhookEvent{}, fmt.Errorf("nil webhook message")
	}
	out := WebhookEvent{
		Processor:    "ccbill",
		Type:         mapCCBillEventType(msg.EventType),
		RawType:      msg.EventType,
		ProcessorRef: strings.TrimSpace(msg.EventID),
		Raw:          msg.Payload,
	}
	if len(msg.Payload) > 0 {
		var body map[string]interface{}
		if err := json.Unmarshal(msg.Payload, &body); err == nil {
			out.SubscriptionRef = ccbillPayloadStringField(body, "subscriptionId")
			if out.ProcessorRef == "" {
				out.ProcessorRef = ccbillPayloadStringField(body, "transactionId")
			}
		}
	}
	return out, nil
}

func mapCCBillEventType(t string) WebhookEventType {
	switch t {
	case EventTypeNewSaleSuccess:
		return WebhookEventPaymentSucceeded
	case EventTypeNewSaleFailure:
		return WebhookEventPaymentFailed
	case EventTypeRenewalSuccess:
		return WebhookEventSubscriptionRenewed
	case EventTypeRenewalFailure:
		return WebhookEventSubscriptionRenewalFailed
	case EventTypeUpgradeSuccess, EventTypeUpgradeFailure, EventTypeBillingDateChange:
		return WebhookEventSubscriptionUpdated
	case EventTypeCancellation:
		return WebhookEventSubscriptionCanceled
	case EventTypeExpiration:
		return WebhookEventSubscriptionExpired
	case EventTypeUserReactivation:
		return WebhookEventSubscriptionReactivated
	case EventTypeCustomerDataUpdate:
		return WebhookEventCustomerUpdated
	case EventTypeRefund:
		return WebhookEventRefund
	case EventTypeVoid:
		return WebhookEventVoid
	case EventTypeChargeback:
		return WebhookEventChargeback
	default:
		return WebhookEventUnknown
	}
}
