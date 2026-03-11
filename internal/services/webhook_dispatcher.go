package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/processors"
)

type webhookCheckoutSessionStore interface {
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
	SignatureValid *bool
	ReceivedAt     time.Time
}

// WebhookDispatcher routes persisted webhook events to processor-specific handlers.
type WebhookDispatcher struct {
	DB                           *db.DB
	Clock                        clockwork.Clock
	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	NotificationService          *NotificationService
	SubscriptionService          *subscriptions.SubscriptionService
	PaymentService               *payments.PaymentService
	EventLogService              *EventLogService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	ProfileRepo                  *repo.ProfileRepo
	DeduplicationService         *DeduplicationService
	ProcessorCustomerService     *payments.ProcessorCustomerService
	CCBillRESTClient             *ccbill.RESTClient
	NMIClients                   map[string]*nmi.NMIClient
	PurchaseRegistrar            stripePurchaseRegistrar
	CheckoutSessionService       webhookCheckoutSessionStore
	CreditsService               *credits.CreditsService
}

// Process executes the processor-specific webhook flow.
func (d *WebhookDispatcher) Process(ctx context.Context, event *WebhookMessage) error {
	if event == nil {
		return fmt.Errorf("webhook event is required")
	}
	processor := strings.ToLower(strings.TrimSpace(event.Processor))
	switch {
	case processor == "ccbill":
		return d.processCCBill(ctx, event)
	case processors.IsNMIBacked(processor):
		return d.processNMI(ctx, event)
	case processor == "stripe":
		return d.processStripe(ctx, event)
	default:
		return fmt.Errorf("unsupported webhook processor: %s", processor)
	}
}

func (d *WebhookDispatcher) processCCBill(ctx context.Context, event *WebhookMessage) error {
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
		CreditsService:               d.CreditsService,
	}
	return service.HandleCCBillWebhook(ctx)
}

func (d *WebhookDispatcher) processNMI(ctx context.Context, event *WebhookMessage) error {
	var payload NMIWebhookEvent
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("parse nmi webhook payload: %w", err)
	}
	client := d.NMIClients[event.Processor]
	if client == nil {
		return fmt.Errorf("nmi client '%s' not configured", event.Processor)
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
		CreditsService:               d.CreditsService,
		DeduplicationService:         d.DeduplicationService,
		NotificationService:          d.NotificationService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
	}
	return service.HandleNMIWebhook(ctx)
}

func (d *WebhookDispatcher) processStripe(ctx context.Context, event *WebhookMessage) error {
	service := StripeWebhookService{
		DB:                           d.DB,
		PriceService:                 d.PriceService,
		ProductService:               d.ProductService,
		SubscriptionService:          d.SubscriptionService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
		PurchaseRegistrar:            d.PurchaseRegistrar,
		PaymentService:               d.PaymentService,
		CreditsService:               d.CreditsService,
		DeduplicationService:         d.DeduplicationService,
		ProcessorCustomerService:     d.ProcessorCustomerService,
		CheckoutSessionService:       d.CheckoutSessionService,
	}
	return service.HandleStripeWebhook(ctx, event.Payload)
}
