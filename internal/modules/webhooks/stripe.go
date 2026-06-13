package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/db/repo"
	"github.com/open-rails/openrails/internal/modules/analytics"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/productaccess"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/normalize"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/api"
	log "github.com/sirupsen/logrus"
)

type stripePurchaseRegistrar interface {
	RegisterPurchase(ctx context.Context, req *payments.RegisterPurchaseRequest) (*payments.RegisterPurchaseResponse, error)
}

type StripeWebhookService struct {
	DB                           *db.DB
	PriceService                 *catalog.PriceService
	ProductService               *catalog.ProductService
	SubscriptionService          *subscriptions.SubscriptionService
	SubscriptionLifecycleService *subscriptions.SubscriptionLifecycleService
	NotificationService          *subscriptions.NotificationService
	PurchaseRegistrar            stripePurchaseRegistrar
	PaymentService               *payments.PaymentService
	EventLogService              *analytics.EventLogService
	CreditsService               *credits.CreditsService
	DeduplicationService         *DeduplicationService
	ProcessorCustomerService     *payments.ProcessorCustomerService
	CheckoutSessionService       webhookCheckoutSessionStore
	Clock                        clockwork.Clock
}

func (s *StripeWebhookService) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now()
}

type stripeEvent struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Created int64           `json:"created"`
	Data    stripeEventData `json:"data"`
}

type stripeEventData struct {
	Object json.RawMessage `json:"object"`
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

// priceID returns the Stripe price id for the line item, tolerating both the
// legacy `price.id` shape and the preview `pricing.price_details.price` shape.
func (li stripeInvoiceLineItem) priceID() string {
	if id := strings.TrimSpace(li.Price.ID); id != "" {
		return id
	}
	return strings.TrimSpace(li.Pricing.PriceDetails.Price)
}

type stripeInvoice struct {
	ID            string `json:"id"`
	Subscription  string `json:"subscription"`
	Customer      string `json:"customer"`
	CustomerEmail string `json:"customer_email"`
	PaymentIntent string `json:"payment_intent"`
	Charge        string `json:"charge"`
	AmountPaid    int64  `json:"amount_paid"`
	AmountDue     int64  `json:"amount_due"`
	Currency      string `json:"currency"`
	// BillingReason is Stripe's reason for the invoice ("subscription_create",
	// "subscription_cycle", "subscription_update", ...). "subscription_update"
	// marks a mid-cycle change (a Model B upgrade) whose total is a PRORATED
	// amount rather than the plan's list price; validateStripeInvoicePrice keys
	// off it to skip the exact list-price match.
	BillingReason string            `json:"billing_reason"`
	Metadata      map[string]string `json:"metadata"`
	Lines         struct {
		Data []stripeInvoiceLineItem `json:"data"`
	} `json:"lines"`
	SubscriptionDetails struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"subscription_details"`
	// The 2026-04-22.preview API moved subscription details (including the
	// metadata we propagate via subscription_data.metadata, and the subscription
	// id itself) under `parent`. The top-level `subscription`, `charge`, and
	// `payment_intent` fields are no longer present on preview invoice events.
	Parent struct {
		SubscriptionDetails struct {
			Subscription string            `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
		} `json:"subscription_details"`
	} `json:"parent"`
}

// processorSubscriptionID returns the Stripe subscription id, reading the
// top-level field (classic/snapshot shape) and falling back to
// parent.subscription_details.subscription (2026-04-22.preview shape).
func (inv stripeInvoice) processorSubscriptionID() string {
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
}

type stripeCharge struct {
	ID             string `json:"id"`
	PaymentIntent  string `json:"payment_intent"`
	Customer       string `json:"customer"`
	Invoice        string `json:"invoice"`
	PaymentMethod  string `json:"payment_method"`
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Currency       string `json:"currency"`
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

// stripePaymentMethod is the payment_method.attached payload; card details live
// directly under `card`.
type stripePaymentMethod struct {
	ID       string                     `json:"id"`
	Customer string                     `json:"customer"`
	Card     payments.StripeCardDetails `json:"card"`
}

type stripeDispute struct {
	ID            string `json:"id"`
	Charge        string `json:"charge"`
	PaymentIntent string `json:"payment_intent"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	Reason        string `json:"reason"`
}

func (s *StripeWebhookService) HandleStripeWebhook(ctx context.Context, payload []byte) error {
	var evt stripeEvent
	if err := json.Unmarshal(payload, &evt); err != nil {
		return fmt.Errorf("parse stripe event: %w", err)
	}

	eventID := strings.TrimSpace(evt.ID)
	eventType := strings.TrimSpace(evt.Type)
	if eventID == "" || eventType == "" {
		return fmt.Errorf("stripe event missing id or type")
	}

	if s.DeduplicationService != nil {
		return s.DeduplicationService.ProcessWebhook(ctx, eventID, eventType, models.ProcessorStripe, evt, func(ctx context.Context) error {
			return s.handleEvent(ctx, eventType, evt.Data.Object)
		})
	}
	return s.handleEvent(ctx, eventType, evt.Data.Object)
}

func (s *StripeWebhookService) handleEvent(ctx context.Context, eventType string, obj json.RawMessage) error {
	switch eventType {
	case "invoice.paid":
		return s.handleInvoicePaid(ctx, obj)
	case "invoice.payment_failed":
		return s.handleInvoicePaymentFailed(ctx, obj)
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
	case "customer.subscription.updated":
		return s.handleSubscriptionUpdated(ctx, obj)
	case "customer.subscription.deleted":
		return s.handleSubscriptionDeleted(ctx, obj)
	case "refund.created", "refund.updated":
		return s.handleRefund(ctx, obj)
	case "charge.succeeded":
		return s.handleChargeSucceeded(ctx, obj)
	case "payment_method.attached":
		return s.handlePaymentMethodAttached(ctx, obj)
	case "charge.refunded":
		return s.handleChargeRefunded(ctx, obj)
	case "charge.dispute.created", "charge.dispute.closed":
		return s.handleDispute(ctx, eventType, obj)
	default:
		log.WithField("event_type", eventType).Info("stripe webhook ignored (not handled)")
		return nil
	}
}

// handleChargeSucceeded snapshots the charged card onto the matching payment row
// (per-payment history) and records it as the subscription's current card. The
// charge payload embeds payment_method_details.card, so no Stripe fetch is needed.
func (s *StripeWebhookService) handleChargeSucceeded(ctx context.Context, obj json.RawMessage) error {
	var charge stripeCharge
	if err := json.Unmarshal(obj, &charge); err != nil {
		return fmt.Errorf("parse charge: %w", err)
	}
	card := payments.NormalizeStripeCard(charge.PaymentMethodDetails.Card)
	if card == nil {
		return nil
	}

	txnIDs := stripeChargeSnapshotTransactionIDs(charge)
	if err := payments.SnapshotPaymentCard(ctx, s.DB, txnIDs, card); err != nil {
		return err
	}

	// Current card for the active subscription.
	return s.recordStripeCardForCustomer(ctx, charge.Customer, charge.PaymentMethod, charge.ID, card)
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

// handlePaymentMethodAttached records a newly attached card as the customer's
// current payment method (e.g. when the user updates their card in the portal).
func (s *StripeWebhookService) handlePaymentMethodAttached(ctx context.Context, obj json.RawMessage) error {
	var pm stripePaymentMethod
	if err := json.Unmarshal(obj, &pm); err != nil {
		return fmt.Errorf("parse payment_method: %w", err)
	}
	card := payments.NormalizeStripeCard(pm.Card)
	if card == nil {
		return nil
	}
	return s.recordStripeCardForCustomer(ctx, pm.Customer, pm.ID, pm.ID, card)
}

// recordStripeCardForCustomer upserts a openrails.payment_methods row for the
// customer's card and links it as the active subscription's payment method. It
// delegates to payments.UpsertStripeCardForCustomer so the webhook path and the
// reconcile backfill share one implementation.
func (s *StripeWebhookService) recordStripeCardForCustomer(ctx context.Context, customerID, paymentMethodID, initialTxnID string, card *payments.StripeCard) error {
	var linker payments.StripeCardSubscriptionLinker
	if s.SubscriptionService != nil {
		linker = s.SubscriptionService
	}
	return payments.UpsertStripeCardForCustomer(
		ctx,
		s.DB,
		s.ProcessorCustomerService,
		linker,
		s.Clock,
		customerID, paymentMethodID, initialTxnID,
		card,
	)
}

func (s *StripeWebhookService) handleInvoicePaid(ctx context.Context, obj json.RawMessage) error {
	var inv stripeInvoice
	if err := json.Unmarshal(obj, &inv); err != nil {
		return fmt.Errorf("parse invoice: %w", err)
	}
	metadata := stripeInvoiceEffectiveMetadata(inv)
	userID := normalize.FirstNonEmpty(metadata["user_id"], metadata["userId"], metadata["uid"])
	if userID == "" {
		// Stripe subscription invoices do not carry user_id in invoice metadata (it lives
		// on the checkout session / subscription). Resolve it from the existing subscription
		// first (renewals), then from the processor customer mapping that
		// checkout.session.completed records (first invoice). If neither is available yet
		// (e.g. invoice.paid arrived before checkout.session.completed), return an error so
		// Stripe retries once the mapping exists.
		if procSubID := inv.processorSubscriptionID(); procSubID != "" {
			if sub, lookupErr := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorStripe), procSubID); lookupErr == nil && sub != nil {
				userID = strings.TrimSpace(sub.TenantSubjectID.String())
			}
		}
		if userID == "" && s.ProcessorCustomerService != nil {
			if customerID := strings.TrimSpace(inv.Customer); customerID != "" {
				if uid, lookupErr := s.ProcessorCustomerService.GetUserIDByCustomerID(ctx, string(models.ProcessorStripe), customerID); lookupErr == nil {
					userID = strings.TrimSpace(uid)
				}
			}
		}
	}
	if userID == "" {
		return fmt.Errorf("stripe invoice missing user_id metadata")
	}
	if s.ProcessorCustomerService != nil {
		customerID := strings.TrimSpace(inv.Customer)
		if customerID != "" {
			_ = s.ProcessorCustomerService.Upsert(ctx, userID, string(models.ProcessorStripe), customerID)
		}
	}
	priceID, price, err := s.resolvePriceFromMetadata(ctx, metadata, inv.Lines.Data)
	if err != nil {
		return err
	}
	if price == nil {
		price, err = s.PriceService.GetByID(ctx, priceID)
		if err != nil {
			return fmt.Errorf("price lookup failed: %w", err)
		}
	}
	if err := validateStripeInvoicePrice(inv, price); err != nil {
		return err
	}
	paymentTransactionID := normalize.FirstNonEmpty(inv.Charge, inv.PaymentIntent)
	if paymentTransactionID == "" {
		// The 2026-04-22.preview invoice.paid payload omits charge/payment_intent
		// (those arrive on charge.succeeded / invoice_payment.paid). Fall back to
		// the invoice id as the stable transaction id for the subscription
		// invoice; the real charge id + card are captured by handleChargeSucceeded.
		paymentTransactionID = strings.TrimSpace(inv.ID)
		if paymentTransactionID == "" {
			return fmt.Errorf("stripe invoice missing transaction id (charge/payment_intent/invoice id)")
		}
	}
	// Cross-key dedup: the same logical payment can already be recorded under a
	// DIFFERENT transaction-id key than the one we resolved above. The reconcile
	// backfill (reconcile.ensureChargePayment) keys subscription payments by the
	// charge id, while the 2026-04-22.preview invoice.paid payload omits the
	// charge and falls back to the invoice id. Because the (tenant, processor,
	// transaction_id) unique index can't see across those keys, a backfill-then-
	// webhook ordering would otherwise insert a duplicate row. If a row for this
	// invoice already exists under any of its ids (charge/payment_intent/invoice)
	// or via the stripe_invoice_id metadata the backfill writes, clear the
	// transaction id so CreateMembership/RenewMembership extend the subscription
	// without recording a second payment.
	if recorded, err := s.stripeInvoicePaymentAlreadyRecorded(ctx, inv); err != nil {
		return err
	} else if recorded {
		paymentTransactionID = ""
	}
	paymentMetadata := stripeInvoicePaymentMetadata(inv)

	processorSubID := inv.processorSubscriptionID()
	if processorSubID == "" {
		return fmt.Errorf("stripe invoice missing subscription id")
	}

	sub, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorStripe), processorSubID)
	createdSubscription := false
	if err != nil {
		sub, err = s.SubscriptionLifecycleService.CreateMembership(ctx, &subscriptions.CreateMembershipParams{
			UserID:                  userID,
			UserEmail:               normalize.OptionalString(inv.CustomerEmail),
			PriceID:                 priceID,
			Processor:               models.ProcessorStripe,
			ProcessorSubscriptionID: &processorSubID,
			CurrentPeriodStartsAt:   zeroTimePtr(stripeInvoicePeriodStart(inv)),
			CurrentPeriodEndsAt:     zeroTimePtr(stripeInvoicePeriodEnd(inv)),
			TransactionID:           paymentTransactionID,
			Amount:                  inv.AmountPaid,
			AmountProvided:          true,
			Currency:                inv.Currency,
			PaymentMetadata:         paymentMetadata,
		})
		if err != nil {
			return fmt.Errorf("create membership: %w", err)
		}
		createdSubscription = true
	} else {
		if err := s.SubscriptionLifecycleService.RenewMembership(ctx, &subscriptions.RenewMembershipParams{
			Processor:               models.ProcessorStripe,
			ProcessorSubscriptionID: processorSubID,
			CurrentPeriodStartsAt:   zeroTimePtr(stripeInvoicePeriodStart(inv)),
			CurrentPeriodEndsAt:     zeroTimePtr(stripeInvoicePeriodEnd(inv)),
			TransactionID:           paymentTransactionID,
			Amount:                  inv.AmountPaid,
			AmountProvided:          true,
			Currency:                inv.Currency,
			PaymentMetadata:         paymentMetadata,
		}); err != nil {
			if subscriptions.IsTerminalTransitionBlocked(err) {
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"processor_subscription_id": processorSubID,
					"transaction_id":            paymentTransactionID,
				}).Warn("Blocked terminal -> active transition for delayed Stripe renewal")
				return nil
			}
			return fmt.Errorf("renew membership: %w", err)
		}
	}

	if err := s.ensureStripePaidSubscriptionEntitlements(ctx, processorSubID, price); err != nil {
		return err
	}

	if s.CreditsService != nil {
		periodEnd := stripeInvoicePeriodEnd(inv)
		if periodEnd.IsZero() && sub.CurrentPeriodEndsAt != nil {
			periodEnd = sub.CurrentPeriodEndsAt.UTC()
		}
		if !periodEnd.IsZero() {
			cadence := models.CreditGrantCadencePerRenewal
			source := "subscription_renewal"
			if createdSubscription {
				cadence = models.CreditGrantCadenceOnce
				source = "subscription_initial"
			}
			if err := s.CreditsService.GrantSubscriptionCredits(ctx, credits.GrantSubscriptionCreditsParams{
				SubscriptionID: sub.ID,
				PeriodEnd:      periodEnd,
				Cadence:        cadence,
				Source:         source,
			}); err != nil {
				// Credits are an optional add-on to subscription processing; don't fail the entire webhook.
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"subscription_id": sub.ID,
					"period_end":      periodEnd,
				}).Warn("failed to grant subscription credits")
			}
		}
	}

	if s.CheckoutSessionService != nil {
		sessionID := parseCheckoutSessionID(metadata)
		if sessionID == uuid.Nil {
			if session, err := s.CheckoutSessionService.FindOpenByUserPriceProcessor(ctx, userID, priceID, models.ProcessorStripe); err == nil && session != nil {
				sessionID = session.ID
			}
		}
		if sessionID != uuid.Nil {
			paymentID := uuid.Nil
			if s.PaymentService != nil {
				if payment, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, paymentTransactionID); err == nil {
					paymentID = payment.ID
				}
			}
			if err := s.CheckoutSessionService.MarkSucceededWithSubscription(ctx, sessionID, paymentID, paymentTransactionID, sub.ID); err != nil {
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"checkout_session_id": sessionID,
					"transaction_id":      paymentTransactionID,
				}).Warn("failed to update checkout session from stripe invoice")
			}
		}
	}
	return nil
}

func (s *StripeWebhookService) ensureStripePaidSubscriptionEntitlements(ctx context.Context, processorSubID string, price *models.Price) error {
	if s == nil || s.DB == nil || s.SubscriptionService == nil {
		return nil
	}
	sub, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorStripe), processorSubID)
	if err != nil {
		return fmt.Errorf("load stripe subscription for paid entitlement reconciliation: %w", err)
	}
	entitlementsSpec := sub.EntitlementsSpecSnapshot
	if len(entitlementsSpec) == 0 && price != nil && s.ProductService != nil {
		product, err := s.ProductService.GetByID(ctx, price.ProductID)
		if err != nil {
			return fmt.Errorf("load stripe product for paid entitlement reconciliation: %w", err)
		}
		entitlementsSpec = product.EntitlementsSpec
	}
	entitlementSet := stripeEntitlementSet(entitlementsSpec)
	if len(entitlementSet) == 0 {
		return nil
	}
	entSvc := entitlements.NewEntitlementService(s.DB, s.Clock)
	notBefore := s.now().UTC()
	var endAt *time.Time
	if sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(notBefore) {
		end := sub.CurrentPeriodEndsAt.UTC()
		endAt = &end
	}
	for entName := range entitlementSet {
		exists, err := entSvc.ExistsBySource(ctx, models.EntitlementSourceSubscription, sub.ID, entName)
		if err != nil {
			return fmt.Errorf("check stripe paid entitlement %s: %w", entName, err)
		}
		if exists {
			continue
		}
		params := entitlements.PushNewEntitlementParams{
			UserID:      sub.TenantSubjectID.String(),
			Entitlement: entName,
			NotBefore:   &notBefore,
			SourceType:  models.EntitlementSourceSubscription,
			SourceID:    sub.ID,
		}
		if endAt != nil {
			params.EndAt = endAt
		} else {
			params.Indefinite = true
		}
		if _, err := entSvc.PushNewEntitlement(ctx, params); err != nil {
			return fmt.Errorf("grant stripe paid entitlement %s: %w", entName, err)
		}
	}
	return nil
}

func stripeInvoicePeriodEnd(inv stripeInvoice) time.Time {
	var end int64
	for _, line := range inv.Lines.Data {
		if line.Period.End > end {
			end = line.Period.End
		}
	}
	if end <= 0 {
		return time.Time{}
	}
	return time.Unix(end, 0).UTC()
}

func stripeInvoicePeriodStart(inv stripeInvoice) time.Time {
	var start int64
	for _, line := range inv.Lines.Data {
		if line.Period.Start <= 0 {
			continue
		}
		if start == 0 || line.Period.Start < start {
			start = line.Period.Start
		}
	}
	if start <= 0 {
		return time.Time{}
	}
	return time.Unix(start, 0).UTC()
}

func zeroTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (s *StripeWebhookService) handleCheckoutSessionExpired(ctx context.Context, obj json.RawMessage) error {
	if s.CheckoutSessionService == nil {
		return nil
	}

	var sess stripeCheckoutSession
	if err := json.Unmarshal(obj, &sess); err != nil {
		return fmt.Errorf("parse checkout session: %w", err)
	}

	sessionID := parseCheckoutSessionID(sess.Metadata)
	if sessionID == uuid.Nil {
		userID := normalize.FirstNonEmpty(sess.Metadata["user_id"], sess.Metadata["userId"], sess.Metadata["uid"])
		if userID == "" {
			return nil
		}
		priceID, _, err := s.resolvePriceFromMetadata(ctx, sess.Metadata, nil)
		if err != nil {
			return nil
		}
		if session, err := s.CheckoutSessionService.FindOpenByUserPriceProcessor(ctx, userID, priceID, models.ProcessorStripe); err == nil && session != nil {
			sessionID = session.ID
		}
	}

	if sessionID == uuid.Nil {
		return nil
	}

	if err := s.CheckoutSessionService.MarkExpired(ctx, sessionID, "checkout expired"); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"checkout_session_id": sessionID,
		}).Warn("failed to update checkout session from stripe expiration")
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
	if s.ProcessorCustomerService != nil {
		customerID := strings.TrimSpace(sess.Customer)
		if customerID != "" {
			_ = s.ProcessorCustomerService.Upsert(ctx, userID, string(models.ProcessorStripe), customerID)
		}
	}
	if sess.Mode != "payment" {
		return nil
	}
	paymentStatus := strings.TrimSpace(sess.PaymentStatus)
	paymentSettled := paymentStatus == "paid" || (sess.AmountTotal == 0 && paymentStatus == "no_payment_required")
	if !paymentSettled || strings.TrimSpace(sess.Status) != "complete" {
		log.WithContext(ctx).WithFields(log.Fields{
			"checkout_session_id": sess.ID,
			"status":              sess.Status,
			"payment_status":      sess.PaymentStatus,
		}).Info("Stripe checkout session not paid; waiting for async payment success")
		return nil
	}
	priceID, price, err := s.resolvePriceFromMetadata(ctx, sess.Metadata, nil)
	if err != nil {
		return err
	}
	if price == nil {
		price, err = s.PriceService.GetByID(ctx, priceID)
		if err != nil {
			return fmt.Errorf("price lookup failed: %w", err)
		}
	}
	if sess.AmountTotal != price.Amount {
		return fmt.Errorf("stripe checkout amount mismatch: got %d, want %d", sess.AmountTotal, price.Amount)
	}
	if !strings.EqualFold(strings.TrimSpace(sess.Currency), strings.TrimSpace(price.Currency)) {
		return fmt.Errorf("stripe checkout currency mismatch: got %s, want %s", sess.Currency, price.Currency)
	}
	paymentTransactionID := normalize.FirstNonEmpty("", sess.PaymentIntent)
	if paymentTransactionID == "" {
		if sess.AmountTotal != 0 || strings.TrimSpace(sess.ID) == "" {
			return fmt.Errorf("stripe checkout session missing payment_intent")
		}
		paymentTransactionID = strings.TrimSpace(sess.ID)
	}

	result, err := s.PurchaseRegistrar.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:         userID,
		PriceID:        priceID,
		Processor:      string(models.ProcessorStripe),
		TransactionID:  paymentTransactionID,
		Amount:         sess.AmountTotal,
		AmountProvided: true,
		Currency:       sess.Currency,
		Metadata:       stripeCheckoutPaymentMetadata(sess),
	})
	if err != nil {
		return fmt.Errorf("register purchase: %w", err)
	}

	if s.CheckoutSessionService != nil {
		sessionID := parseCheckoutSessionID(sess.Metadata)
		if sessionID == uuid.Nil {
			if session, err := s.CheckoutSessionService.FindOpenByUserPriceProcessor(ctx, userID, priceID, models.ProcessorStripe); err == nil && session != nil {
				sessionID = session.ID
			}
		}
		if sessionID != uuid.Nil {
			if err := s.CheckoutSessionService.MarkSucceeded(ctx, sessionID, result.PaymentID, paymentTransactionID); err != nil {
				log.WithContext(ctx).WithError(err).WithFields(log.Fields{
					"checkout_session_id": sessionID,
					"transaction_id":      paymentTransactionID,
				}).Warn("failed to update checkout session from stripe checkout")
			}
		}
	}

	var creditsSpec models.CreditsSpec
	if s.PaymentService != nil {
		payment, err := s.PaymentService.GetByID(ctx, result.PaymentID)
		if err == nil {
			creditsSpec = payment.CreditsSpecSnapshot
		} else {
			log.WithContext(ctx).WithError(err).WithField("payment_id", result.PaymentID).Warn("failed to load payment snapshot for stripe purchase credits")
		}
	}
	if len(creditsSpec) > 0 && s.CreditsService != nil {
		for creditType, spec := range creditsSpec {
			cadence := spec.Cadence
			if cadence == "" {
				cadence = models.CreditGrantCadenceOnce
			}
			if cadence != models.CreditGrantCadenceOnce {
				continue
			}
			if strings.TrimSpace(creditType) == "" || spec.Amount <= 0 {
				continue
			}
			var expiresAt *time.Time
			if spec.ExpiresDays != nil && *spec.ExpiresDays > 0 {
				t := s.now().UTC().Add(time.Duration(*spec.ExpiresDays) * 24 * time.Hour)
				expiresAt = &t
			}
			_, err = s.CreditsService.Deposit(ctx, credits.CreditDepositParams{
				Actor:      userID,
				CreditType: creditType,
				Amount:     spec.Amount,
				Source:     "purchase",
				SourceID:   &result.PaymentID,
				ExpiresAt:  expiresAt,
			})
			if err != nil {
				return fmt.Errorf("grant purchased credits (%s): %w", creditType, err)
			}
		}
	}
	return nil
}

func (s *StripeWebhookService) handleCheckoutSessionAsyncPaymentFailed(ctx context.Context, obj json.RawMessage) error {
	var sess stripeCheckoutSession
	if err := json.Unmarshal(obj, &sess); err != nil {
		return fmt.Errorf("parse checkout session: %w", err)
	}
	if s.CheckoutSessionService == nil {
		return nil
	}
	sessionID := parseCheckoutSessionID(sess.Metadata)
	if sessionID == uuid.Nil {
		return nil
	}
	if err := s.CheckoutSessionService.MarkFailed(ctx, sessionID, "stripe async payment failed", "async_payment_failed"); err != nil {
		return fmt.Errorf("mark stripe checkout failed: %w", err)
	}
	return nil
}

func parseCheckoutSessionID(metadata map[string]string) uuid.UUID {
	if metadata == nil {
		return uuid.Nil
	}
	raw := strings.TrimSpace(metadata["checkout_session_id"])
	if raw == "" {
		return uuid.Nil
	}
	id, err := api.ParseCheckoutSessionID(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
}

func (s *StripeWebhookService) handleSubscriptionDeleted(ctx context.Context, obj json.RawMessage) error {
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(obj, &data); err != nil {
		return fmt.Errorf("parse subscription: %w", err)
	}
	if data.ID == "" {
		return nil
	}
	return s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
		Processor:               ptrProcessor(models.ProcessorStripe),
		ProcessorSubscriptionID: &data.ID,
		CancelType:              models.CancelTypeUser,
		RevokeAccess:            false,
	})
}

func (s *StripeWebhookService) handleSubscriptionUpdated(ctx context.Context, obj json.RawMessage) error {
	var data struct {
		ID                 string `json:"id"`
		Status             string `json:"status"`
		CancelAtPeriodEnd  bool   `json:"cancel_at_period_end"`
		CanceledAt         int64  `json:"canceled_at"`
		CurrentPeriodStart int64  `json:"current_period_start"`
		CurrentPeriodEnd   int64  `json:"current_period_end"`
		Items              struct {
			Data []struct {
				Price struct {
					ID string `json:"id"`
				} `json:"price"`
			} `json:"data"`
		} `json:"items"`
	}
	if err := json.Unmarshal(obj, &data); err != nil {
		return fmt.Errorf("parse subscription update: %w", err)
	}
	subID := strings.TrimSpace(data.ID)
	if subID == "" {
		return nil
	}
	sub, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorStripe), subID)
	if err != nil {
		return nil
	}
	incomingStatus := strings.ToLower(strings.TrimSpace(data.Status))
	if _, terminal := subscriptions.TerminalCancelReason(sub); terminal && (incomingStatus == "active" || incomingStatus == "trialing") {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":           sub.ID,
			"processor_subscription_id": subID,
			"incoming_status":           incomingStatus,
		}).Warn("ignoring stripe active update for terminal local subscription")
		return nil
	}

	oldEntitlementsSpec := models.CloneEntitlementsSpec(sub.EntitlementsSpecSnapshot)
	var oldProduct *models.Product
	if s.ProductService != nil && sub.ProductID != uuid.Nil {
		if product, err := s.ProductService.GetByID(ctx, sub.ProductID); err == nil {
			oldProduct = product
		} else {
			log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warn("failed to load old product for stripe entitlement reconciliation")
		}
	}
	var newProduct *models.Product

	if len(data.Items.Data) > 0 {
		stripePrice := strings.TrimSpace(data.Items.Data[0].Price.ID)
		if stripePrice != "" {
			price, err := s.PriceService.GetByStripePriceID(ctx, stripePrice)
			if err == nil {
				sub.PriceID = price.ID
				sub.ProductID = price.ProductID
				sub.ScheduledPriceID = nil
				if s.ProductService != nil {
					if product, err := s.ProductService.GetByID(ctx, price.ProductID); err == nil {
						newProduct = product
						sub.EntitlementsSpecSnapshot = models.CloneEntitlementsSpec(product.EntitlementsSpec)
						sub.CreditsSpecSnapshot = models.CloneCreditsSpec(product.CreditsSpec)
					} else {
						log.WithContext(ctx).WithError(err).WithField("subscription_id", sub.ID).Warn("failed to load new product for stripe entitlement reconciliation")
					}
				}
			} else {
				log.WithFields(log.Fields{
					"stripe_price_id": stripePrice,
					"subscription_id": sub.ID,
				}).Warn("stripe subscription update price not mapped")
			}
		}
	}

	if data.CurrentPeriodStart > 0 {
		ts := time.Unix(data.CurrentPeriodStart, 0).UTC()
		sub.CurrentPeriodStartsAt = &ts
	}
	if data.CurrentPeriodEnd > 0 {
		ts := time.Unix(data.CurrentPeriodEnd, 0).UTC()
		sub.CurrentPeriodEndsAt = &ts
	}

	if data.CancelAtPeriodEnd {
		applyStripeScheduledCancellation(sub, data.Status, data.CanceledAt, s.now().UTC())
	} else {
		// Not scheduled to cancel. If Stripe now reports it active/trialing while
		// the local record is still cancelled, this is a resume — clear the prior
		// scheduled-cancellation marks before applying the status.
		if sub.Status == models.StatusCancelled && (incomingStatus == "active" || incomingStatus == "trialing") {
			sub.CancelledAt = nil
			sub.EndedAt = nil
		}
		applyStripeSubscriptionStatus(sub, data.Status, s.now().UTC())
	}

	if err := s.SubscriptionService.Update(ctx, sub); err != nil {
		return fmt.Errorf("update subscription from stripe: %w", err)
	}
	if err := s.reconcileStripeSubscriptionEntitlements(ctx, sub, oldEntitlementsSpec, oldProduct, newProduct); err != nil {
		return err
	}
	return nil
}

func applyStripeScheduledCancellation(sub *models.Subscription, rawStatus string, canceledAt int64, now time.Time) {
	if sub == nil {
		return
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	incoming := strings.ToLower(strings.TrimSpace(rawStatus))
	periodEndsInFuture := sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(now)
	if periodEndsInFuture && incoming != "canceled" && incoming != "incomplete_expired" {
		// Keep paid-through scheduled cancellations as lifecycle owners so
		// duplicate-checkout guards and the tier-group unique index still apply.
		applyStripeSubscriptionStatus(sub, rawStatus, now)
	} else {
		sub.Status = models.StatusCancelled
		sub.ClearRetrySchedule()
	}
	cancelType := models.CancelTypeUser
	if sub.CancelType == nil {
		sub.CancelType = &cancelType
	}
	if sub.CancelledAt == nil {
		ts := now
		if canceledAt > 0 {
			ts = time.Unix(canceledAt, 0).UTC()
		}
		sub.CancelledAt = &ts
	}
	endAt := now
	if sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(now) {
		endAt = *sub.CurrentPeriodEndsAt
	}
	sub.EndedAt = &endAt
}

func (s *StripeWebhookService) reconcileStripeSubscriptionEntitlements(ctx context.Context, sub *models.Subscription, oldEntitlementsSpec map[string]*int, oldProduct, newProduct *models.Product) error {
	if s == nil || s.DB == nil || sub == nil {
		return nil
	}
	entSvc := entitlements.NewEntitlementService(s.DB, s.Clock)
	now := s.now().UTC()

	if sub.Status == models.StatusCancelled && (sub.CurrentPeriodEndsAt == nil || !sub.CurrentPeriodEndsAt.After(now)) {
		if err := entSvc.RevokeSourcesForSubscription(ctx, sub.TenantSubjectID.String(), sub.ID, models.EntitlementRevokeAdmin, models.EntitlementSourceSubscription, models.EntitlementSourceGrace); err != nil {
			return fmt.Errorf("revoke stripe subscription entitlements after terminal update: %w", err)
		}
		return nil
	}

	newEntitlementsSpec := sub.EntitlementsSpecSnapshot
	if len(oldEntitlementsSpec) == 0 && oldProduct != nil {
		oldEntitlementsSpec = oldProduct.EntitlementsSpec
	}
	if len(newEntitlementsSpec) == 0 && newProduct != nil {
		newEntitlementsSpec = newProduct.EntitlementsSpec
	}
	if len(oldEntitlementsSpec) == 0 && len(newEntitlementsSpec) == 0 {
		return nil
	}

	oldEnts := stripeEntitlementSet(oldEntitlementsSpec)
	newEnts := stripeEntitlementSet(newEntitlementsSpec)
	sourceType := models.EntitlementSourceSubscription
	sourceID := sub.ID

	for entName := range oldEnts {
		if newEnts[entName] {
			continue
		}
		if err := entSvc.RevokeExistingEntitlement(ctx, entitlements.RevokeExistingEntitlementParams{
			UserID:      sub.TenantSubjectID.String(),
			Entitlement: entName,
			SourceType:  &sourceType,
			SourceID:    &sourceID,
			Reason:      models.EntitlementRevokeDowngrade,
		}); err != nil {
			return fmt.Errorf("revoke stripe subscription entitlement %s: %w", entName, err)
		}
	}

	return nil
}

func stripeEntitlementSet(spec map[string]*int) map[string]bool {
	out := map[string]bool{}
	for name := range spec {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = true
		}
	}
	if len(out) == 0 {
		out["premium"] = true
	}
	return out
}

func applyStripeSubscriptionStatus(sub *models.Subscription, rawStatus string, now time.Time) {
	if sub == nil {
		return
	}

	status := strings.TrimSpace(strings.ToLower(rawStatus))
	switch status {
	case "active", "trialing":
		sub.Status = models.StatusActive
	case "past_due", "unpaid", "incomplete":
		sub.Status = models.StatusPastDue
	case "canceled", "incomplete_expired":
		sub.Status = models.StatusCancelled
		sub.ClearRetrySchedule()
		if sub.CancelledAt == nil {
			timestamp := now
			sub.CancelledAt = &timestamp
		}
		if sub.EndedAt == nil {
			endAt := now
			if sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(now) {
				endAt = *sub.CurrentPeriodEndsAt
			}
			sub.EndedAt = &endAt
		}
	}
}

// failedPaymentTransactionID builds the transaction id for a recorded failed
// payment. The "failed:" prefix guarantees it never collides with the eventual
// successful charge's row (which uses the bare charge/payment_intent id), while
// staying stable for a given failed invoice so duplicate webhook deliveries
// dedupe via the (processor, transaction_id) unique constraint.
func failedPaymentTransactionID(inv stripeInvoice) string {
	return "failed:" + normalize.FirstNonEmpty(inv.Charge, inv.PaymentIntent, inv.ID)
}

func (s *StripeWebhookService) handleInvoicePaymentFailed(ctx context.Context, obj json.RawMessage) error {
	var inv stripeInvoice
	if err := json.Unmarshal(obj, &inv); err != nil {
		return fmt.Errorf("parse failed invoice: %w", err)
	}
	processorSubID := inv.processorSubscriptionID()
	if processorSubID == "" {
		return nil
	}
	sub, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorStripe), processorSubID)
	if err != nil {
		return nil
	}
	if sub.Status == models.StatusCancelled {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id":           sub.ID,
			"processor_subscription_id": processorSubID,
			"invoice_id":                inv.ID,
		}).Warn("ignoring stripe payment failure for cancelled local subscription")
		return nil
	}
	sub.Status = models.StatusPastDue
	if err := s.SubscriptionService.Update(ctx, sub); err != nil {
		return fmt.Errorf("update stripe subscription after payment failure: %w", err)
	}

	// Record the failed attempt so it shows in payment history. The transaction
	// id is prefixed with "failed:" so it can never collide with the eventual
	// successful charge's row, and so duplicate webhook deliveries of the same
	// failure dedupe (CreateIfNotExists on processor+transaction_id). This is a
	// plain insert: no entitlements/credits are granted for a failed payment.
	if s.PaymentService != nil {
		amount := inv.AmountDue
		txnID := failedPaymentTransactionID(inv)
		subID := sub.ID
		failed := &models.Payment{
			ID:              uuidutil.NewV7(),
			TenantSubjectID: sub.TenantSubjectID,
			PriceID:         sub.PriceID,
			SubscriptionID:  &subID,
			Processor:       models.ProcessorStripe,
			TransactionID:   txnID,
			Amount:          amount,
			ListAmount:      amount,
			Currency:        normalize.FirstNonEmpty(inv.Currency, "usd"),
			Status:          "failed",
			PurchasedAt:     s.now(),
		}
		if _, err := s.PaymentService.CreateIfNotExists(ctx, failed); err != nil {
			return fmt.Errorf("record failed payment: %w", err)
		}
	}

	if s.NotificationService != nil {
		notification := &models.NotificationQueue{
			ID:              uuidutil.NewV7(),
			TenantSubjectID: sub.TenantSubjectID,
			EventType:       models.NotificationPaymentMethodFailed,
			Data: map[string]any{
				"processor":                 string(models.ProcessorStripe),
				"processor_subscription_id": processorSubID,
				"stripe_invoice_id":         strings.TrimSpace(inv.ID),
			},
		}
		if err := s.NotificationService.CreateAndDeliver(ctx, notification); err != nil {
			log.WithContext(ctx).WithError(err).WithFields(log.Fields{
				"subscription_id":           sub.ID,
				"processor_subscription_id": processorSubID,
				"invoice_id":                inv.ID,
			}).Error("failed to create and deliver stripe payment failure notification")
		}
	}

	s.logStripePaymentFailure(ctx, sub, inv, processorSubID)
	return nil
}

func (s *StripeWebhookService) logStripePaymentFailure(ctx context.Context, sub *models.Subscription, inv stripeInvoice, processorSubID string) {
	if s.EventLogService == nil || sub == nil {
		return
	}
	reason := "stripe_invoice_payment_failed"
	if err := s.EventLogService.LogLifecycleFailure(ctx, sub.ID, sub.TenantSubjectID.String(), models.ProcessorStripe, models.StatusPastDue, &reason, nil, s.now()); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"subscription_id":           sub.ID,
			"processor_subscription_id": processorSubID,
			"invoice_id":                inv.ID,
		}).Error("failed to log stripe payment failure event")
	}

	metadata := map[string]interface{}{
		"processor":                 string(models.ProcessorStripe),
		"event_source":              "webhook",
		"stripe_invoice_id":         strings.TrimSpace(inv.ID),
		"processor_subscription_id": processorSubID,
		"is_renewal":                true,
	}
	priceAmount := 0.0
	priceCurrency := strings.ToLower(strings.TrimSpace(inv.Currency))
	if priceCurrency == "" {
		priceCurrency = "usd"
	}
	billingCycleDays := uint32(0)
	var productID *uuid.UUID
	priceID := sub.PriceID
	if sub.Price != nil {
		priceAmount = float64(sub.Price.Amount) / 100.0
		if curr := strings.TrimSpace(sub.Price.Currency); curr != "" {
			priceCurrency = curr
		}
		if sub.Price.BillingCycleDays != nil {
			billingCycleDays, _ = safecast.Convert[uint32](*sub.Price.BillingCycleDays)
		}
		productID = &sub.Price.ProductID
	}
	if txnID := strings.TrimPrefix(failedPaymentTransactionID(inv), "failed:"); strings.TrimSpace(txnID) != "" {
		metadata["transaction_id"] = txnID
	}
	statusPastDue := string(models.StatusPastDue)
	if err := s.EventLogService.LogSubscriptionEvent(ctx, analytics.SubscriptionEventData{
		EventID:                 uuidutil.NewV7(),
		SubscriptionID:          sub.ID,
		UserID:                  sub.TenantSubjectID.String(),
		EventType:               analytics.PaymentEventSubscriptionPastDue,
		Status:                  statusPastDue,
		PriceAmount:             priceAmount,
		PriceCurrency:           priceCurrency,
		BillingCycleDays:        billingCycleDays,
		ProductID:               productID,
		PriceID:                 &priceID,
		Processor:               string(models.ProcessorStripe),
		ProcessorSubscriptionID: &processorSubID,
		Metadata:                analytics.CreateMetadataJSON(metadata),
		Timestamp:               s.now().UTC(),
	}); err != nil {
		log.WithContext(ctx).WithError(err).WithFields(log.Fields{
			"subscription_id":           sub.ID,
			"processor_subscription_id": processorSubID,
			"invoice_id":                inv.ID,
		}).Error("failed to log stripe subscription past_due event")
	}
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
			refund.Status = "succeeded"
		}
		if strings.TrimSpace(refund.Charge) == "" {
			refund.Charge = charge.ID
		}
		if strings.TrimSpace(refund.PaymentIntent) == "" {
			refund.PaymentIntent = charge.PaymentIntent
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
		log.WithContext(ctx).WithFields(log.Fields{
			"refund_id": refund.ID,
			"status":    refund.Status,
		}).Info("Stripe refund is not succeeded; skipping ledger and entitlement changes")
		return nil
	}
	refundID := strings.TrimSpace(refund.ID)
	if refundID == "" || refund.Amount <= 0 {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe refund missing id or amount"))
	}
	var original *models.Payment
	if existing, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, refundID); err == nil && existing != nil {
		if existing.RefundedPaymentID == nil {
			return nil
		}
		original, err = s.PaymentService.GetByID(ctx, *existing.RefundedPaymentID)
		if err != nil {
			return fmt.Errorf("lookup original stripe payment for existing refund: %w", err)
		}
	} else if err != nil && !repo.IsNotFound(err) {
		return fmt.Errorf("lookup stripe refund payment: %w", err)
	}
	if original == nil {
		var err error
		original, err = s.lookupStripeOriginalPayment(ctx, refund.Charge, refund.PaymentIntent)
		if err != nil {
			return err
		}
		if _, err := s.PaymentService.Refund(ctx, original.ID, refundID, refund.Amount); err != nil {
			return fmt.Errorf("record stripe refund: %w", err)
		}
	}
	refundedTotal, err := s.PaymentService.GetRefundTotalByPaymentID(ctx, original.ID)
	if err != nil {
		return fmt.Errorf("calculate stripe refund total: %w", err)
	}
	if original.SubscriptionID != nil && refundedTotal >= original.Amount && s.SubscriptionLifecycleService != nil {
		processor := models.ProcessorStripe
		reason := "Stripe refund processed"
		if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: original.SubscriptionID,
			Processor:      &processor,
			CancelType:     models.CancelTypeMerchant,
			CancelFeedback: &reason,
			RevokeAccess:   true,
		}); err != nil {
			return fmt.Errorf("cancel subscription after stripe refund: %w", err)
		}
	} else if original.SubscriptionID == nil && refundedTotal >= original.Amount && s.DB != nil {
		entSvc := entitlements.NewEntitlementService(s.DB, s.Clock)
		if err := entSvc.EndActiveByPayment(ctx, original.ID, models.EntitlementRevokeRefund); err != nil {
			return fmt.Errorf("revoke one-off entitlements after stripe refund: %w", err)
		}
		// Revoke the durable product access grant tied to this payment (issue #250),
		// consistent with the entitlement reversal above.
		paSvc := productaccess.NewService(s.DB, s.Clock)
		if _, err := paSvc.RevokeProductAccessByPayment(ctx, original.ID, models.ProductAccessRevokeRefund); err != nil {
			return fmt.Errorf("revoke product access after stripe refund: %w", err)
		}
	}
	return nil
}

func (s *StripeWebhookService) handleDispute(ctx context.Context, eventType string, obj json.RawMessage) error {
	var dispute stripeDispute
	if err := json.Unmarshal(obj, &dispute); err != nil {
		return fmt.Errorf("parse stripe dispute: %w", err)
	}
	if stripeDisputeWon(eventType, dispute.Status) {
		return s.handleStripeDisputeWon(ctx, dispute)
	}
	if !stripeDisputeShouldReverse(eventType, dispute.Status) {
		log.WithContext(ctx).WithFields(log.Fields{
			"dispute_id": dispute.ID,
			"event_type": eventType,
			"status":     dispute.Status,
		}).Info("Stripe dispute does not require reversal; skipping ledger and entitlement changes")
		return nil
	}
	if s.PaymentService == nil {
		return fmt.Errorf("payment service is required for stripe dispute")
	}
	disputeID := strings.TrimSpace(dispute.ID)
	if disputeID == "" || dispute.Amount <= 0 {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe dispute missing id or amount"))
	}
	var original *models.Payment
	if existing, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, disputeID); err == nil && existing != nil {
		if existing.RefundedPaymentID == nil {
			return nil
		}
		original, err = s.PaymentService.GetByID(ctx, *existing.RefundedPaymentID)
		if err != nil {
			return fmt.Errorf("lookup original stripe payment for existing dispute: %w", err)
		}
	} else if err != nil && !repo.IsNotFound(err) {
		return fmt.Errorf("lookup stripe dispute payment: %w", err)
	}
	var ledgerErr error
	if original == nil {
		var err error
		original, err = s.lookupStripeOriginalPayment(ctx, dispute.Charge, dispute.PaymentIntent)
		if err != nil {
			return err
		}
		if _, err := s.PaymentService.Refund(ctx, original.ID, disputeID, dispute.Amount); err != nil {
			ledgerErr = fmt.Errorf("record stripe dispute reversal: %w", err)
			log.WithContext(ctx).WithError(ledgerErr).WithFields(log.Fields{
				"dispute_id":          disputeID,
				"original_payment_id": original.ID,
			}).Error("Failed to record Stripe dispute reversal; continuing entitlement revocation")
		}
	}
	if original.SubscriptionID != nil && s.SubscriptionLifecycleService != nil {
		processor := models.ProcessorStripe
		reason := fmt.Sprintf("STRIPE DISPUTE: %s status=%s", strings.TrimSpace(dispute.Reason), strings.TrimSpace(dispute.Status))
		if err := s.SubscriptionLifecycleService.CancelMembership(ctx, &subscriptions.CancelMembershipParams{
			SubscriptionID: original.SubscriptionID,
			Processor:      &processor,
			CancelType:     models.CancelTypeChargeback,
			CancelFeedback: &reason,
			RevokeAccess:   true,
		}); err != nil {
			return fmt.Errorf("cancel subscription after stripe dispute: %w", err)
		}
	} else if original.SubscriptionID == nil && s.DB != nil {
		entSvc := entitlements.NewEntitlementService(s.DB, s.Clock)
		if err := entSvc.EndActiveByPayment(ctx, original.ID, models.EntitlementRevokeChargeback); err != nil {
			return fmt.Errorf("revoke one-off entitlements after stripe dispute: %w", err)
		}
		// Revoke the durable product access grant tied to this payment (issue #250),
		// consistent with the entitlement reversal above.
		paSvc := productaccess.NewService(s.DB, s.Clock)
		if _, err := paSvc.RevokeProductAccessByPayment(ctx, original.ID, models.ProductAccessRevokeChargeback); err != nil {
			return fmt.Errorf("revoke product access after stripe dispute: %w", err)
		}
	}
	if ledgerErr != nil {
		originalID := original.ID
		if err := recordLedgerRepairAlert(ctx, s.NotificationService, s.DB, s.now(), ledgerRepairAlert{
			Provider:          "stripe",
			Operation:         "dispute_reversal",
			TransactionID:     disputeID,
			UserID:            original.TenantSubjectID.String(),
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

func (s *StripeWebhookService) handleStripeDisputeWon(ctx context.Context, dispute stripeDispute) error {
	if s.PaymentService == nil {
		return nil
	}
	disputeID := strings.TrimSpace(dispute.ID)
	if disputeID == "" {
		return nil
	}
	disputeReversal, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, disputeID)
	if err != nil {
		if repo.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("lookup stripe dispute reversal for won dispute: %w", err)
	}
	if disputeReversal == nil || disputeReversal.RefundedPaymentID == nil || disputeReversal.Amount >= 0 {
		return nil
	}

	recoveryID := "dispute_won:" + disputeID
	var existingRecovery *models.Payment
	if existing, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, recoveryID); err == nil && existing != nil {
		existingRecovery = existing
	} else if err != nil && !repo.IsNotFound(err) {
		return fmt.Errorf("lookup stripe won dispute recovery payment: %w", err)
	}

	original, err := s.PaymentService.GetByID(ctx, *disputeReversal.RefundedPaymentID)
	if err != nil {
		return fmt.Errorf("lookup original stripe payment for won dispute: %w", err)
	}
	if existingRecovery != nil {
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
			TenantSubjectID:   original.TenantSubjectID,
			PriceID:           original.PriceID,
			SubscriptionID:    original.SubscriptionID,
			RefundedPaymentID: &original.ID,
			Processor:         original.Processor,
			TransactionID:     recoveryID,
			Amount:            -disputeReversal.Amount,
			ListAmount:        original.ListAmount,
			Currency:          original.Currency,
			PurchasedAt:       s.now(),
			CreatedAt:         s.now(),
		}
		if err := s.PaymentService.Create(ctx, recovery); err != nil {
			return fmt.Errorf("record stripe won dispute recovery payment: %w", err)
		}
	}

	if original.SubscriptionID != nil {
		if err := s.reactivateStripeSubscriptionAfterWonDispute(ctx, *original.SubscriptionID, original); err != nil {
			return err
		}
	} else if s.DB != nil {
		log.WithContext(ctx).WithFields(log.Fields{
			"dispute_id":          disputeID,
			"original_payment_id": original.ID,
		}).Warn("Stripe dispute was won for a one-off payment; entitlement restoration requires manual review")
	}
	return nil
}

func (s *StripeWebhookService) reactivateStripeSubscriptionAfterWonDispute(ctx context.Context, subscriptionID uuid.UUID, original *models.Payment) error {
	if s.SubscriptionService == nil || s.SubscriptionLifecycleService == nil || original == nil {
		return nil
	}
	sub, err := s.SubscriptionService.GetByID(ctx, subscriptionID)
	if err != nil {
		return fmt.Errorf("load stripe subscription for won dispute recovery: %w", err)
	}
	if sub.Status != models.StatusCancelled {
		return nil
	}
	now := s.now().UTC()
	var paidThrough time.Time
	if sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(now) {
		paidThrough = sub.CurrentPeriodEndsAt.UTC()
	} else if s.PriceService != nil {
		price, err := s.PriceService.GetByID(ctx, original.PriceID)
		if err != nil {
			return fmt.Errorf("load stripe price for won dispute recovery: %w", err)
		}
		if price.BillingCycleDays != nil && *price.BillingCycleDays > 0 {
			paidThrough = original.PurchasedAt.UTC().Add(time.Duration(*price.BillingCycleDays) * 24 * time.Hour)
		}
	}
	if !paidThrough.After(now) {
		log.WithContext(ctx).WithFields(log.Fields{
			"subscription_id": sub.ID,
			"payment_id":      original.ID,
		}).Warn("Stripe dispute was won but no future paid-through date exists; subscription restoration requires manual review")
		return nil
	}
	_, err = s.SubscriptionLifecycleService.ReactivateMembership(ctx, &subscriptions.ReactivateMembershipParams{
		Processor:                 models.ProcessorStripe,
		ProcessorSubscriptionID:   sub.ProcessorSubscriptionID,
		CurrentPeriodEndsAt:       &paidThrough,
		AllowTerminalReactivation: true,
	})
	if err != nil {
		return fmt.Errorf("reactivate stripe subscription after won dispute: %w", err)
	}
	return nil
}

func stripeRefundSucceeded(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "succeeded")
}

func stripeDisputeWon(eventType, status string) bool {
	return eventType == "charge.dispute.closed" && strings.EqualFold(strings.TrimSpace(status), "won")
}

func stripeDisputeShouldReverse(eventType, status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "won" {
		return false
	}
	switch eventType {
	case "charge.dispute.created":
		return true
	case "charge.dispute.closed":
		return status == "lost"
	default:
		return false
	}
}

func (s *StripeWebhookService) lookupStripeOriginalPayment(ctx context.Context, chargeID, paymentIntentID string) (*models.Payment, error) {
	for _, candidate := range []string{strings.TrimSpace(chargeID), strings.TrimSpace(paymentIntentID)} {
		if candidate == "" {
			continue
		}
		payment, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, candidate)
		if err == nil {
			return payment, nil
		}
		if !repo.IsNotFound(err) {
			return nil, fmt.Errorf("lookup stripe original payment: %w", err)
		}
	}
	return nil, fmt.Errorf("stripe original payment not found for charge/payment_intent")
}

func (s *StripeWebhookService) resolvePriceFromMetadata(ctx context.Context, metadata map[string]string, lines []stripeInvoiceLineItem) (uuid.UUID, *models.Price, error) {
	if idStr := normalize.FirstNonEmpty(metadata["internal_price_id"], metadata["price_id"]); idStr != "" {
		priceID, err := uuid.Parse(idStr)
		if err == nil {
			return priceID, nil, nil
		}
	}
	if len(lines) > 0 {
		if stripePriceID := lines[0].priceID(); stripePriceID != "" {
			price, err := s.PriceService.GetByStripePriceID(ctx, stripePriceID)
			if err != nil {
				return uuid.Nil, nil, fmt.Errorf("stripe price not mapped")
			}
			return price.ID, price, nil
		}
	}
	return uuid.Nil, nil, fmt.Errorf("unable to resolve price")
}

func stripeInvoiceEffectiveMetadata(inv stripeInvoice) map[string]string {
	metadata := map[string]string{}
	for key, value := range inv.Parent.SubscriptionDetails.Metadata {
		metadata[key] = value
	}
	for key, value := range inv.SubscriptionDetails.Metadata {
		metadata[key] = value
	}
	for key, value := range inv.Metadata {
		metadata[key] = value
	}
	return metadata
}

func validateStripeInvoicePrice(inv stripeInvoice, price *models.Price) error {
	if price == nil {
		return fmt.Errorf("stripe invoice price is required for validation")
	}
	if !strings.EqualFold(strings.TrimSpace(inv.Currency), strings.TrimSpace(price.Currency)) {
		return fmt.Errorf("stripe invoice currency mismatch: got %s, want %s", inv.Currency, price.Currency)
	}
	// Model B upgrades drive Stripe with proration_behavior=always_invoice +
	// billing_cycle_anchor=now (see checkout.processTierChangeStripe), so the
	// upgrade invoice total is the PRORATED charge (new_full - old_unused), which
	// by design never equals the new tier's list price. For those invoices Stripe
	// is the source of truth for the amount — handleInvoicePaid records
	// inv.AmountPaid, not price.Amount — so require only a positive settled
	// amount and matching currency rather than an exact list-price match.
	if stripeInvoiceIsProration(inv) {
		if inv.AmountPaid <= 0 {
			return fmt.Errorf("stripe proration invoice has non-positive amount_paid: %d", inv.AmountPaid)
		}
		return nil
	}
	if inv.AmountPaid != price.Amount {
		if inv.AmountPaid == 0 && stripeInvoiceHasNoRefundableTransaction(inv) {
			return nil
		}
		return fmt.Errorf("stripe invoice amount mismatch: got %d, want %d", inv.AmountPaid, price.Amount)
	}
	return nil
}

// stripeInvoiceIsProration reports whether the invoice was generated by a
// mid-cycle subscription change (Stripe billing_reason="subscription_update"),
// i.e. a Model B upgrade invoiced with proration. Such invoices charge a
// prorated total, not the plan's full list price.
func stripeInvoiceIsProration(inv stripeInvoice) bool {
	return strings.EqualFold(strings.TrimSpace(inv.BillingReason), "subscription_update")
}

// stripeInvoicePaymentAlreadyRecorded reports whether a completed openrails.payments
// row already exists for this Stripe invoice under any of its known
// transaction-id keys (charge, payment_intent, invoice id) or via the
// stripe_invoice_id metadata that the reconcile backfill and invoice_payment
// linker write. It lets handleInvoicePaid avoid inserting a second row for a
// payment already recorded under a different key (e.g. the backfill keyed by
// charge id vs the preview webhook keyed by invoice id), which the
// (tenant, processor, transaction_id) unique index cannot dedupe on its own.
//
// Failed-attempt rows never match: their transaction ids are "failed:"-prefixed
// and they carry no stripe_invoice_id metadata, so a prior failure does not
// suppress recording the eventual success.
func (s *StripeWebhookService) stripeInvoicePaymentAlreadyRecorded(ctx context.Context, inv stripeInvoice) (bool, error) {
	if s.PaymentService == nil {
		return false, nil
	}
	seen := map[string]struct{}{}
	for _, candidate := range []string{inv.Charge, inv.PaymentIntent, inv.ID} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		seen[candidate] = struct{}{}
		existing, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, candidate)
		if err != nil {
			if repo.IsNotFound(err) {
				continue
			}
			return false, fmt.Errorf("dedup stripe invoice payment by transaction id: %w", err)
		}
		if existing != nil && existing.Status != payments.PaymentStatusFailedValue {
			return true, nil
		}
	}
	if invoiceID := strings.TrimSpace(inv.ID); invoiceID != "" {
		existing, err := s.PaymentService.GetByMetadataValue(ctx, "stripe_invoice_id", invoiceID)
		if err != nil {
			if repo.IsNotFound(err) {
				return false, nil
			}
			return false, fmt.Errorf("dedup stripe invoice payment by metadata: %w", err)
		}
		if existing != nil && existing.Status != payments.PaymentStatusFailedValue {
			return true, nil
		}
	}
	return false, nil
}

func stripeInvoiceHasNoRefundableTransaction(inv stripeInvoice) bool {
	return strings.TrimSpace(inv.Charge) == "" && strings.TrimSpace(inv.PaymentIntent) == ""
}

func ptrProcessor(p models.Processor) *models.Processor {
	return &p
}

func stripeInvoicePaymentMetadata(inv stripeInvoice) map[string]any {
	metadata := map[string]any{
		"stripe_invoice_id": strings.TrimSpace(inv.ID),
	}
	if chargeID := strings.TrimSpace(inv.Charge); chargeID != "" {
		metadata["stripe_charge_id"] = chargeID
	}
	if paymentIntentID := strings.TrimSpace(inv.PaymentIntent); paymentIntentID != "" {
		metadata["stripe_payment_intent_id"] = paymentIntentID
	}
	return metadata
}

func stripeCheckoutPaymentMetadata(sess stripeCheckoutSession) map[string]any {
	metadata := map[string]any{
		"stripe_checkout_session_id": strings.TrimSpace(sess.ID),
	}
	if paymentIntentID := strings.TrimSpace(sess.PaymentIntent); paymentIntentID != "" {
		metadata["stripe_payment_intent_id"] = paymentIntentID
	}
	return metadata
}
