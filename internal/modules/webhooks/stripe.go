package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/credits"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/normalize"
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
	PurchaseRegistrar            stripePurchaseRegistrar
	PaymentService               *payments.PaymentService
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
}

type stripeInvoice struct {
	ID            string            `json:"id"`
	Subscription  string            `json:"subscription"`
	Customer      string            `json:"customer"`
	CustomerEmail string            `json:"customer_email"`
	PaymentIntent string            `json:"payment_intent"`
	Charge        string            `json:"charge"`
	AmountPaid    int64             `json:"amount_paid"`
	Currency      string            `json:"currency"`
	Metadata      map[string]string `json:"metadata"`
	Lines         struct {
		Data []stripeInvoiceLineItem `json:"data"`
	} `json:"lines"`
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
	Amount         int64  `json:"amount"`
	AmountRefunded int64  `json:"amount_refunded"`
	Currency       string `json:"currency"`
	Refunds        struct {
		Data []stripeRefund `json:"data"`
	} `json:"refunds"`
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
	case "charge.refunded":
		return s.handleChargeRefunded(ctx, obj)
	case "charge.dispute.created", "charge.dispute.closed":
		return s.handleDispute(ctx, obj)
	default:
		log.WithField("event_type", eventType).Info("stripe webhook ignored (not handled)")
		return nil
	}
}

func (s *StripeWebhookService) handleInvoicePaid(ctx context.Context, obj json.RawMessage) error {
	var inv stripeInvoice
	if err := json.Unmarshal(obj, &inv); err != nil {
		return fmt.Errorf("parse invoice: %w", err)
	}
	userID := normalize.FirstNonEmpty(inv.Metadata["user_id"], inv.Metadata["userId"], inv.Metadata["uid"])
	if userID == "" {
		return fmt.Errorf("stripe invoice missing user_id metadata")
	}
	if s.ProcessorCustomerService != nil {
		customerID := strings.TrimSpace(inv.Customer)
		if customerID != "" {
			_ = s.ProcessorCustomerService.Upsert(ctx, userID, string(models.ProcessorStripe), customerID)
		}
	}
	priceID, price, err := s.resolvePriceFromMetadata(ctx, inv.Metadata, inv.Lines.Data)
	if err != nil {
		return err
	}
	paymentTransactionID := normalize.FirstNonEmpty(inv.Charge, inv.PaymentIntent)
	if paymentTransactionID == "" {
		if inv.AmountPaid != 0 || strings.TrimSpace(inv.ID) == "" {
			return fmt.Errorf("stripe invoice missing refundable transaction id (charge/payment_intent)")
		}
		paymentTransactionID = strings.TrimSpace(inv.ID)
	}
	paymentMetadata := stripeInvoicePaymentMetadata(inv)

	processorSubID := strings.TrimSpace(inv.Subscription)
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

	if price == nil {
		price, err = s.PriceService.GetByID(ctx, priceID)
		if err != nil {
			return fmt.Errorf("price lookup failed: %w", err)
		}
	}
	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return fmt.Errorf("product lookup failed: %w", err)
	}

	if s.CreditsService != nil && len(product.CreditsSpec) > 0 {
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
		sessionID := parseCheckoutSessionID(inv.Metadata)
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

	// Grant purchased credits based on the product's credits_spec.
	if price == nil {
		price, err = s.PriceService.GetByID(ctx, priceID)
		if err != nil {
			return fmt.Errorf("price lookup failed: %w", err)
		}
	}
	product, err := s.ProductService.GetByID(ctx, price.ProductID)
	if err != nil {
		return fmt.Errorf("product lookup failed: %w", err)
	}
	if len(product.CreditsSpec) > 0 && s.CreditsService != nil {
		for creditType, spec := range product.CreditsSpec {
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
				UserID:     userID,
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

	if len(data.Items.Data) > 0 {
		stripePrice := strings.TrimSpace(data.Items.Data[0].Price.ID)
		if stripePrice != "" {
			price, err := s.PriceService.GetByStripePriceID(ctx, stripePrice)
			if err == nil {
				sub.PriceID = price.ID
				sub.ProductID = price.ProductID
				sub.ScheduledPriceID = nil
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

	applyStripeSubscriptionStatus(sub, data.Status, s.now().UTC())

	if err := s.SubscriptionService.Update(ctx, sub); err != nil {
		return fmt.Errorf("update subscription from stripe: %w", err)
	}
	return nil
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

func (s *StripeWebhookService) handleInvoicePaymentFailed(ctx context.Context, obj json.RawMessage) error {
	var inv stripeInvoice
	if err := json.Unmarshal(obj, &inv); err != nil {
		return fmt.Errorf("parse failed invoice: %w", err)
	}
	processorSubID := strings.TrimSpace(inv.Subscription)
	if processorSubID == "" {
		return nil
	}
	sub, err := s.SubscriptionService.GetByProcessorSubscriptionID(ctx, string(models.ProcessorStripe), processorSubID)
	if err != nil {
		return nil
	}
	sub.Status = models.StatusPastDue
	if err := s.SubscriptionService.Update(ctx, sub); err != nil {
		return fmt.Errorf("update stripe subscription after payment failure: %w", err)
	}
	return nil
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
	refundID := strings.TrimSpace(refund.ID)
	if refundID == "" || refund.Amount <= 0 {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe refund missing id or amount"))
	}
	if existing, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, refundID); err == nil && existing != nil {
		return nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup stripe refund payment: %w", err)
	}
	original, err := s.lookupStripeOriginalPayment(ctx, refund.Charge, refund.PaymentIntent)
	if err != nil {
		return err
	}
	if _, err := s.PaymentService.Refund(ctx, original.ID, refundID, refund.Amount); err != nil {
		return fmt.Errorf("record stripe refund: %w", err)
	}
	if original.SubscriptionID != nil && refund.Amount >= original.Amount && s.SubscriptionLifecycleService != nil {
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
	}
	return nil
}

func (s *StripeWebhookService) handleDispute(ctx context.Context, obj json.RawMessage) error {
	var dispute stripeDispute
	if err := json.Unmarshal(obj, &dispute); err != nil {
		return fmt.Errorf("parse stripe dispute: %w", err)
	}
	if s.PaymentService == nil {
		return fmt.Errorf("payment service is required for stripe dispute")
	}
	disputeID := strings.TrimSpace(dispute.ID)
	if disputeID == "" || dispute.Amount <= 0 {
		return MarkWebhookErrorNonRetryable(fmt.Errorf("stripe dispute missing id or amount"))
	}
	if existing, err := s.PaymentService.GetByTransactionID(ctx, models.ProcessorStripe, disputeID); err == nil && existing != nil {
		return nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup stripe dispute payment: %w", err)
	}
	original, err := s.lookupStripeOriginalPayment(ctx, dispute.Charge, dispute.PaymentIntent)
	if err != nil {
		return err
	}
	if _, err := s.PaymentService.Refund(ctx, original.ID, disputeID, dispute.Amount); err != nil {
		return fmt.Errorf("record stripe dispute reversal: %w", err)
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
	}
	return nil
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
		if !errors.Is(err, sql.ErrNoRows) {
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
	if len(lines) > 0 && lines[0].Price.ID != "" {
		price, err := s.PriceService.GetByStripePriceID(ctx, lines[0].Price.ID)
		if err != nil {
			return uuid.Nil, nil, fmt.Errorf("stripe price not mapped")
		}
		return price.ID, price, nil
	}
	return uuid.Nil, nil, fmt.Errorf("unable to resolve price")
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
