package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/payments"
	paycharge "github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/purchasedcredits"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Memberships creates a paid recurring line's subscription in the caller's
// transaction.
type Memberships interface {
	CreateMembershipTx(ctx context.Context, txDB *db.DB, params *subscriptions.CreateMembershipParams) (*models.Subscription, []*models.NotificationQueue, error)
}

// Charge is money a provider took for an order, as its qualified receipt
// reports it.
type Charge struct {
	PaymentID       uuid.UUID
	AttemptID       uuid.UUID
	PSPID           uuid.UUID
	Rail            string
	TransactionID   string
	Amount          int64
	Currency        string
	PaymentMethodID uuid.UUID
	PurchasedAt     time.Time
	TokenType       string
	Metadata        map[string]any
	// Cites is the card's lineage the charge used; nil when it was its
	// agreement's storing transaction, referenced by StoringRef.
	Cites      *paycharge.Mandate
	StoringRef string
}

// lineage is the agreement references the charge leaves on the card.
func (c *Charge) lineage() *paycharge.Mandate {
	if c.Cites != nil {
		return c.Cites
	}
	return &paycharge.Mandate{InitialTransactionID: c.StoringRef}
}

// Outcome is what a charge did to its order.
type Outcome string

const (
	// OutcomePaid: the order is paid (now, or by an earlier delivery).
	OutcomePaid Outcome = "paid"
	// OutcomeRefunded: the order had closed and what it bought is owned
	// meanwhile; the payment is recorded and its refund queued.
	OutcomeRefunded Outcome = "refunded"
)

// Paid settles an order a charge paid, in d's transaction (the caller's
// provider-intent completion): the payment, the number, the claims handed
// to what the lines produce, fulfilment and the order.paid event. A charge
// on a canceled or expired order revives it while its claims are free;
// otherwise the payment is refunded with a finding.
func (s *Service) Paid(ctx context.Context, d *db.DB, orderID uuid.UUID, charge Charge) (Outcome, error) {
	if d == nil || d.Pool() != nil {
		return "", errors.New("an order is settled in its charge's transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	order, err := s.load(ctx, d.Gen(ctx), mid.UUID(), orderID, true)
	if err != nil {
		return "", err
	}
	if charge.Amount != order.Total || !strings.EqualFold(charge.Currency, order.Currency) || charge.PaymentID == uuid.Nil {
		return "", fmt.Errorf("order %s charge contradicts its total", orderID)
	}
	return s.settle(ctx, d, order, &charge)
}

// settle is Paid for a locked order; charge is nil for a free order.
func (s *Service) settle(ctx context.Context, d *db.DB, order *Order, charge *Charge) (Outcome, error) {
	q := d.Gen(ctx)
	now := s.now()
	if order.Status == string(billing.OrderPaid) {
		if charge != nil && uuidOf(order.PaymentID) != charge.PaymentID {
			return "", errLateApprovalDup
		}
		return OutcomePaid, nil
	}
	var payment *models.Payment
	if charge != nil {
		if order.PaymentID != nil {
			if *order.PaymentID != charge.PaymentID {
				return "", errLateApprovalDup
			}
			return OutcomeRefunded, nil
		}
		var err error
		if payment, err = s.recordPayment(ctx, d, order, charge); err != nil {
			return "", err
		}
	}
	if order.Status == string(billing.OrderCanceled) || order.Status == string(billing.OrderExpired) {
		free, err := s.reclaim(ctx, q, order, now)
		if err != nil {
			return "", err
		}
		if !free {
			if payment == nil {
				return "", fmt.Errorf("free order %s cannot be paid late", order.ID)
			}
			if _, err := q.SetOrderLatePayment(ctx, gen.SetOrderLatePaymentParams{MerchantID: order.MerchantID, ID: order.ID, PaymentID: payment.ID, Now: now}); err != nil {
				return "", err
			}
			if err := s.attemptDone(ctx, q, order, "succeeded", charge, now); err != nil {
				return "", err
			}
			return OutcomeRefunded, intents.RefundLateOrderPayment(ctx, d, payment, s.Clock)
		}
	}
	number, err := q.NextDocumentNumber(ctx, gen.NextDocumentNumberParams{MerchantID: order.MerchantID, Now: now})
	if err != nil {
		return "", err
	}
	var paymentID *uuid.UUID
	if payment != nil {
		paymentID = &payment.ID
	}
	if n, err := q.SetOrderPaid(ctx, gen.SetOrderPaidParams{MerchantID: order.MerchantID, ID: order.ID, Number: FormatNumber(number), PaymentID: paymentID, Now: now}); err != nil || n != 1 {
		if err == nil {
			err = fmt.Errorf("order %s did not move to paid", order.ID)
		}
		return "", err
	}
	recurring := false
	for _, line := range order.Lines {
		recurring = recurring || line.BillingIntervalHours != nil
		if err := s.fulfil(ctx, d, order, line, charge, now); err != nil {
			return "", fmt.Errorf("fulfil order line %s: %w", line.ID, err)
		}
	}
	// A one-time order's storing charge on a PSP vault is the card's
	// card-on-file consent; a Stripe card has its own from its setup.
	if charge != nil && !recurring && charge.Cites == nil && charge.Rail != string(models.RailStripe) {
		if err := mandates.RecordStored(ctx, q, mandates.Stored{MerchantID: order.MerchantID, CustomerID: order.CustomerID, PaymentMethodID: charge.PaymentMethodID, PSPID: charge.PSPID,
			Rail: charge.Rail, Currency: order.Currency, Lineage: *charge.lineage(), AcceptedAt: now}); err != nil {
			return "", err
		}
	}
	if _, err := q.ReleaseOrderClaims(ctx, gen.ReleaseOrderClaimsParams{MerchantID: order.MerchantID, CustomerID: order.CustomerID, OrderID: order.ID}); err != nil {
		return "", err
	}
	if err := s.attemptDone(ctx, q, order, "succeeded", charge, now); err != nil {
		return "", err
	}
	extra := map[string]any{"number": FormatNumber(number)}
	if paymentID != nil {
		extra["payment_id"] = paymentID.String()
	}
	return OutcomePaid, s.event(ctx, q, &order.BillingOrder, "order.paid", now, extra)
}

// FormatNumber spells a document number.
func FormatNumber(n int64) string { return fmt.Sprintf("%06d", n) }

// reclaim takes a closed order's claims again: it reports false when what a
// unique line buys is held meanwhile.
func (s *Service) reclaim(ctx context.Context, q *gen.Queries, order *Order, now time.Time) (bool, error) {
	for _, l := range order.Lines {
		if l.ClaimKey == nil {
			continue
		}
		n, err := q.ClaimOwnership(ctx, gen.ClaimOwnershipParams{MerchantID: order.MerchantID, CustomerID: order.CustomerID, ClaimKey: *l.ClaimKey, OrderID: order.ID, Now: now})
		if err != nil {
			return false, err
		}
		if n == 0 {
			held, err := q.GetOwnershipClaim(ctx, gen.GetOwnershipClaimParams{MerchantID: order.MerchantID, CustomerID: order.CustomerID, ClaimKey: *l.ClaimKey})
			if err != nil {
				return false, err
			}
			if held.OrderID != order.ID {
				return false, nil
			}
		}
	}
	held := &Quote{Lines: make([]QuotedLine, 0, len(order.Lines))}
	for _, l := range order.Lines {
		if l.ClaimKey == nil {
			continue
		}
		product, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: order.MerchantID, ID: l.ProductID})
		if err != nil {
			return false, err
		}
		p, err := models.ProductFromGen(product)
		if err != nil {
			return false, err
		}
		held.Lines = append(held.Lines, QuotedLine{Product: p, ClaimKey: *l.ClaimKey})
	}
	if err := s.refuseOwned(ctx, q, order.MerchantID, order.CustomerID, held, order.ID, now); err != nil {
		return false, err
	}
	for _, l := range held.Lines {
		if l.Refusal != nil {
			return false, nil
		}
	}
	return true, nil
}

// recordPayment writes the charge as the order's payment.
func (s *Service) recordPayment(ctx context.Context, d *db.DB, order *Order, charge *Charge) (*models.Payment, error) {
	psp := charge.PSPID
	kind := payments.AttemptInitial
	payment := &models.Payment{
		ID: charge.PaymentID, CustomerID: order.CustomerID, OrderID: &order.ID,
		Channel: models.ChannelRail, Rail: models.Rail(charge.Rail), PspID: &psp, TransactionID: charge.TransactionID,
		Amount: charge.Amount, ListAmount: order.Total, Currency: order.Currency, Status: payments.PaymentStatusSucceededValue,
		PurchasedAt: charge.PurchasedAt, CreatedAt: s.now(), AttemptKind: &kind, Metadata: charge.Metadata,
		MoneyMovement: models.MoneyMovementRail,
	}
	if charge.TokenType != "" {
		payment.TokenType = &charge.TokenType
	}
	if err := payments.NewPaymentService(d, s.Clock).Create(ctx, payment); err != nil {
		return nil, err
	}
	return payment, nil
}

// fulfil delivers one paid line: a subscription for a recurring line, a
// credit lot for a credit product, product access otherwise.
func (s *Service) fulfil(ctx context.Context, d *db.DB, order *Order, line gen.BillingOrderLine, charge *Charge, now time.Time) error {
	q := d.Gen(ctx)
	var holderType string
	var holder uuid.UUID
	switch {
	case line.BillingIntervalHours != nil:
		if charge == nil || s.Memberships == nil {
			return errors.New("a recurring line needs its charge and the membership service")
		}
		interval := time.Duration(*line.BillingIntervalHours) * time.Hour
		terms := subscriptions.InitialMembershipTerms{
			CollectionPolicy: models.CollectionPolicyEngine, SubscriptionID: uuidutil.NewV7(), CustomerID: order.CustomerID, PSPID: charge.PSPID,
			ProductID: line.ProductID, PriceID: line.PriceID, PaymentMethodID: charge.PaymentMethodID, ProductName: line.Description,
			RecurringAmount: line.Amount, Currency: order.Currency, AccessDurationHours: intPtr(line.AccessDurationHours),
			AcceptedAt: now, PeriodStart: now, PeriodEnd: now.Add(interval), Quantity: seats(line.Quantity),
		}
		sub, _, err := s.Memberships.CreateMembershipTx(db.WithPSPID(ctx, charge.PSPID), d, &subscriptions.CreateMembershipParams{
			Prepared: &terms, UserID: order.CustomerID.String(), PriceID: line.PriceID, Rail: models.Rail(charge.Rail), PurchasedAt: &now,
			PaymentMetadata: map[string]any{"order_id": order.ID.String()},
		})
		if err != nil {
			return err
		}
		if _, err := mandates.Create(ctx, q, mandates.Agreement{MerchantID: order.MerchantID, CustomerID: order.CustomerID, PaymentMethodID: charge.PaymentMethodID, PSPID: charge.PSPID,
			Rail: charge.Rail, Kind: paycharge.AgreementRecurring, SubscriptionID: &sub.ID, Lineage: charge.lineage(), AcceptedAt: now}); err != nil {
			return err
		}
		holderType, holder = "subscription", sub.ID
		if err := q.SetOrderLineProduced(ctx, gen.SetOrderLineProducedParams{MerchantID: order.MerchantID, ID: line.ID, SubscriptionID: &sub.ID}); err != nil {
			return err
		}
	case len(line.CreditGrant) > 0 && string(line.CreditGrant) != "null":
		credit, err := creditOf(line)
		if err != nil {
			return err
		}
		if charge == nil {
			return errors.New("a credit line needs its charge")
		}
		expires := now.AddDate(0, 0, credit.ExpiresAfterDays)
		if _, err := purchasedcredits.New(d, s.Clock).Fund(ctx, purchasedcredits.Params{
			CustomerID: identity.CustomerID(order.CustomerID), PaymentID: charge.PaymentID, ProductID: line.ProductID, OrderLineID: line.ID,
			Amount: credit.Amount * int64(seats(line.Quantity)), PaidAmount: line.Amount, Currency: credit.Currency, StartsAt: now, ExpiresAt: &expires,
			Description: line.Description,
		}); err != nil {
			return err
		}
	default:
		params := entitlements.PushAccessParams{UserID: order.CustomerID.String(), CustomerID: order.CustomerID, ProductID: line.ProductID,
			SourceType: models.AccessSourcePurchase, SourceID: line.ID.String()}
		if charge != nil {
			params.PaymentID = &charge.PaymentID
		}
		if line.AccessDurationHours != nil {
			duration := time.Duration(*line.AccessDurationHours) * time.Hour * time.Duration(seats(line.Quantity))
			params.Duration = &duration
		} else {
			params.Indefinite = true
		}
		access, err := entitlements.NewEntitlementService(d, s.Clock).PushAccess(ctx, params)
		if err != nil {
			return err
		}
		if access != nil {
			holderType, holder = "product_access", access.ID
			if err := q.SetOrderLineProduced(ctx, gen.SetOrderLineProducedParams{MerchantID: order.MerchantID, ID: line.ID, ProductAccessID: &access.ID}); err != nil {
				return err
			}
		}
	}
	if line.ClaimKey == nil || holderType == "" || catalog.Ownership(line.Ownership) != catalog.OwnershipUnique {
		return nil
	}
	_, err := q.HandOverOwnershipClaim(ctx, gen.HandOverOwnershipClaimParams{MerchantID: order.MerchantID, CustomerID: order.CustomerID,
		ClaimKey: *line.ClaimKey, OrderID: order.ID, HolderType: holderType, HolderID: holder, Now: now})
	return err
}

// seats is a line's quantity as a count; a recurring line without seats bills
// one unit.
func seats(quantity *int32) int {
	if quantity == nil {
		return 1
	}
	return int(*quantity)
}

// attemptDone ends the order's live attempt.
func (s *Service) attemptDone(ctx context.Context, q *gen.Queries, order *Order, status string, charge *Charge, now time.Time) error {
	if charge == nil || charge.AttemptID == uuid.Nil {
		return nil
	}
	params := gen.SetOrderAttemptStatusParams{MerchantID: order.MerchantID, ID: charge.AttemptID, Status: status, Now: now}
	if status == "succeeded" {
		params.PaymentID, params.TransactionID = &charge.PaymentID, &charge.TransactionID
	}
	_, err := q.SetOrderAttemptStatus(ctx, params)
	return err
}

// Declined records a definite decline of the order's attempt, in d's
// transaction: the order is open again with the reason.
func (s *Service) Declined(ctx context.Context, d *db.DB, orderID, attemptID uuid.UUID, failure *billing.PaymentFailure) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := d.Gen(ctx)
	order, err := s.load(ctx, q, mid.UUID(), orderID, true)
	if err != nil {
		return err
	}
	now := s.now()
	if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: order.MerchantID, ID: attemptID, Status: "failed", Now: now}); err != nil {
		return err
	}
	if order.AttemptID == nil || *order.AttemptID != attemptID || !order.Live() {
		return nil
	}
	if failure == nil {
		failure = &billing.PaymentFailure{Reason: "generic_decline", Message: "The payment was declined."}
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	if _, err := q.SetOrderDeclined(ctx, gen.SetOrderDeclinedParams{MerchantID: order.MerchantID, ID: order.ID, AttemptID: attemptID, LastPaymentError: raw, Now: now}); err != nil {
		return err
	}
	return s.event(ctx, q, &order.BillingOrder, "order.payment_failed", now, map[string]any{"reason": failure.Reason})
}

// Pending records that the order's live attempt waits for the customer
// (requires_action) or the provider (processing).
func (s *Service) Pending(ctx context.Context, orderID, attemptID uuid.UUID, status billing.OrderStatus) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.DB.Gen(ctx)
	now := s.now()
	n, err := q.SetOrderPending(ctx, gen.SetOrderPendingParams{MerchantID: mid.UUID(), ID: orderID, AttemptID: attemptID, Status: string(status), Now: now})
	if err != nil || n == 0 {
		return err
	}
	if _, err := q.SetOrderAttemptStatus(ctx, gen.SetOrderAttemptStatusParams{MerchantID: mid.UUID(), ID: attemptID, Status: string(status), Now: now}); err != nil {
		return err
	}
	if status != billing.OrderRequiresAction {
		return nil
	}
	order, err := s.load(ctx, q, mid.UUID(), orderID, false)
	if err != nil {
		return err
	}
	return s.event(ctx, q, &order.BillingOrder, "order.requires_action", now, nil)
}
