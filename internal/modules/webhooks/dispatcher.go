package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
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
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/processors"
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
	if !webhookSignatureVerified(event) {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("nmi webhook signature was not verified before processing"))
	}
	client := d.NMIClients[event.Processor]
	if client == nil {
		return fmt.Errorf("nmi client '%s' not configured", event.Processor)
	}
	if err := verifyQueuedNMISignature(client.GetWebhookSecret(), event.Signature, event.Payload); err != nil {
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
		CreditsService:               d.CreditsService,
		DeduplicationService:         d.DeduplicationService,
		NotificationService:          d.NotificationService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
	}
	return service.HandleNMIWebhook(ctx)
}

func (d *WebhookDispatcher) processStripe(ctx context.Context, event *WebhookMessage) error {
	if !webhookSignatureVerified(event) {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe webhook signature was not verified before processing"))
	}
	// HTTP ingestion verifies the original Stripe payload before optional thin-event
	// hydration. Re-verifying the hydrated body against Stripe's signature would
	// incorrectly reject valid thin events because the signed bytes changed.
	service := StripeWebhookService{
		DB:                           d.DB,
		PriceService:                 d.PriceService,
		ProductService:               d.ProductService,
		SubscriptionService:          d.SubscriptionService,
		SubscriptionLifecycleService: d.SubscriptionLifecycleService,
		NotificationService:          d.NotificationService,
		PurchaseRegistrar:            d.PurchaseRegistrar,
		PaymentService:               d.PaymentService,
		CreditsService:               d.CreditsService,
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

func verifyQueuedStripeSignature(secret, header string, body []byte, tolerance time.Duration) error {
	timestamp, signatures := parseStripeSignatureHeader(header)
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("stripe webhook secret not configured")
	}
	if timestamp == "" || len(signatures) == 0 {
		return fmt.Errorf("invalid stripe signature header")
	}
	tsInt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid stripe signature timestamp")
	}
	if tolerance > 0 {
		now := time.Now().Unix()
		if now-tsInt > int64(tolerance.Seconds()) || tsInt-now > int64(tolerance.Seconds()) {
			return fmt.Errorf("stripe signature timestamp outside tolerance")
		}
	}
	signedPayload := fmt.Sprintf("%s.%s", timestamp, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signedPayload))
	expected := hex.EncodeToString(mac.Sum(nil))
	for _, sig := range signatures {
		if hmac.Equal([]byte(expected), []byte(sig)) {
			return nil
		}
	}
	return fmt.Errorf("stripe signature mismatch")
}

func parseStripeSignatureHeader(header string) (string, []string) {
	parts := strings.Split(header, ",")
	var ts string
	sigs := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "t=") {
			ts = strings.TrimPrefix(part, "t=")
			continue
		}
		if strings.HasPrefix(part, "v1=") {
			sigs = append(sigs, strings.TrimPrefix(part, "v1="))
		}
	}
	return ts, sigs
}

func verifyQueuedNMISignature(secret, header string, body []byte) error {
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("nmi webhook secret not configured")
	}
	timestamp, signature, err := parseNMISignatureHeader(header)
	if err != nil {
		return err
	}
	if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		return fmt.Errorf("invalid webhook signature timestamp")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(timestamp + "." + string(body)))
	expectedSig := mac.Sum(nil)
	providedSig, err := hex.DecodeString(strings.ToLower(signature))
	if err != nil {
		return fmt.Errorf("invalid webhook signature")
	}
	if !hmac.Equal(providedSig, expectedSig) {
		return fmt.Errorf("invalid webhook signature")
	}
	return nil
}

func parseNMISignatureHeader(header string) (string, string, error) {
	var ts string
	var sig string
	parts := strings.Split(header, ",")
	for _, part := range parts {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.TrimSpace(kv[0]) {
		case "t":
			ts = strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		case "s":
			sig = strings.Trim(strings.TrimSpace(kv[1]), `"'`)
		}
	}
	if ts == "" || sig == "" {
		return "", "", fmt.Errorf("unrecognized webhook signature format")
	}
	return ts, sig, nil
}
