// Package orders is the purchase: frozen lines and totals bought by one
// customer, ownership claims that keep two buys of one thing from both
// succeeding, the merchant's gapless document numbers, and fulfilment when an
// order is paid. Charging is checkout's: an order's attempts are checkout
// attempts, and their provider intents settle the order through Paid.
package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/orders/usage"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

const (
	// CustomerTTL is how long a customer's own order takes payment.
	CustomerTTL = 24 * time.Hour
	// MerchantTTL is a merchant order's default lifetime, MaxMerchantTTL its
	// longest (Stripe's invoice ceiling).
	MerchantTTL    = 30 * 24 * time.Hour
	MaxMerchantTTL = 120 * 24 * time.Hour
	// MaxQuantity bounds one line.
	MaxQuantity = 10_000
	// PurgeAfter is how long an unpaid closed order without attempts is kept.
	PurgeAfter = 90 * 24 * time.Hour
)

var (
	ErrNotFound        = apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "Order not found.")
	ErrNotPayable      = apperr.New(http.StatusConflict, billing.CodeOrderNotPayable, "The order takes no payment.")
	ErrNotCancelable   = apperr.New(http.StatusConflict, billing.CodeOrderNotCancelable, "The order cannot be canceled.")
	ErrRenewalCancel   = apperr.New(http.StatusConflict, billing.CodeOrderNotCancelable, "A renewal is not canceled; cancel its subscription.")
	ErrInProgress      = apperr.New(http.StatusConflict, billing.CodeOrderPaymentInProgress, "A payment on this order is unresolved.")
	ErrTotalChanged    = apperr.New(http.StatusConflict, billing.CodeOrderTotalChanged, "The order's total is not expected_total.")
	ErrOptionMissing   = apperr.New(http.StatusUnprocessableEntity, billing.CodePaymentOptionUnavailable, "No PSP that can take these lines accepts this payment.")
	ErrRecurringLine   = apperr.New(http.StatusConflict, billing.CodeOrderHasRecurringLine, "An order with a recurring line is paid by its customer; it cannot be recorded as paid.")
	ErrAmountNotTotal  = apperr.New(http.StatusConflict, billing.CodePaymentExceedsDue, "A recorded payment must be the order's total.")
	errLateApprovalDup = errors.New("order payment contradicts the recorded one")
)

// LineError refuses a line of a create or pay: already owned (409) or not
// buyable (422). Param is lines[i].
type LineError struct {
	Index   int
	Refusal billing.OrderLineRefusal
}

func (e *LineError) Error() string {
	return fmt.Sprintf("lines[%d]: %s", e.Index, e.Refusal.Message)
}

// Code is the wire error code.
func (e *LineError) Code() string {
	switch e.Refusal.Code {
	case RefusalAlreadyOwned:
		return billing.CodeAlreadyOwned
	case RefusalQuantityNotAllowed:
		return billing.CodeQuantityNotAllowed
	}
	return billing.CodeOrderLineUnavailable
}

// Line refusal codes.
const (
	RefusalAlreadyOwned     = "already_owned"
	RefusalUnavailable      = "unavailable"
	RefusalCurrencyMismatch = "currency_mismatch"
	RefusalQuantityInvalid  = "quantity_invalid"
	// A chosen amount: on a fixed price, missing, or out of bounds.
	RefusalAmountNotAllowed = "amount_not_allowed"
	RefusalAmountRequired   = "amount_required"
	RefusalAmountInvalid    = "amount_invalid"
	// RefusalQuantityNotAllowed is also the line's error code.
	RefusalQuantityNotAllowed = billing.CodeQuantityNotAllowed
)

// Service reads and writes orders for the context's merchant.
type Service struct {
	DB    *db.DB
	Clock clockwork.Clock
	// Memberships creates the subscription a paid recurring line produces.
	Memberships Memberships
	// Abandon closes, at the provider, the attempt of an order that closed
	// while its payment awaited the customer.
	Abandon func(ctx context.Context, attemptID uuid.UUID) error
}

// New is the orders service.
func New(database *db.DB, memberships Memberships, clocks ...clockwork.Clock) *Service {
	return &Service{DB: database, Memberships: memberships, Clock: timeutil.FirstClock(clocks...)}
}

func (s *Service) now() time.Time {
	return s.Clock.Now().UTC().Truncate(time.Microsecond)
}

// Order is one stored order with its lines.
type Order struct {
	gen.BillingOrder
	Lines           []gen.BillingOrderLine
	PriceReferences map[uuid.UUID]billing.PriceID
}

// Payable reports an open order not past its expiry (a renewal has none).
func Payable(o gen.BillingOrder, now time.Time) bool {
	return o.Status == string(billing.OrderOpen) && (o.ExpiresAt == nil || o.ExpiresAt.After(now))
}

// Live reports an order that may still take payment.
func (o *Order) Live() bool {
	return o.Status == string(billing.OrderOpen) || o.Status == string(billing.OrderProcessing)
}

// AwaitsCustomer reports an open order whose payment waits for the customer's
// action.
func (o *Order) AwaitsCustomer() bool {
	return o.Status == string(billing.OrderOpen) && o.PaymentStatus == string(billing.OrderPaymentRequiresAction)
}

// HasRecurring reports a line that renews.
// recurringLines counts the order's subscriptions.
func (o *Order) HasRecurring() bool {
	for _, l := range o.Lines {
		if l.BillingIntervalHours != nil {
			return true
		}
	}
	return false
}

// HasOneOff reports a line bought once: the order's payment is a one-off
// purchase with its own receipt, beside any subscription it starts.
func (o *Order) HasOneOff() bool {
	for _, l := range o.Lines {
		if l.BillingIntervalHours == nil {
			return true
		}
	}
	return false
}

// Items names what the order bought, as its receipt does.
func (o *Order) Items() string {
	names := make([]string, 0, len(o.Lines))
	for _, l := range o.Lines {
		if l.Quantity != nil && *l.Quantity > 1 {
			names = append(names, fmt.Sprintf("%s × %d", l.Description, *l.Quantity))
			continue
		}
		names = append(names, l.Description)
	}
	return strings.Join(names, ", ")
}

// View is the order as its customer reads it. nextAction and options are
// the caller's: they come from the live attempt and the routing.
func (o *Order) View(nextAction *billing.NextAction, options []billing.OrderPaymentOption) billing.Order {
	out := billing.Order{
		ID: billing.OrderID(o.ID), CustomerID: billing.CustomerID(o.CustomerID), Origin: billing.OrderOrigin(o.Origin),
		Status: billing.OrderStatus(o.Status), Number: o.Number, Currency: o.Currency, Total: o.Total,
		Lines: make([]billing.OrderLine, 0, len(o.Lines)), PaymentOptions: []billing.OrderPaymentOption{},
		Payment:   billing.OrderPayment{Status: billing.OrderPaymentStatus(o.PaymentStatus)},
		ExpiresAt: o.ExpiresAt, CompletedAt: o.CompletedAt, CanceledAt: o.CanceledAt, ExpiredAt: o.ExpiredAt, VoidedAt: o.VoidedAt,
		UncollectibleAt: o.UncollectibleAt, CreatedAt: o.CreatedAt,
	}
	if o.SubscriptionID != nil && o.PeriodStartsAt != nil && o.PeriodEndsAt != nil && o.DueAt != nil {
		out.Renewal = &billing.OrderRenewal{SubscriptionID: billing.SubscriptionID(*o.SubscriptionID), PeriodStartsAt: *o.PeriodStartsAt, PeriodEndsAt: *o.PeriodEndsAt, DueAt: *o.DueAt}
	}
	if o.Origin == string(billing.OrderOriginUsage) {
		out.Usage = usageView(o.BillingOrder)
	}
	if o.AwaitsCustomer() {
		out.Payment.NextAction = nextAction
	}
	if o.CheckoutExpiresAt != nil && o.CheckoutSuccessUrl != nil && o.CheckoutSavedPaymentMethods != nil && o.CheckoutUiMode != nil {
		out.Checkout = &billing.OrderCheckout{UIMode: billing.CheckoutUIMode(*o.CheckoutUiMode), ExpiresAt: o.CheckoutExpiresAt.UTC(), SuccessURL: *o.CheckoutSuccessUrl,
			CancelURL: o.CheckoutCancelUrl, SavedPaymentMethods: *o.CheckoutSavedPaymentMethods}
	}
	if o.Status == string(billing.OrderOpen) && options != nil {
		out.PaymentOptions = options
	}
	if len(o.LastPaymentError) > 0 {
		var failure billing.PaymentFailure
		if json.Unmarshal(o.LastPaymentError, &failure) == nil {
			out.Payment.LastPaymentError = &failure
		}
	}
	if o.PaymentMethodID != nil {
		id := billing.PaymentMethodID(*o.PaymentMethodID)
		out.Payment.PaymentMethodID = &id
	}
	if o.PaymentID != nil {
		id := billing.PaymentID(*o.PaymentID)
		out.Payment.PaymentID = &id
	}
	for _, l := range o.Lines {
		line := billing.OrderLine{ID: billing.OrderLineID(l.ID), Description: l.Description, Quantity: intPtr(l.Quantity), UnitAmount: l.UnitAmount, Amount: l.Amount,
			BillingIntervalHours: intPtr(l.BillingIntervalHours), AccessDurationHours: intPtr(l.AccessDurationHours), Source: l.Source}
		if l.PriceID != nil && l.ProductID != nil && l.Ownership != nil {
			price, product, ownership := o.PriceReferences[*l.PriceID], billing.ProductID(*l.ProductID), catalog.Ownership(*l.Ownership)
			line.PriceID, line.ProductID, line.Ownership = &price, &product, &ownership
		}
		if l.SubscriptionID != nil {
			id := billing.SubscriptionID(*l.SubscriptionID)
			line.SubscriptionID = &id
		}
		if l.ProductAccessID != nil {
			id := billing.ProductAccessID(*l.ProductAccessID)
			line.ProductAccessID = &id
		}
		out.Lines = append(out.Lines, line)
	}
	return out
}

func usageView(o gen.BillingOrder) *billing.OrderUsage {
	out := &billing.OrderUsage{AmountPaid: usage.Paid(o), AmountDue: usage.Due(o), NextCollectionAttemptAt: o.NextCollectionAttemptAt,
		PONumber: o.PoNumber, Memo: o.Memo, Tax: map[string]any{}, BillingContacts: []billing.InvoiceContact{}}
	if o.PeriodStartsAt != nil && o.PeriodEndsAt != nil && o.DueAt != nil {
		out.PeriodStartsAt, out.PeriodEndsAt, out.DueAt = *o.PeriodStartsAt, *o.PeriodEndsAt, *o.DueAt
	}
	if o.Status == string(billing.OrderVoid) {
		out.AmountDue = 0
	}
	if o.CollectionMethod != nil {
		out.CollectionMethod = billing.InvoiceCollectionMethod(*o.CollectionMethod)
	}
	if len(o.Tax) > 0 {
		_ = json.Unmarshal(o.Tax, &out.Tax)
	}
	if len(o.BillingContacts) > 0 {
		_ = json.Unmarshal(o.BillingContacts, &out.BillingContacts)
	}
	return out
}

func intPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	out := int(*v)
	return &out
}

func int32Ptr(v *int) (*int32, error) {
	if v == nil {
		return nil, nil
	}
	if *v < math.MinInt32 || *v > math.MaxInt32 {
		return nil, fmt.Errorf("order line value %d is out of range", *v)
	}
	out := int32(*v)
	return &out, nil
}

// creditOf reads a line's frozen credit benefit.
func creditOf(l gen.BillingOrderLine) (*models.CreditGrantSnapshot, error) {
	if len(l.CreditGrant) == 0 || string(l.CreditGrant) == "null" {
		return nil, nil
	}
	var out models.CreditGrantSnapshot
	if err := json.Unmarshal(l.CreditGrant, &out); err != nil {
		return nil, err
	}
	return &out, out.Validate()
}

// uuidOf reads an optional typed id.
func uuidOf(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}
