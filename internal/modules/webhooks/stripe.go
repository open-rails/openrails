package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/booktag"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/modules/payment"

	"github.com/open-rails/openrails/internal/modules/alerting"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/disputerules"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/disputes"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

type StripeWebhookService struct {
	DB                           *db.DB
	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	PaymentService               *payments.PaymentService
	MoneyService                 *money.MoneyService
	DeduplicationService         *DeduplicationService
	RailCustomerService          *payments.RailCustomerService
	InvoiceAttemptService        *payment.InvoiceAttemptService
	Clock                        clockwork.Clock
	// ConvergeEnqueuer: subscription-state events are wake-up signals; the
	// handler marks the subscription dirty and the coalesced River job fetches
	// provider truth and converges it.
	ConvergeEnqueuer SubscriptionConvergeEnqueuer
	// StripePaymentState reads the exact routed Stripe account. Payment-method
	// events are wake-up signals; fetched provider truth owns the local link.
	StripePaymentState *payments.HTTPStripePaymentStateReader
	// ObserveEnqueuer queues the coalesced read of a catalog object a
	// product.* or price.* event names.
	ObserveEnqueuer ProviderObjectObserveEnqueuer
	// Disputes records disputes and fraud reports; nil records with no alerts.
	Disputes *disputes.Service
	Log      *slog.Logger
}

func (s *StripeWebhookService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

type stripeEvent struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Created    int64           `json:"created"`
	APIVersion string          `json:"api_version"`
	Data       stripeEventData `json:"data"`
}

type stripeEventData struct {
	Object             json.RawMessage `json:"object"`
	PreviousAttributes json.RawMessage `json:"previous_attributes"`
}

type stripeInvoiceLineItem struct {
	Period struct {
		Start int64 `json:"start"`
		End   int64 `json:"end"`
	} `json:"period"`
	Price struct {
		ID string `json:"id"`
	} `json:"price"`
	// The 2026-04-22.preview API moved the price reference off the line item's
	// `price` object into `pricing.price_details.price` (a bare id string).
	Pricing struct {
		PriceDetails struct {
			Price string `json:"price"`
		} `json:"price_details"`
	} `json:"pricing"`
}

type stripeInvoice struct {
	ID string `json:"id"`
	// Created is the invoice's creation time (Unix sec): the provider's
	// transaction timestamp, recorded instead of the processing time.
	Created       int64  `json:"created"`
	Subscription  string `json:"subscription"`
	Customer      string `json:"customer"`
	CustomerEmail string `json:"customer_email"`
	PaymentIntent string `json:"payment_intent"`
	Charge        string `json:"charge"`
	AmountPaid    int64  `json:"amount_paid"`
	AmountDue     int64  `json:"amount_due"`
	Currency      string `json:"currency"`
	// BillingReason is Stripe's reason for the invoice; "subscription_update"
	// marks a mid-cycle change whose total is prorated, not the list price.
	BillingReason string            `json:"billing_reason"`
	Metadata      map[string]string `json:"metadata"`
	Lines         struct {
		Data []stripeInvoiceLineItem `json:"data"`
	} `json:"lines"`
	SubscriptionDetails struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"subscription_details"`
	// The 2026-04-22.preview shape nests subscription details (the subscription
	// id and the metadata propagated via subscription_data.metadata) under
	// `parent`; it has no top-level `subscription`, `charge` or `payment_intent`.
	Parent struct {
		SubscriptionDetails struct {
			Subscription string            `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
		} `json:"subscription_details"`
	} `json:"parent"`
}

// railSubscriptionID returns the Stripe subscription id, reading the
// top-level field (classic/snapshot shape) and falling back to
// parent.subscription_details.subscription (2026-04-22.preview shape).
func (inv stripeInvoice) railSubscriptionID() string {
	if id := strings.TrimSpace(inv.Subscription); id != "" {
		return id
	}
	return strings.TrimSpace(inv.Parent.SubscriptionDetails.Subscription)
}

type stripeCheckoutSession struct {
	ID            string            `json:"id"`
	Mode          string            `json:"mode"`
	Status        string            `json:"status"`
	PaymentStatus string            `json:"payment_status"`
	Subscription  string            `json:"subscription"`
	Customer      string            `json:"customer"`
	CustomerEmail string            `json:"customer_email"`
	PaymentIntent string            `json:"payment_intent"`
	Metadata      map[string]string `json:"metadata"`
	AmountTotal   int64             `json:"amount_total"`
	Currency      string            `json:"currency"`
}

type stripeRefund struct {
	ID            string `json:"id"`
	Charge        string `json:"charge"`
	PaymentIntent string `json:"payment_intent"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
	Created       int64  `json:"created"`
	// Metadata carries the billing book that created the refund (or, on a
	// charge.refunded event, the charge).
	Metadata map[string]string `json:"metadata"`
}

type stripeCharge struct {
	ID             string            `json:"id"`
	PaymentIntent  string            `json:"payment_intent"`
	Customer       string            `json:"customer"`
	Invoice        string            `json:"invoice"`
	PaymentMethod  string            `json:"payment_method"`
	Amount         int64             `json:"amount"`
	AmountRefunded int64             `json:"amount_refunded"`
	Metadata       map[string]string `json:"metadata"`
	Currency       string            `json:"currency"`
	// payment_method_details.card carries the brand/last4/exp for the card that
	// was actually charged. This is the no-fetch source for the card snapshot.
	PaymentMethodDetails struct {
		Card payments.StripeCardDetails `json:"card"`
	} `json:"payment_method_details"`
	Refunds struct {
		Data []stripeRefund `json:"data"`
	} `json:"refunds"`
}

type stripeInvoicePayment struct {
	ID      string `json:"id"`
	Invoice string `json:"invoice"`
	Payment struct {
		Type          string `json:"type"`
		Charge        string `json:"charge"`
		PaymentIntent string `json:"payment_intent"`
	} `json:"payment"`
}

// stripePaymentMethod is the identity carried by payment_method events.
type stripePaymentMethod struct {
	ID       string `json:"id"`
	Customer string `json:"customer"`
}

// redactStripeClientSecrets preserves JSON numbers exactly while removing only
// Stripe's client_secret field, including expanded objects and previous values.
func redactStripeClientSecrets(payload []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var event any
	if err := decoder.Decode(&event); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("Stripe event has trailing data")
	}
	var scrub func(any)
	scrub = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			delete(value, "client_secret")
			for _, child := range value {
				scrub(child)
			}
		case []any:
			for _, child := range value {
				scrub(child)
			}
		}
	}
	scrub(event)
	return json.Marshal(event)
}

func (s *StripeWebhookService) HandleStripeWebhook(ctx context.Context, payload []byte) error {
	// Ingestion authenticates the original bytes before this service runs.
	// Browser credentials are never needed by handlers or completed replay.
	payload, err := redactStripeClientSecrets(payload)
	if err != nil {
		return fmt.Errorf("parse stripe event: %w", err)
	}
	var evt stripeEvent
	if err := json.Unmarshal(payload, &evt); err != nil {
		return fmt.Errorf("parse stripe event: %w", err)
	}

	eventID := strings.TrimSpace(evt.ID)
	eventType := strings.TrimSpace(evt.Type)
	if eventID == "" || eventType == "" {
		return fmt.Errorf("stripe event missing id or type")
	}

	// Inbound events don't pass through the outbound client, so the endpoint's
	// API version is pinned in the Stripe dashboard. Warn (don't reject: the
	// parsers tolerate adjacent shapes) when it drifts from stripeapi.APIVersion.
	if v := strings.TrimSpace(evt.APIVersion); v != "" && v != stripeapi.APIVersion {
		s.Log.WarnContext(ctx, "stripe webhook api_version differs from pinned version; pin the webhook endpoint in the Stripe dashboard to match", "event_id", eventID, "event_type", eventType, "event_api_version", v, "pinned_api_version", stripeapi.APIVersion)
	}

	if s.DeduplicationService != nil {
		return s.DeduplicationService.ProcessWebhook(ctx, eventID, eventType, models.RailStripe.EventSource(), func(ctx context.Context) error {
			return s.handleEvent(ctx, eventType, evt)
		})
	}
	return s.handleEvent(ctx, eventType, evt)
}

// HandledStripeEventTypes is the canonical list of Stripe event types OpenRails
// acts on, and the enabled_events of a managed webhook endpoint. Keep in sync
// with the handleEvent switch below.
var HandledStripeEventTypes = []string{
	"payment_intent.succeeded",
	"payment_intent.payment_failed",
	"payment_intent.requires_action",
	"invoice.paid",
	"invoice.payment_failed",
	"invoice_payment.paid",
	"checkout.session.completed",
	"checkout.session.async_payment_succeeded",
	"checkout.session.async_payment_failed",
	"checkout.session.expired",
	"customer.subscription.updated",
	"customer.subscription.deleted",
	"refund.created",
	"refund.updated",
	"charge.succeeded",
	"payment_method.attached",
	"payment_method.detached",
	"payment_method.automatically_updated",
	"payment_method.updated",
	"customer.updated",
	"charge.refunded",
	"charge.dispute.created",
	"charge.dispute.updated",
	"charge.dispute.funds_withdrawn",
	"charge.dispute.closed",
	"radar.early_fraud_warning.created",
	"radar.early_fraud_warning.updated",
	"product.created",
	"product.updated",
	"product.deleted",
	"price.created",
	"price.updated",
	"price.deleted",
	"payout.created",
	"payout.updated",
	"payout.paid",
	"payout.failed",
	"payout.canceled",
	"payout.reconciliation_completed",
}

func (s *StripeWebhookService) handleEvent(ctx context.Context, eventType string, evt stripeEvent) error {
	obj := evt.Data.Object
	switch eventType {
	case "payment_intent.succeeded", "payment_intent.payment_failed", "payment_intent.requires_action":
		return s.wakeStripeEngineOperation(ctx, obj)
	// Subscription-state events are wake-up signals: parse only the dirty
	// object's identity and enqueue fetch-and-converge. Fetched provider truth,
	// never the payload, decides the transition, so event order is irrelevant.
	case "invoice.paid", "invoice.payment_failed":
		return s.markDirtyFromInvoice(ctx, eventType, obj, evt.Created)
	case "customer.subscription.updated", "customer.subscription.deleted":
		if err := s.markDirtyFromSubscription(ctx, eventType, obj, evt.Created); err != nil {
			return err
		}
		return s.reconcileStripePaymentStateFromSubscription(ctx, obj)

	case "invoice_payment.paid":
		return s.handleInvoicePaymentPaid(ctx, obj)
	case "checkout.session.completed":
		return s.handleCheckoutSessionCompleted(ctx, obj)
	case "checkout.session.async_payment_succeeded":
		return s.handleCheckoutSessionCompleted(ctx, obj)
	case "checkout.session.async_payment_failed":
		return s.handleCheckoutSessionAsyncPaymentFailed(ctx, obj)
	case "checkout.session.expired":
		return s.handleCheckoutSessionExpired(ctx, obj)
	case "refund.created", "refund.updated":
		return s.handleRefund(ctx, obj)
	case "charge.succeeded":
		return s.handleChargeSucceeded(ctx, obj)
	case "payment_method.attached":
		return s.handlePaymentMethodAttached(ctx, evt.ID, obj)
	case "payment_method.detached":
		return s.handlePaymentMethodDetached(ctx, obj, evt.Data.PreviousAttributes)
	case "payment_method.automatically_updated":
		return s.handlePaymentMethodAutomaticallyUpdated(ctx, evt.ID, obj)
	case "payment_method.updated":
		return s.handlePaymentMethodUpdated(ctx, evt.ID, obj)
	case "customer.updated":
		return s.handleCustomerUpdated(ctx, obj)
	case "charge.refunded":
		return s.handleChargeRefunded(ctx, obj)
	case "charge.dispute.created", "charge.dispute.updated", "charge.dispute.funds_withdrawn", "charge.dispute.closed":
		return s.handleDispute(ctx, eventType, obj)
	case "radar.early_fraud_warning.created", "radar.early_fraud_warning.updated":
		return s.handleEarlyFraudWarning(ctx, obj)
	// Catalog events are wake-up signals too: the observe job reads the
	// object by id, so a deleted one reads as gone.
	case "product.created", "product.updated", "product.deleted":
		return s.markCatalogObjectDirty(ctx, eventType, "product", obj)
	case "price.created", "price.updated", "price.deleted":
		return s.markCatalogObjectDirty(ctx, eventType, "price", obj)
	// A payout's notice queues its read; the books read the payout itself.
	case "payout.created", "payout.updated", "payout.paid", "payout.failed", "payout.canceled", "payout.reconciliation_completed":
		return s.queuePayoutRead(ctx, eventType, obj)
	default:
		s.Log.InfoContext(ctx, "stripe webhook ignored (not handled)", "event_type", eventType)
		return nil
	}
}

// markDirtyFromInvoice extracts the subscription identity from an invoice
// event and enqueues fetch-and-converge. Invoices without a subscription
// (one-off invoices) carry no subscription state — ignored.
func (s *StripeWebhookService) markDirtyFromInvoice(ctx context.Context, eventType string, obj json.RawMessage, evtCreated int64) error {
	var inv stripeInvoice
	if err := json.Unmarshal(obj, &inv); err != nil {
		return fmt.Errorf("parse invoice: %w", err)
	}
	railSubID := inv.railSubscriptionID()
	if railSubID == "" {
		s.Log.InfoContext(ctx, "stripe invoice event without subscription id; nothing to converge", "event_type", eventType, "invoice_id", strings.TrimSpace(inv.ID))
		return nil
	}
	return markSubscriptionDirty(ctx, s.Log, s.ConvergeEnqueuer, string(models.RailStripe), railSubID, eventType, evtCreated)
}

// markDirtyFromSubscription extracts the subscription id from a
// customer.subscription.* event and enqueues fetch-and-converge.
func (s *StripeWebhookService) markDirtyFromSubscription(ctx context.Context, eventType string, obj json.RawMessage, evtCreated int64) error {
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(obj, &data); err != nil {
		return fmt.Errorf("parse subscription event: %w", err)
	}
	if strings.TrimSpace(data.ID) == "" {
		return nil
	}
	return markSubscriptionDirty(ctx, s.Log, s.ConvergeEnqueuer, string(models.RailStripe), data.ID, eventType, evtCreated)
}

// handleChargeSucceeded snapshots the charged card onto the matching payment
// row. It then treats the charge as a wake-up signal and fetches Stripe's
// current defaults; the charged method itself does not imply "default".
func (s *StripeWebhookService) handleChargeSucceeded(ctx context.Context, obj json.RawMessage) error {
	var charge stripeCharge
	if err := json.Unmarshal(obj, &charge); err != nil {
		return fmt.Errorf("parse charge: %w", err)
	}
	card := payments.NormalizeStripeCard(charge.PaymentMethodDetails.Card)
	if card != nil {
		txnIDs := stripeChargeSnapshotTransactionIDs(charge)
		if err := payments.SnapshotPaymentCard(ctx, s.DB, txnIDs, card); err != nil {
			return err
		}
	}
	return s.reconcileStripeCustomerPaymentStateInTx(ctx, charge.Customer)
}

func stripeChargeSnapshotTransactionIDs(charge stripeCharge) []string {
	// Per-payment snapshot: match by charge id, payment_intent, or invoice id.
	// Preview invoice.paid events can create the payment row keyed by invoice.id.
	txnIDs := make([]string, 0, 3)
	if id := strings.TrimSpace(charge.ID); id != "" {
		txnIDs = append(txnIDs, id)
	}
	if pi := strings.TrimSpace(charge.PaymentIntent); pi != "" {
		txnIDs = append(txnIDs, pi)
	}
	if invoiceID := strings.TrimSpace(charge.Invoice); invoiceID != "" {
		txnIDs = append(txnIDs, invoiceID)
	}
	return txnIDs
}

func (s *StripeWebhookService) handleInvoicePaymentPaid(ctx context.Context, obj json.RawMessage) error {
	var invoicePayment stripeInvoicePayment
	if err := json.Unmarshal(obj, &invoicePayment); err != nil {
		return fmt.Errorf("parse stripe invoice_payment: %w", err)
	}
	if err := payments.LinkStripeInvoicePayment(ctx, s.DB, invoicePayment.Invoice, invoicePayment.Payment.Charge, invoicePayment.Payment.PaymentIntent); err != nil {
		return err
	}
	return nil
}

// handlePaymentMethodAttached mirrors an attached method only after fetching
// its current Stripe state, then converges subscription links from provider
// defaults. The event payload never decides which card is current.
func (s *StripeWebhookService) handlePaymentMethodAttached(ctx context.Context, eventID string, obj json.RawMessage) error {
	return s.foldPaymentMethod(ctx, eventID, obj, paymentmethods.SourceProviderRead)
}

// handlePaymentMethodAutomaticallyUpdated folds Stripe's card updater: the
// same method takes the card Stripe now holds, read from Stripe rather than
// the payload, as a card version. A same-brand reissue keeps billing and wakes
// members waiting on the card; another brand holds the card's mandates until
// the customer verifies it, and its members are asked to act.
func (s *StripeWebhookService) handlePaymentMethodAutomaticallyUpdated(ctx context.Context, eventID string, obj json.RawMessage) error {
	return s.foldPaymentMethod(ctx, eventID, obj, paymentmethods.SourceStripeUpdater)
}

// handlePaymentMethodUpdated folds a change made through Stripe's API (an
// expiry edit, a billing portal) the same way.
func (s *StripeWebhookService) handlePaymentMethodUpdated(ctx context.Context, eventID string, obj json.RawMessage) error {
	return s.foldPaymentMethod(ctx, eventID, obj, paymentmethods.SourceProviderRead)
}

func (s *StripeWebhookService) foldPaymentMethod(ctx context.Context, eventID string, obj json.RawMessage, source paymentmethods.CardSource) error {
	var pm stripePaymentMethod
	if err := json.Unmarshal(obj, &pm); err != nil {
		return fmt.Errorf("parse payment_method: %w", err)
	}

	err := s.withStripePaymentStateTx(ctx, pm.Customer, pm.ID, func(txdb *db.DB, tx pgx.Tx) error {
		_, life, err := payments.MirrorAttachedStripePaymentMethod(ctx, txdb, payments.NewRailCustomerService(txdb), s.Clock, s.StripePaymentState, pm.ID, source, eventID)
		if err != nil {
			return fmt.Errorf("mirror stripe payment method: %w", err)
		}

		if life.Change != "" {
			if s.SubscriptionLifecycleService == nil {
				return errors.New("stripe card change: no lifecycle service wired")
			}
			if _, err = s.SubscriptionLifecycleService.FollowCardChange(ctx, txdb, life, s.now()); err != nil {
				return err
			}
		}
		if err := s.convergeStripeCustomerPaymentState(ctx, txdb, pm.Customer); err != nil {
			return err
		}
		return MarkWebhookProcessedInTx(ctx, tx)
	})
	return err
}

func (s *StripeWebhookService) handlePaymentMethodDetached(ctx context.Context, obj, previous json.RawMessage) error {
	var pm stripePaymentMethod
	if err := json.Unmarshal(obj, &pm); err != nil {
		return fmt.Errorf("parse detached payment_method: %w", err)
	}
	customerID := strings.TrimSpace(pm.Customer)
	if customerID == "" && len(previous) > 0 {
		var prior struct {
			Customer string `json:"customer"`
		}
		if err := json.Unmarshal(previous, &prior); err != nil {
			return fmt.Errorf("parse detached payment_method previous attributes: %w", err)
		}
		customerID = strings.TrimSpace(prior.Customer)
	}
	return s.withStripePaymentStateTx(ctx, customerID, pm.ID, func(txdb *db.DB, tx pgx.Tx) error {
		if s.StripePaymentState == nil {
			return errors.New("stripe payment state reader is not configured")
		}
		truth, err := s.StripePaymentState.PaymentMethod(ctx, pm.ID)
		if err != nil && !errors.Is(err, payments.ErrStripeObjectNotFound) {
			return fmt.Errorf("read current stripe method before detachment: %w", err)
		}
		if truth != nil && truth.ID != pm.ID {
			return errors.New("stripe method read returned another instrument")
		}
		if truth != nil && strings.TrimSpace(truth.CustomerID) != "" {
			// An old detach event cannot override current attached provider
			// truth. Preserve the method and converge the provider's current
			// defaults without creating a detached finding.
			if _, _, err := payments.UpsertStripeCardForCustomer(ctx, txdb, payments.NewRailCustomerService(txdb), s.Clock, truth.CustomerID, truth); err != nil {
				return err
			}
			if err := s.convergeStripeCustomerPaymentState(ctx, txdb, truth.CustomerID); err != nil {
				return err
			}
			return MarkWebhookProcessedInTx(ctx, tx)
		}
		method, err := payments.RemoveDetachedStripePaymentMethod(ctx, txdb, pm.ID)
		if err != nil {
			return err
		}
		if method != nil {
			if err := recordStripeDetachedPaymentMethodFinding(ctx, txdb, method); err != nil {
				return err
			}
			if customerID == "" {
				customerID, err = payments.NewRailCustomerService(txdb).GetAccountIDForPSP(
					ctx, method.CustomerID, string(models.RailStripe),
				)
				if db.IsNotFound(err) {
					customerID = ""
				} else if err != nil {
					return fmt.Errorf("resolve detached stripe method customer: %w", err)
				}
				if customerID != "" {
					merchantID, err := merchant.Require(ctx)
					if err != nil {
						return err
					}
					if err := gen.New(tx).LockStripePaymentState(
						ctx,
						stripePaymentStateLockKey(
							merchantID.UUID(),
							method.HoldingPSP(),
							stripeCustomerLockSubject,
							customerID,
						),
					); err != nil {
						return fmt.Errorf("lock detached stripe customer reconciliation: %w", err)
					}
				}
			}
		}
		if err := s.convergeStripeCustomerPaymentState(ctx, txdb, customerID); err != nil {
			return err
		}
		return MarkWebhookProcessedInTx(ctx, tx)
	})
}

func (s *StripeWebhookService) handleCustomerUpdated(ctx context.Context, obj json.RawMessage) error {
	var customer struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(obj, &customer); err != nil {
		return fmt.Errorf("parse stripe customer: %w", err)
	}
	return s.reconcileStripeCustomerPaymentStateInTx(ctx, customer.ID)
}

func (s *StripeWebhookService) reconcileStripePaymentStateFromSubscription(ctx context.Context, obj json.RawMessage) error {
	var subscription struct {
		Customer string `json:"customer"`
	}
	if err := json.Unmarshal(obj, &subscription); err != nil {
		return fmt.Errorf("parse stripe subscription payment state: %w", err)
	}
	return s.reconcileStripeCustomerPaymentStateInTx(ctx, subscription.Customer)
}

func (s *StripeWebhookService) reconcileStripeCustomerPaymentStateInTx(ctx context.Context, customerID string) error {
	if strings.TrimSpace(customerID) == "" {
		return nil
	}
	return s.withStripePaymentStateTx(ctx, customerID, "", func(txdb *db.DB, tx pgx.Tx) error {
		if err := s.convergeStripeCustomerPaymentState(ctx, txdb, customerID); err != nil {
			return err
		}
		return MarkWebhookProcessedInTx(ctx, tx)
	})
}

func (s *StripeWebhookService) convergeStripeCustomerPaymentState(ctx context.Context, txdb *db.DB, customerID string) error {
	customerID = strings.TrimSpace(customerID)
	if customerID == "" {
		return nil
	}
	err := payments.ConvergeStripeCustomerPaymentState(
		ctx,
		txdb,
		payments.NewRailCustomerService(txdb),
		s.Clock,
		s.StripePaymentState,
		customerID,
	)
	if errors.Is(err, payments.ErrStripeObjectNotFound) {
		s.Log.InfoContext(ctx, "stripe payment-method reconciliation skipped unknown customer", "stripe_customer_id", customerID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("converge stripe customer payment state: %w", err)
	}
	return nil
}

func (s *StripeWebhookService) withStripePaymentStateTx(
	ctx context.Context,
	customerID string,
	paymentMethodID string,
	fn func(*db.DB, pgx.Tx) error,
) error {
	if s.DB == nil {
		return errors.New("stripe payment-method reconciliation database is not configured")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("stripe payment-method reconciliation requires merchant scope: %w", err)
	}
	pspID, err := db.RequirePSPID(ctx)
	if err != nil {
		return fmt.Errorf("stripe payment-method reconciliation requires PSP scope: %w", err)
	}
	subjectKind := stripeCustomerLockSubject
	subjectID := strings.TrimSpace(customerID)
	if subjectID == "" {
		subjectKind = stripePaymentMethodLockSubject
		subjectID = strings.TrimSpace(paymentMethodID)
	}
	if subjectID == "" {
		return errors.New("stripe payment-method reconciliation requires a customer or payment method")
	}
	lockKey := stripePaymentStateLockKey(
		merchantID.UUID(),
		pspID,
		subjectKind,
		subjectID,
	)
	return s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := gen.New(tx).LockStripePaymentState(ctx, lockKey); err != nil {
			return fmt.Errorf("lock stripe payment-method reconciliation: %w", err)
		}
		return fn(s.DB.NewWithPgxTx(tx), tx)
	})
}

type stripePaymentStateLockSubject string

const (
	stripeCustomerLockSubject      stripePaymentStateLockSubject = "customer"
	stripePaymentMethodLockSubject stripePaymentStateLockSubject = "method"
)

func stripePaymentStateLockKey(
	merchantID uuid.UUID,
	pspID uuid.UUID,
	subject stripePaymentStateLockSubject,
	subjectID string,
) string {
	return "payments:stripe:" +
		merchantID.String() + ":" +
		pspID.String() + ":" +
		string(subject) + ":" +
		strings.TrimSpace(subjectID)
}

func recordStripeDetachedPaymentMethodFinding(ctx context.Context, database *db.DB, method *models.PaymentMethod) error {
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	evidence, err := json.Marshal(map[string]any{
		"provider":          string(models.RailStripe),
		"psp_id":            method.HoldingPSP().String(),
		"payment_method_id": method.ID.String(),
		"rail_method_ref":   method.RailMethodRef,
		"customer_id":       method.CustomerID.String(),
	})
	if err != nil {
		return fmt.Errorf("marshal detached stripe payment-method finding: %w", err)
	}
	action := "Collect a replacement payment method in Stripe and verify the subscription default before resuming collection."
	if _, err := database.Gen(ctx).UpsertReconciliationFinding(ctx, gen.UpsertReconciliationFindingParams{
		MerchantID:        merchantID.UUID(),
		FindingType:       "consistency.stripe_payment_method_detached",
		SubjectKey:        method.HoldingPSP().String() + ":" + method.RailMethodRef,
		Severity:          "high",
		Status:            "requires_review",
		RecommendedAction: &action,
		Evidence:          evidence,
	}); err != nil {
		return fmt.Errorf("record detached stripe payment-method finding: %w", err)
	}
	return nil
}

func zeroTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (s *StripeWebhookService) handleCheckoutSessionExpired(ctx context.Context, obj json.RawMessage) error {
	if s.InvoiceAttemptService == nil {
		return nil
	}

	var sess stripeCheckoutSession
	if err := json.Unmarshal(obj, &sess); err != nil {
		return fmt.Errorf("parse checkout session: %w", err)
	}
	if invoicePayment(sess) {
		return s.InvoiceAttemptService.CloseInvoiceHosted(ctx, parseInvoiceAttemptID(sess.Metadata), false)
	}

	return nil
}

func (s *StripeWebhookService) handleCheckoutSessionCompleted(ctx context.Context, obj json.RawMessage) error {
	var sess stripeCheckoutSession
	if err := json.Unmarshal(obj, &sess); err != nil {
		return fmt.Errorf("parse checkout session: %w", err)
	}
	userID := normalize.FirstNonEmpty(sess.Metadata["user_id"], sess.Metadata["userId"], sess.Metadata["uid"])
	if userID == "" {
		return fmt.Errorf("stripe checkout missing user_id metadata")
	}
	if s.RailCustomerService != nil {
		customerID := strings.TrimSpace(sess.Customer)
		if customerID != "" {
			_ = s.RailCustomerService.Upsert(ctx, userID, string(models.RailStripe), customerID)
		}
	}
	// Its subscription may have converged before the customer was linked.
	if subscription := strings.TrimSpace(sess.Subscription); subscription != "" {
		if err := markSubscriptionDirty(ctx, s.Log, s.ConvergeEnqueuer, string(models.RailStripe), subscription, "checkout.session.completed", 0); err != nil {
			return err
		}
	}
	if sess.Mode != "payment" {
		return nil
	}
	paymentStatus := strings.TrimSpace(sess.PaymentStatus)
	paymentSettled := paymentStatus == "paid" || (sess.AmountTotal == 0 && paymentStatus == "no_payment_required")
	if !paymentSettled || strings.TrimSpace(sess.Status) != "complete" {
		s.Log.InfoContext(ctx, "Stripe checkout session not paid; waiting for async payment success", "checkout_session_id", sess.ID, "status", sess.Status, "payment_status", sess.PaymentStatus)
		return nil
	}
	if invoicePayment(sess) {
		if s.InvoiceAttemptService == nil {
			return errors.New("invoice purchase settlement unavailable")
		}
		return s.InvoiceAttemptService.SettleInvoiceHosted(ctx, parseInvoiceAttemptID(sess.Metadata), sess.ID, sess.PaymentIntent, sess.Currency, sess.AmountTotal)
	}
	return nil
}

func (s *StripeWebhookService) handleCheckoutSessionAsyncPaymentFailed(ctx context.Context, obj json.RawMessage) error {
	var sess stripeCheckoutSession
	if err := json.Unmarshal(obj, &sess); err != nil {
		return fmt.Errorf("parse checkout session: %w", err)
	}
	if s.InvoiceAttemptService == nil {
		return nil
	}
	sessionID := parseInvoiceAttemptID(sess.Metadata)
	if sessionID == uuid.Nil {
		return nil
	}
	if invoicePayment(sess) {
		return s.InvoiceAttemptService.CloseInvoiceHosted(ctx, sessionID, true)
	}
	return nil
}

// invoicePayment reports a Checkout Session that pays an invoice's attempt.
func invoicePayment(sess stripeCheckoutSession) bool {
	return strings.TrimSpace(sess.Metadata["order_id"]) != "" && parseInvoiceAttemptID(sess.Metadata) != uuid.Nil
}

func parseInvoiceAttemptID(metadata map[string]string) uuid.UUID {
	if metadata == nil {
		return uuid.Nil
	}
	id, _ := providerAttemptID(metadata["order_attempt_id"])
	return id
}

func (s *StripeWebhookService) handleRefund(ctx context.Context, obj json.RawMessage) error {
	var refund stripeRefund
	if err := json.Unmarshal(obj, &refund); err != nil {
		return fmt.Errorf("parse stripe refund: %w", err)
	}
	return s.recordStripeRefund(ctx, refund)
}

func (s *StripeWebhookService) handleChargeRefunded(ctx context.Context, obj json.RawMessage) error {
	var charge stripeCharge
	if err := json.Unmarshal(obj, &charge); err != nil {
		return fmt.Errorf("parse stripe charge: %w", err)
	}
	if len(charge.Refunds.Data) == 0 {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe charge.refunded missing refund data"))
	}
	for _, refund := range charge.Refunds.Data {
		if strings.TrimSpace(refund.Status) == "" {
			// Stripe always sends a refund status; empty is malformed. Never
			// assume "succeeded": reject as non-retryable.
			return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe charge.refunded missing refund status (charge %s)", charge.ID))
		}
		if strings.TrimSpace(refund.Charge) == "" {
			refund.Charge = charge.ID
		}
		if strings.TrimSpace(refund.PaymentIntent) == "" {
			refund.PaymentIntent = charge.PaymentIntent
		}
		if refund.Metadata[booktag.StripeKey] == "" {
			refund.Metadata = charge.Metadata
		}
		if err := s.recordStripeRefund(ctx, refund); err != nil {
			return err
		}
	}
	return nil
}

func (s *StripeWebhookService) recordStripeRefund(ctx context.Context, refund stripeRefund) error {
	if s.PaymentService == nil {
		return fmt.Errorf("payment service is required for stripe refund")
	}
	if !stripeRefundSucceeded(refund.Status) {
		s.Log.InfoContext(ctx, "Stripe refund is not succeeded; skipping ledger and entitlement changes", "refund_id", refund.ID, "status", refund.Status)
		return nil
	}
	refundID := strings.TrimSpace(refund.ID)
	if refundID == "" || refund.Amount <= 0 {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe refund missing id or amount"))
	}
	var original *models.Payment
	if existing, err := s.PaymentService.GetByPSPTransactionID(ctx, models.RailStripe, refundID); err == nil && existing != nil {
		if existing.RefundedPaymentID == nil || refundDecidedByOpenRails(existing) {
			return nil
		}
		original, err = s.PaymentService.GetByID(ctx, *existing.RefundedPaymentID)
		if err != nil {
			return fmt.Errorf("lookup original stripe payment for existing refund: %w", err)
		}
	} else if err != nil && !db.IsNotFound(err) {
		return fmt.Errorf("lookup stripe refund payment: %w", err)
	}
	if original == nil {
		var err error
		original, err = s.lookupStripeOriginalPayment(ctx, refund.Charge, refund.PaymentIntent)
		if errors.Is(err, errStripeOriginalUnknown) && quietUnknownObject(ctx, s.Log, s.DB, refund.Metadata[booktag.StripeKey], refundID) {
			return nil
		}
		if err != nil {
			return err
		}
		currency := strings.ToUpper(strings.TrimSpace(refund.Currency))
		if currency == "" {
			currency = original.Currency
		}
		amount, err := moneyutil.RailMinorToNative(currency, moneyutil.Cents(refund.Amount))
		if err != nil {
			return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe refund %s amount: %w", refundID, err))
		}
		var at time.Time
		if refund.Created > 0 {
			at = time.Unix(refund.Created, 0).UTC()
		}
		// An RDR resolution's refund a prevented case booked already is this
		// refund, unless OpenRails asked for it: the case takes it once.
		absorbed, err := s.disputes().RefundAbsorbed(ctx, s.DB, original.ID, refundID)
		if err == nil && !absorbed && refund.Metadata[subscriptions.StripeRefundMetadataKey] == "" {
			absorbed, err = s.disputes().AbsorbByPrevention(ctx, s.DB, original.ID, refundID, amount, currency, s.atOrNow(at).Add(rdrRefundLead))
		}
		if err != nil {
			return err
		}
		if !absorbed {
			if _, err := s.PaymentService.RefundObservedWithReason(ctx, original.ID, refundID, amount, stripeRefundReason(refund.Reason), at); err != nil {
				return fmt.Errorf("record stripe refund: %w", err)
			}
		}
	}
	return s.PaymentService.ApplyRefundAccessPolicy(ctx, original.ID)
}

func (s *StripeWebhookService) disputes() *disputes.Service {
	if s.Disputes != nil {
		return s.Disputes
	}
	return disputes.New(s.DB, s.Clock)
}

// handleDispute records the case each dispute event reports, then acts on the
// kind it reports: a chargeback reverses its charge and ends what it paid
// for; an inquiry, an unknown status or a CE 3.0 block moves no money; an RDR
// resolution is a refund. A won dispute returns the money, and stays won
// against any later notice. A dispute on a charge OpenRails has no record of
// is recorded and raised for staff, never retried.
func (s *StripeWebhookService) handleDispute(ctx context.Context, eventType string, obj json.RawMessage) error {
	var dispute disputes.StripeDispute
	if err := json.Unmarshal(obj, &dispute); err != nil {
		return fmt.Errorf("parse stripe dispute: %w", err)
	}
	if s.PaymentService == nil {
		return fmt.Errorf("payment service is required for stripe dispute")
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return fmt.Errorf("record stripe dispute: %w", err)
	}
	original, err := disputes.StripeCharge(ctx, s.DB, dispute.Charge, dispute.PaymentIntent)
	if err != nil {
		return err
	}
	report, err := dispute.Report(psp, original, s.now())
	if err != nil {
		return MarkWebhookErrorNonRetryable(err)
	}
	disputeID := report.ProviderRef
	rec, err := s.disputes().Record(ctx, s.DB, report)
	if err != nil {
		return err
	}
	if original == nil {
		return s.disputes().RaiseUnknownCharge(ctx, s.DB, psp, disputeID, strings.TrimSpace(dispute.Charge+" "+dispute.PaymentIntent))
	}
	if rec.Status == string(billing.DisputeWon) {
		return s.handleStripeDisputeWon(ctx, dispute, rec)
	}
	switch report.Kind {
	case billing.DisputeChargeback:
	case billing.DisputePrevented:
		if !disputerules.StripePreventionRefunds(dispute.PaymentMethodDetails.Card.CaseType) || rec.ReversalPaymentID != nil {
			return nil
		}
		if err := s.preventionRefund(ctx, original, rec, report.Amount, report.Currency); err != nil {
			return fmt.Errorf("record stripe dispute resolution: %w", err)
		}
		return nil
	default:
		s.Log.InfoContext(ctx, "Stripe dispute recorded; nothing reversed or ended", "dispute_id", disputeID, "event_type", eventType, "status", dispute.Status, "kind", report.Kind)
		return nil
	}
	var ledgerErr error
	reversal, err := s.PaymentService.GetByPSPTransactionID(ctx, models.RailStripe, disputeID)
	if err != nil && !db.IsNotFound(err) {
		return fmt.Errorf("lookup stripe dispute payment: %w", err)
	}
	if reversal == nil {
		if reversal, err = s.PaymentService.Refund(ctx, original.ID, disputeID, report.Amount, payments.ReversalChargeback); err != nil {
			reversal, ledgerErr = nil, fmt.Errorf("record stripe dispute reversal: %w", err)
			s.Log.ErrorContext(ctx, "Failed to record Stripe dispute reversal; continuing entitlement revocation", logx.Err(ledgerErr), "dispute_id", disputeID, "original_payment_id", original.ID)
		}
	}
	if reversal != nil && rec.ReversalPaymentID == nil {
		if err := s.disputes().Link(ctx, s.DB, rec, &reversal.ID, nil); err != nil {
			return err
		}
	}
	cause := fmt.Sprintf("STRIPE DISPUTE %s: %s status=%s", disputeID, strings.TrimSpace(dispute.Reason), strings.TrimSpace(dispute.Status))
	if err := s.endDisputedCharge(ctx, original, func(sub *models.Subscription) string {
		// A membership already canceled is not this dispute's cancellation to undo.
		if sub.Status == models.StatusCanceled {
			return fmt.Sprintf("STRIPE DISPUTE %s after cancellation: %s status=%s", disputeID, strings.TrimSpace(dispute.Reason), strings.TrimSpace(dispute.Status))
		}
		return cause
	}); err != nil {
		return err
	}
	if ledgerErr != nil {
		originalID := original.ID
		if err := alerting.RecordLedgerRepair(ctx, s.DB, s.now(), alerting.LedgerRepair{
			Provider:          "stripe",
			Operation:         "dispute_reversal",
			TransactionID:     disputeID,
			CustomerID:        original.CustomerID.String(),
			OriginalPaymentID: &originalID,
			SubscriptionID:    original.SubscriptionID,
			Err:               ledgerErr,
			Metadata: map[string]any{
				"event_type": eventType,
				"status":     strings.TrimSpace(dispute.Status),
				"reason":     strings.TrimSpace(dispute.Reason),
			},
		}); err != nil {
			return fmt.Errorf("record stripe dispute ledger repair alert: %w", err)
		}
	}
	return nil
}

// rdrRefundLead is how long before its dispute object Stripe may make an RDR
// resolution's refund. Stripe's ordering is unconfirmed; one refund is
// booked whichever arrives first.
const rdrRefundLead = 5 * time.Minute

// preventionRefund books an RDR resolution's refund once. A Stripe refund of
// the whole charge made no earlier than a few minutes before the case, which
// OpenRails did not ask for and no case took yet, is the resolution's: the
// case absorbs it rather than booking another.
func (s *StripeWebhookService) preventionRefund(ctx context.Context, original *models.Payment, rec disputes.Case, amount int64, currency string) error {
	if existing, err := s.PaymentService.GetByPSPTransactionID(ctx, models.RailStripe, rec.ProviderRef); err == nil && existing != nil {
		return s.disputes().Link(ctx, s.DB, rec, &existing.ID, nil)
	} else if err != nil && !db.IsNotFound(err) {
		return err
	}
	_, refunds, err := s.PaymentService.GetByIDWithDetails(ctx, original.ID)
	if err != nil {
		return err
	}
	for _, r := range refunds {
		if r.ReversalKind == nil || *r.ReversalKind != payments.ReversalRefund || r.Status != payments.PaymentStatusSucceededValue ||
			-r.Amount != amount || !strings.EqualFold(r.Currency, currency) || r.PurchasedAt.Before(rec.DisputedAt.Add(-rdrRefundLead)) ||
			!strings.HasPrefix(r.TransactionID, "re_") || refundInitiatedByOpenRails(r) {
			continue
		}
		taken, err := s.disputes().RefundAbsorbed(ctx, s.DB, original.ID, r.TransactionID)
		if err != nil {
			return err
		}
		if !taken {
			return s.disputes().AbsorbRefund(ctx, s.DB, rec, r)
		}
	}
	refund, err := s.PaymentService.RefundWithReason(ctx, original.ID, rec.ProviderRef, amount, billing.RefundDisputePrevention)
	if err != nil {
		return err
	}
	return s.disputes().Link(ctx, s.DB, rec, &refund.ID, nil)
}

func (s *StripeWebhookService) atOrNow(at time.Time) time.Time {
	if at.IsZero() {
		return s.now()
	}
	return at
}

type stripeEarlyFraudWarning struct {
	ID            string `json:"id"`
	Charge        string `json:"charge"`
	PaymentIntent string `json:"payment_intent"`
	FraudType     string `json:"fraud_type"`
	Actionable    bool   `json:"actionable"`
	Created       int64  `json:"created"`
}

// handleEarlyFraudWarning records an issuer's fraud report (Visa TC40,
// Mastercard SAFE) once, from whichever event reports it. An actionable one
// queues its response: the charge refunded in full and what it paid for
// ended, as a chargeback would, unless the merchant records reports only or
// a dispute decides the charge. A report on a charge OpenRails has no record
// of is raised for staff, never retried.
func (s *StripeWebhookService) handleEarlyFraudWarning(ctx context.Context, obj json.RawMessage) error {
	var warning stripeEarlyFraudWarning
	if err := json.Unmarshal(obj, &warning); err != nil {
		return fmt.Errorf("parse stripe early fraud warning: %w", err)
	}
	ref := strings.TrimSpace(warning.ID)
	if ref == "" || s.PaymentService == nil || s.DB == nil {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe early fraud warning missing id"))
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return fmt.Errorf("record stripe early fraud warning: %w", err)
	}
	original, err := disputes.StripeCharge(ctx, s.DB, warning.Charge, warning.PaymentIntent)
	if err != nil {
		return err
	}
	if original == nil {
		return s.disputes().RaiseUnknownCharge(ctx, s.DB, psp, ref, strings.TrimSpace(warning.Charge+" "+warning.PaymentIntent))
	}
	report := disputes.FraudReport{PSPID: psp, Rail: models.RailStripe, ProviderRef: ref, Payment: original, FraudType: warning.FraudType, Actionable: warning.Actionable}
	if warning.Created > 0 {
		report.ReportedAt = time.Unix(warning.Created, 0).UTC()
	}
	_, _, err = s.disputes().RecordFraudReport(ctx, s.DB, report)
	return err
}

// endDisputedCharge ends what a charged-back charge paid for: its
// subscriptions at once with their access and a one-off's access. Its card is
// blocked by the chargeback's response, under its gates.
func (s *StripeWebhookService) endDisputedCharge(ctx context.Context, original *models.Payment, feedback func(*models.Subscription) string) error {
	if s.SubscriptionLifecycleService != nil && s.DB != nil {
		subscriptionIDs, err := paymentSubscriptionIDs(ctx, s.DB, original.ID)
		if err != nil {
			return err
		}
		for _, subscriptionID := range subscriptionIDs {
			if err := revokeStripeMembership(ctx, s.DB, s.SubscriptionLifecycleService, subscriptionID, models.CancelTypeChargeback, feedback, nil); err != nil {
				return fmt.Errorf("cancel subscription after stripe dispute: %w", err)
			}
		}
	}
	if original.SubscriptionID == nil && s.DB != nil {
		entSvc := entitlements.NewEntitlementService(s.DB, s.Clock)
		if err := entSvc.EndActiveByPayment(ctx, original.ID, models.AccessRevokeChargeback); err != nil {
			return fmt.Errorf("revoke product access after stripe dispute: %w", err)
		}
	}
	return nil
}

func (s *StripeWebhookService) handleStripeDisputeWon(ctx context.Context, dispute disputes.StripeDispute, rec disputes.Case) error {
	if s.PaymentService == nil {
		return nil
	}
	disputeID := strings.TrimSpace(dispute.ID)
	if disputeID == "" {
		return nil
	}
	disputeReversal, err := s.PaymentService.GetByPSPTransactionID(ctx, models.RailStripe, disputeID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("lookup stripe dispute reversal for won dispute: %w", err)
	}
	if disputeReversal == nil || disputeReversal.RefundedPaymentID == nil || disputeReversal.Amount >= 0 {
		return nil
	}

	recoveryID := "dispute_won:" + disputeID
	var existingRecovery *models.Payment
	if existing, err := s.PaymentService.GetByPSPTransactionID(ctx, models.RailStripe, recoveryID); err == nil && existing != nil {
		existingRecovery = existing
	} else if err != nil && !db.IsNotFound(err) {
		return fmt.Errorf("lookup stripe won dispute recovery payment: %w", err)
	}

	original, err := s.PaymentService.GetByID(ctx, *disputeReversal.RefundedPaymentID)
	if err != nil {
		return fmt.Errorf("lookup original stripe payment for won dispute: %w", err)
	}
	var recoveryPaymentID uuid.UUID
	if existingRecovery != nil {
		recoveryPaymentID = existingRecovery.ID
		if existingRecovery.RefundedPaymentID == nil {
			if err := s.PaymentService.LinkRefundedPayment(ctx, existingRecovery.ID, original.ID); err != nil {
				return fmt.Errorf("link existing stripe won dispute recovery payment: %w", err)
			}
		} else if *existingRecovery.RefundedPaymentID != original.ID {
			return fmt.Errorf("stripe won dispute recovery payment is linked to a different original payment")
		}
	} else {
		recovery := &models.Payment{
			ID:                uuidutil.NewV7(),
			CustomerID:        original.CustomerID,
			SubscriptionID:    original.SubscriptionID,
			InvoiceID:         original.InvoiceID,
			RefundedPaymentID: &original.ID,
			Rail:              original.Rail,
			TransactionID:     recoveryID,
			Metadata:          map[string]any{"reverses_payment_id": disputeReversal.ID.String()},
			Amount:            -disputeReversal.Amount,
			ListAmount:        original.ListAmount,
			Currency:          original.Currency,
			ReversalKind:      func() *string { k := payments.ReversalDisputeReversal; return &k }(),
			Status:            payments.PaymentStatusSucceededValue,
			MoneyMovement:     models.MoneyMovementRail, // Stripe returned the disputed funds.
			PurchasedAt:       s.now(),
			CreatedAt:         s.now(),
		}
		if err := s.PaymentService.Create(ctx, recovery); err != nil {
			return fmt.Errorf("record stripe won dispute recovery payment: %w", err)
		}
		recoveryPaymentID = recovery.ID
	}
	if err := s.disputes().Link(ctx, s.DB, rec, nil, &recoveryPaymentID); err != nil {
		return err
	}

	subscriptionIDs, err := paymentSubscriptionIDs(ctx, s.DB, original.ID)
	if err != nil {
		return err
	}
	for _, subscriptionID := range subscriptionIDs {
		if err := s.reactivateStripeSubscriptionAfterWonDispute(ctx, subscriptionID, original, disputeID); err != nil {
			return err
		}
	}
	if original.SubscriptionID == nil && s.DB != nil {
		s.Log.WarnContext(ctx, "Stripe dispute was won; nonrecurring access restoration requires manual review", "dispute_id", disputeID, "original_payment_id", original.ID)
	}
	return nil
}

func (s *StripeWebhookService) reactivateStripeSubscriptionAfterWonDispute(ctx context.Context, subscriptionID uuid.UUID, original *models.Payment, disputeID string) error {
	if s.SubscriptionService == nil || s.SubscriptionLifecycleService == nil || original == nil {
		return nil
	}
	sub, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("load stripe subscription for won dispute recovery: %w", err)
	}
	// Returning disputed funds restores accounting, not permission to restart an
	// engine-owned agreement. It has no native Stripe schedule to reactivate and
	// terminal customer/merchant/chargeback decisions must remain preserved.
	if sub.CollectionPolicy == models.CollectionPolicyEngine {
		return nil
	}
	if sub.Status != models.StatusCanceled {
		return nil
	}
	// Only the cancellation this dispute caused is undone; a user, merchant or
	// other chargeback cancellation stays terminal.
	if sub.CancelType == nil || *sub.CancelType != models.CancelTypeChargeback || sub.CancelFeedback == nil ||
		!strings.HasPrefix(*sub.CancelFeedback, "STRIPE DISPUTE "+disputeID+":") {
		s.Log.InfoContext(ctx, "Stripe dispute won; subscription was not canceled by this dispute and stays canceled", "subscription_id", sub.ID, "dispute_id", disputeID)
		return nil
	}
	now := s.now().UTC()
	var paidThrough time.Time
	if sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(now) {
		paidThrough = sub.CurrentPeriodEndsAt.UTC()
	} else if s.DB != nil && original.InvoiceID != nil {
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		invoice, err := s.DB.Gen(ctx).GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: mid.UUID(), ID: *original.InvoiceID})
		if err != nil {
			return fmt.Errorf("load won-dispute billing obligation: %w", err)
		}
		if invoice.CustomerID != original.CustomerID || invoice.SubscriptionID == nil || *invoice.SubscriptionID != sub.ID {
			return errors.New("won-dispute payment invoice belongs to another subscription")
		}
		if invoice.PeriodEndsAt != nil {
			paidThrough = invoice.PeriodEndsAt.UTC()
		}
	}
	if !paidThrough.After(now) {
		s.Log.WarnContext(ctx, "Stripe dispute was won but no future paid-through date exists; subscription restoration requires manual review", "subscription_id", sub.ID, "payment_id", original.ID)
		return nil
	}
	_, err = s.SubscriptionLifecycleService.ReactivateMembership(ctx, &subscriptions.ReactivateMembershipParams{
		Rail:                      models.RailStripe,
		RailSubscriptionID:        sub.RailSubscriptionID,
		CurrentPeriodEndsAt:       &paidThrough,
		AllowTerminalReactivation: true,
	})
	if err != nil {
		return fmt.Errorf("reactivate stripe subscription after won dispute: %w", err)
	}
	return nil
}

// stripeRefundReason is the reason Stripe gives a refund in OpenRails' terms.
func stripeRefundReason(reason string) billing.RefundReason {
	switch strings.TrimSpace(reason) {
	case "duplicate":
		return billing.RefundDuplicate
	case "fraudulent":
		return billing.RefundFraudulent
	case "requested_by_customer":
		return billing.RefundRequestedByCustomer
	case "":
		return billing.RefundUnknown
	}
	return billing.RefundOther
}

func stripeRefundSucceeded(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "succeeded")
}

var errStripeOriginalUnknown = errors.New("stripe original payment not found for charge/payment_intent")

func (s *StripeWebhookService) lookupStripeOriginalPayment(ctx context.Context, chargeID, paymentIntentID string) (*models.Payment, error) {
	for _, candidate := range []string{strings.TrimSpace(chargeID), strings.TrimSpace(paymentIntentID)} {
		if candidate == "" {
			continue
		}
		payment, err := s.PaymentService.GetByPSPTransactionID(ctx, models.RailStripe, candidate)
		if err == nil {
			return payment, nil
		}
		if !db.IsNotFound(err) {
			return nil, fmt.Errorf("lookup stripe original payment: %w", err)
		}
	}
	return nil, errStripeOriginalUnknown
}
