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
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

type webhookCheckoutSessionStore interface {
	FindOpenCCBillReservation(ctx context.Context, reservationID string, userID string, priceID uuid.UUID) (*models.CheckoutSession, error)
	FindOpenByUserPriceProcessor(ctx context.Context, userID string, priceID uuid.UUID, processor models.Processor) (*models.CheckoutSession, error)
	MarkSucceeded(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string) error
	MarkSucceededWithSubscription(ctx context.Context, sessionID uuid.UUID, paymentID uuid.UUID, transactionID string, subscriptionID uuid.UUID) error
	MarkFailed(ctx context.Context, sessionID uuid.UUID, failureMessage, failureCode string) error
	MarkExpired(ctx context.Context, sessionID uuid.UUID, reason string) error
}

// WebhookMessage is the runtime representation of a webhook event that needs dispatching.
// It is intentionally minimal and decoupled from any database persistence.
type WebhookMessage struct {
	Processor      string
	EventID        string
	EventType      string
	Payload        []byte
	IPAddress      string
	Signature      string
	SigningSecret  string
	SignatureValid *bool
	ReceivedAt     time.Time
}

// WebhookDispatcher routes persisted webhook events to processor-specific handlers.
type WebhookDispatcher struct {
	Config                       *config.Config
	DB                           *db.DB
	Clock                        clockwork.Clock
	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	NotificationService          *subscriptions.NotificationService
	SubscriptionService          *subscriptions.SubscriptionService
	PaymentService               *payments.PaymentService
	EventLogService              *analytics.EventLogService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	ProfileRepo                  *repo.ProfileRepo
	DeduplicationService         *DeduplicationService
	ProcessorCustomerService     *payments.ProcessorCustomerService
	CCBillRESTClient             *ccbill.RESTClient
	NMIClients                   map[string]*nmi.NMIClient
	PurchaseRegistrar            stripePurchaseRegistrar
	CheckoutSessionService       webhookCheckoutSessionStore
	MoneyService                 *money.MoneyService
}

// webhookRegistry resolves WebhookHandlers by processor. The dispatcher is fully
// registry-driven: Process looks up the handler and calls Apply, so adding a
// processor is "implement the WebhookHandler interface + register here" rather
// than adding a branch (issue #296). The NMIWebhookHandler resolves its signing
// secret per gateway alias from the dispatcher's NMI clients.
func (d *WebhookDispatcher) webhookRegistry() *WebhookHandlerRegistry {
	reg := NewWebhookHandlerRegistry()
	reg.Register(StripeWebhookHandler{})
	reg.Register(NMIWebhookHandler{SecretFor: func(processor string) string {
		if c := d.NMIClients[processor]; c != nil {
			return c.GetWebhookSecret()
		}
		return ""
	}})
	reg.Register(CCBillWebhookHandler{})
	return reg
}

// Process resolves the registered handler for the message's processor and runs
// its Apply. No processor switch lives here anymore.
func (d *WebhookDispatcher) Process(ctx context.Context, event *WebhookMessage) error {
	if event == nil {
		return fmt.Errorf("webhook event is required")
	}
	handler, ok := d.webhookRegistry().Handler(event.Processor)
	if !ok {
		return fmt.Errorf("unsupported webhook processor: %s", strings.ToLower(strings.TrimSpace(event.Processor)))
	}
	return handler.Apply(ctx, d, event)
}

// Apply builds the CCBill webhook service from the dispatcher and runs it.
func (h CCBillWebhookHandler) Apply(ctx context.Context, d *WebhookDispatcher, event *WebhookMessage) error {
	if d.CCBillRESTClient == nil {
		return fmt.Errorf("ccbill rest client not configured")
	}
	data := CCBillWebhookEvent{
		EventType: CCBillWebhookEventType(event.EventType),
		EventBody: json.RawMessage(event.Payload),
	}
	service := CCBillWebhookService{
		Data:                         data,
		DB:                           d.DB,
		CCBillClient:                 d.CCBillRESTClient,
		ProductService:               d.ProductService,
		PriceService:                 d.PriceService,
		NotificationService:          d.NotificationService,
		EventLogService:              d.EventLogService,
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
	var client *nmi.NMIClient
	if d.NMIClients != nil {
		client = d.NMIClients[event.Processor]
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
		Processor:                    event.Processor,
		NMIClient:                    client,
		EventLogService:              d.EventLogService,
		SubscriptionService:          d.SubscriptionService,
		PaymentService:               d.PaymentService,
		MoneyService:                 d.MoneyService,
		DeduplicationService:         d.DeduplicationService,
		NotificationService:          d.NotificationService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
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
	service := StripeWebhookService{
		DB:                           d.DB,
		PriceService:                 d.PriceService,
		ProductService:               d.ProductService,
		SubscriptionService:          d.SubscriptionService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
		NotificationService:          d.NotificationService,
		PurchaseRegistrar:            d.PurchaseRegistrar,
		PaymentService:               d.PaymentService,
		EventLogService:              d.EventLogService,
		MoneyService:                 d.MoneyService,
		DeduplicationService:         d.DeduplicationService,
		ProcessorCustomerService:     d.ProcessorCustomerService,
		CheckoutSessionService:       d.CheckoutSessionService,
		Clock:                        d.Clock,
	}
	return service.HandleStripeWebhook(ctx, event.Payload)
}

func webhookSignatureVerified(event *WebhookMessage) bool {
	return event != nil && event.SignatureValid != nil && *event.SignatureValid
}
