package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/identity"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/railresolve"
)

// CheckoutSessionStore is the exported alias of the checkout-session surface
// webhook/converge paths use, so the River converge worker can inject the
// runtime's checkout session service without importing the checkout package.
type CheckoutSessionStore = webhookCheckoutSessionStore

type webhookCheckoutSessionStore interface {
	FindOpenCCBillReservation(ctx context.Context, reservationID string, userID string, priceID uuid.UUID) (*models.CheckoutSession, error)
	FindOpenByUserPriceRail(ctx context.Context, userID string, priceID uuid.UUID, rail models.Rail) (*models.CheckoutSession, error)
	MarkSucceeded(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string) error
	MarkSucceededWithSubscription(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string, subscriptionID uuid.UUID) error
	MarkFailed(ctx context.Context, sessionID uuid.UUID, failureMessage, failureCode string) error
	MarkExpired(ctx context.Context, sessionID uuid.UUID, reason string) error
}

// WebhookMessage is the runtime representation of a webhook event that needs dispatching.
// It is intentionally minimal and decoupled from any database persistence.
type WebhookMessage struct {
	Rail           string
	EventID        string
	EventType      string
	Payload        []byte
	IPAddress      string
	Signature      string
	SigningSecret  string
	SignatureValid *bool
	ReceivedAt     time.Time
	// PspID (#641) is the account_id the event was routed to, so
	// dispatch selects that account's rail client. Empty = primary.
	PspID string
	// CustodianAccountID (or#880) is the CUSTODIAN-native account id a custody
	// event was routed by (Basis Theory: the tenant id). Custody is not a
	// rail, so it is a field of its own and never overloads PspID.
	CustodianAccountID string
}

// WebhookDispatcher routes persisted webhook events to rail-specific handlers.
type WebhookDispatcher struct {
	Config                       *config.Config
	DB                           *db.DB
	Clock                        clockwork.Clock
	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	NotificationService          *subscriptions.NotificationService
	SubscriptionService          *subscriptions.SubscriptionService
	PaymentService               *payments.PaymentService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	ProfileRepo                  *identity.ProfileRepo
	DeduplicationService         *DeduplicationService
	RailCustomerService          *payments.RailCustomerService
	// RailConfigs resolves per-merchant armed rail credentials at dispatch
	// time (Layer C, #788): the ONLY rail-credential source in this package.
	// The merchant comes from ctx; an unarmed rail fails closed.
	RailConfigs            railresolve.Source
	PurchaseRegistrar      stripePurchaseRegistrar
	CheckoutSessionService webhookCheckoutSessionStore
	MoneyService           *money.MoneyService
	// ConvergeEnqueuer (#684): schedules the coalesced fetch-and-converge job
	// the slimmed Stripe/NMI subscription-state handlers enqueue.
	ConvergeEnqueuer SubscriptionConvergeEnqueuer
	// StripePaymentStateReaderFactory is a test seam for exact-account provider
	// reads. Production leaves it nil and uses the HTTP reader.
	StripePaymentStateReaderFactory func(secretKey string) payments.StripePaymentStateReader
}

// webhookRegistry resolves WebhookHandlers by rail. The dispatcher is fully
// registry-driven: Process looks up the handler and calls Apply, so adding a
// rail is "implement the WebhookHandler interface + register here" rather
// than adding a branch (issue #296).
func (d *WebhookDispatcher) webhookRegistry() *WebhookHandlerRegistry {
	reg := NewWebhookHandlerRegistry()
	reg.Register(StripeWebhookHandler{})
	reg.Register(NMIWebhookHandler{})
	reg.Register(CCBillWebhookHandler{})
	reg.Register(BasisTheoryWebhookHandler{})
	return reg
}

// Process resolves the registered handler for the message's rail and runs
// its Apply. No rail switch lives here anymore.
func (d *WebhookDispatcher) Process(ctx context.Context, event *WebhookMessage) error {
	if event == nil {
		return fmt.Errorf("webhook event is required")
	}
	handler, ok := d.webhookRegistry().Handler(event.Rail)
	if !ok {
		return fmt.Errorf("unsupported webhook rail: %s", strings.ToLower(strings.TrimSpace(event.Rail)))
	}
	return handler.Apply(ctx, d, event)
}

// Apply builds the CCBill webhook service from the dispatcher and runs it.
// The CCBill client is built PER MERCHANT at dispatch time from the armed
// psps state (#788): the ctx merchant plus the routed
// account id (empty = the active account) resolve through RailConfigs. An
// unarmed rail or a resolution failure rejects the webhook — retryable, so
// the provider redelivers once the rail is armed; never default-allow.
func (h CCBillWebhookHandler) Apply(ctx context.Context, d *WebhookDispatcher, event *WebhookMessage) error {
	if d.RailConfigs == nil {
		return fmt.Errorf("ccbill webhook rejected: rail resolution is not configured")
	}
	proc, err := d.RailConfigs.RailConfig(ctx, string(models.RailCCBill), event.PspID)
	if err != nil {
		return fmt.Errorf("ccbill webhook rejected: %w", err)
	}
	ccbillConfig := proc.ToCCBillConfig()
	if ccbillConfig.ClientAccNum == "" || ccbillConfig.ClientSubAcc == "" {
		return fmt.Errorf("ccbill webhook rejected: armed account %q is not a valid clientAccnum-clientSubacc pair", proc.EffectiveAccountID())
	}
	data := CCBillWebhookEvent{
		EventType: CCBillWebhookEventType(event.EventType),
		EventBody: json.RawMessage(event.Payload),
	}
	service := CCBillWebhookService{
		Data:                         data,
		DB:                           d.DB,
		CCBillClient:                 ccbill.NewRESTClient(ccbillConfig),
		ProductService:               d.ProductService,
		PriceService:                 d.PriceService,
		NotificationService:          d.NotificationService,
		SubscriptionService:          d.SubscriptionService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
		ProfileRepo:                  d.ProfileRepo,
		PaymentService:               d.PaymentService,
		DeduplicationService:         d.DeduplicationService,
		CheckoutSessionService:       d.CheckoutSessionService,
		MoneyService:                 d.MoneyService,
	}
	return service.HandleCCBillWebhook(ctx)
}

// Apply verifies the queued NMI signature through the handler, then builds and
// runs the NMI webhook service from the dispatcher.
func (h NMIWebhookHandler) Apply(ctx context.Context, d *WebhookDispatcher, event *WebhookMessage) error {
	if !webhookSignatureVerified(event) {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("nmi webhook signature was not verified before processing"))
	}
	if err := h.Verify(event); err != nil {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("nmi queued webhook signature verification failed: %w", err))
	}
	var payload NMIWebhookEvent
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("parse nmi webhook payload: %w", err)
	}

	service := NMIWebhookService{
		DB:                           d.DB,
		PriceService:                 d.PriceService,
		ProductService:               d.ProductService,
		Data:                         payload,
		Rail:                         event.Rail,
		SubscriptionService:          d.SubscriptionService,
		PaymentService:               d.PaymentService,
		MoneyService:                 d.MoneyService,
		DeduplicationService:         d.DeduplicationService,
		NotificationService:          d.NotificationService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
		ConvergeEnqueuer:             d.ConvergeEnqueuer,
	}
	return service.HandleNMIWebhook(ctx)
}

// Apply builds the Stripe webhook service from the dispatcher and runs it.
// Stripe is verified at HTTP ingestion (before optional thin-event hydration);
// re-verifying the hydrated body here would reject valid thin events because the
// signed bytes changed, so Apply trusts the ingestion-set SignatureValid flag.
func (h StripeWebhookHandler) Apply(ctx context.Context, d *WebhookDispatcher, event *WebhookMessage) error {
	if !webhookSignatureVerified(event) {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe webhook signature was not verified before processing"))
	}
	var paymentState payments.StripePaymentStateReader
	if stripeEventNeedsPaymentState(event.EventType) {
		if d.RailConfigs == nil {
			return fmt.Errorf("stripe webhook rejected: rail resolution is not configured")
		}
		proc, err := d.RailConfigs.RailConfig(ctx, string(models.RailStripe), event.PspID)
		if err != nil {
			return fmt.Errorf("stripe webhook rejected: %w", err)
		}
		if proc == nil || proc.Stripe == nil || strings.TrimSpace(proc.Stripe.SecretKey) == "" {
			return fmt.Errorf("stripe webhook rejected: routed account %q has no secret key", event.PspID)
		}
		factory := d.StripePaymentStateReaderFactory
		if factory == nil {
			factory = func(secretKey string) payments.StripePaymentStateReader {
				return payments.NewHTTPStripePaymentStateReader(secretKey)
			}
		}
		paymentState = factory(proc.Stripe.SecretKey)
		if paymentState == nil {
			return fmt.Errorf("stripe webhook rejected: payment-state reader is not configured")
		}
	}
	service := StripeWebhookService{
		DB:                           d.DB,
		PriceService:                 d.PriceService,
		ProductService:               d.ProductService,
		SubscriptionService:          d.SubscriptionService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
		NotificationService:          d.NotificationService,
		PurchaseRegistrar:            d.PurchaseRegistrar,
		PaymentService:               d.PaymentService,
		MoneyService:                 d.MoneyService,
		DeduplicationService:         d.DeduplicationService,
		RailCustomerService:          d.RailCustomerService,
		CheckoutSessionService:       d.CheckoutSessionService,
		Clock:                        d.Clock,
		ConvergeEnqueuer:             d.ConvergeEnqueuer,
		StripePaymentState:           paymentState,
	}
	return service.HandleStripeWebhook(ctx, event.Payload)
}

func stripeEventNeedsPaymentState(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "charge.succeeded",
		"payment_method.attached",
		"payment_method.detached",
		"customer.updated",
		"customer.subscription.updated",
		"customer.subscription.deleted":
		return true
	default:
		return false
	}
}

func webhookSignatureVerified(event *WebhookMessage) bool {
	return event != nil && event.SignatureValid != nil && *event.SignatureValid
}
