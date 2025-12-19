package services

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/doujins-org/doujins-billing/internal/db"
	"github.com/doujins-org/doujins-billing/internal/db/models"
	"github.com/doujins-org/doujins-billing/internal/db/repo"
	"github.com/doujins-org/doujins-billing/internal/integrations/ccbill"
	"github.com/doujins-org/doujins-billing/internal/integrations/nmi"
	"github.com/doujins-org/doujins-billing/internal/processors"
	"github.com/jonboulle/clockwork"
)

// WebhookDispatcher routes persisted webhook events to processor-specific handlers.
type WebhookDispatcher struct {
	DB                           *db.DB
	Clock                        clockwork.Clock
	PriceService                 *PriceService
	ProductService               *ProductService
	NotificationService          *NotificationService
	SubscriptionService          *SubscriptionService
	PaymentService               *PaymentService
	EventLogService              *EventLogService
	SubscriptionLifecycleService *SubscriptionLifecycleService
	ProfileRepo                  *repo.ProfileRepo
	DeduplicationService         *DeduplicationService
	ProcessorCustomerService     *ProcessorCustomerService
	CCBillRESTClient             *ccbill.RESTClient
	NMIClients                   map[string]*nmi.NMIClient
	CheckoutService              *CheckoutService
	CreditsService               *CreditsService
}

// Process executes the processor-specific webhook flow.
func (d *WebhookDispatcher) Process(ctx context.Context, event *models.WebhookEvent) error {
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

func (d *WebhookDispatcher) processCCBill(ctx context.Context, event *models.WebhookEvent) error {
	if d.CCBillRESTClient == nil {
		return fmt.Errorf("ccbill rest client not configured")
	}
	data := CCBillWebhookEvent{
		EventType: CCBillWebhookEventType(event.EventType),
		EventBody: json.RawMessage(event.RawPayload),
	}
	service := CCBillWebhookService{
		Data:                         data,
		DB:                           d.DB,
		CCBillClient:                 d.CCBillRESTClient,
		ProductService:               d.ProductService,
		PriceService:                 d.PriceService,
		NotificationService:          d.NotificationService,
		DeadLetterService:            &DeadLetterService{DB: d.DB, NotificationService: d.NotificationService},
		EventLogService:              d.EventLogService,
		SubscriptionService:          d.SubscriptionService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
		ProfileRepo:                  d.ProfileRepo,
		PaymentService:               d.PaymentService,
		DeduplicationService:         d.DeduplicationService,
	}
	return service.HandleCCBillWebhook(ctx)
}

func (d *WebhookDispatcher) processNMI(ctx context.Context, event *models.WebhookEvent) error {
	var payload NMIWebhookEvent
	if err := json.Unmarshal([]byte(event.RawPayload), &payload); err != nil {
		return fmt.Errorf("parse nmi webhook payload: %w", err)
	}
	provider := strings.ToLower(strings.TrimSpace(extractProcessor(event.Headers)))
	if provider == "" {
		provider = "mobius"
	}
	client := d.NMIClients[provider]
	if client == nil {
		return fmt.Errorf("nmi client '%s' not configured", provider)
	}

	service := NMIWebhookService{
		DB:                           d.DB,
		PriceService:                 d.PriceService,
		ProductService:               d.ProductService,
		Data:                         payload,
		Processor:                    provider,
		DeadLetterService:            &DeadLetterService{DB: d.DB, NotificationService: d.NotificationService},
		NMIClient:                    client,
		EventLogService:              d.EventLogService,
		SubscriptionService:          d.SubscriptionService,
		PaymentService:               d.PaymentService,
		DeduplicationService:         d.DeduplicationService,
		NotificationService:          d.NotificationService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
	}
	return service.HandleNMIWebhook(ctx)
}

func (d *WebhookDispatcher) processStripe(ctx context.Context, event *models.WebhookEvent) error {
	service := StripeWebhookService{
		DB:                           d.DB,
		PriceService:                 d.PriceService,
		ProductService:               d.ProductService,
		SubscriptionService:          d.SubscriptionService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
		CheckoutService:              d.CheckoutService,
		PaymentService:               d.PaymentService,
		CreditsService:               d.CreditsService,
		DeduplicationService:         d.DeduplicationService,
		ProcessorCustomerService:     d.ProcessorCustomerService,
	}
	return service.HandleStripeWebhook(ctx, []byte(event.RawPayload))
}

func extractProcessor(headers map[string]string) string {
	if headers == nil {
		return ""
	}
	if provider, ok := headers["x-internal-processor"]; ok {
		return provider
	}
	if provider, ok := headers["processor"]; ok {
		return provider
	}
	return ""
}
