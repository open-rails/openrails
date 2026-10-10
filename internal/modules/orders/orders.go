// Package orders is the purchase (#1168): frozen lines and totals bought by
// one customer, ownership claims that keep two buys of one thing from both
// succeeding, the merchant's gapless document numbers, and fulfilment when an
// order is paid. Charging is checkout's: an order's attempts are checkout
// attempts, and their provider intents settle the order through Paid.
package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

const (
	// CustomerTTL is how long a customer's own order takes payment.
	CustomerTTL = 24 * time.Hour
	// MaxQuantity bounds one line.
	MaxQuantity = 10_000
	// PurgeAfter is how long an unpaid closed order without attempts is kept.
	PurgeAfter = 90 * 24 * time.Hour
)

var (
	ErrNotFound        = apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "Order not found.")
	ErrNotPayable      = apperr.New(http.StatusConflict, billing.CodeOrderNotPayable, "The order takes no payment.")
	ErrNotCancelable   = apperr.New(http.StatusConflict, billing.CodeOrderNotCancelable, "The order cannot be canceled.")
	ErrInProgress      = apperr.New(http.StatusConflict, billing.CodeOrderPaymentInProgress, "A payment on this order is unresolved.")
	ErrTotalChanged    = apperr.New(http.StatusConflict, billing.CodeOrderTotalChanged, "The order's total is not expected_total.")
	ErrOptionMissing   = apperr.New(http.StatusUnprocessableEntity, billing.CodePaymentOptionUnavailable, "No PSP that can take these lines accepts this payment.")
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
	Lines []gen.BillingOrderLine
}

// Live reports an order that may still take payment.
func (o *Order) Live() bool {
	return o.Status == string(billing.OrderOpen) || o.Status == string(billing.OrderRequiresAction) || o.Status == string(billing.OrderProcessing)
}

// HasRecurring reports a line that renews.
func (o *Order) HasRecurring() bool {
	for _, l := range o.Lines {
		if l.BillingIntervalHours != nil {
			return true
		}
	}
	return false
}

// View is the order as its customer reads it. nextAction and options are
// the caller's: they come from the live attempt and the routing.
func (o *Order) View(nextAction *billing.NextAction, options []billing.OrderPaymentOption) billing.Order {
	out := billing.Order{
		ID: billing.OrderID(o.ID), CustomerID: billing.CustomerID(o.CustomerID), Origin: billing.OrderOrigin(o.Origin),
		Status: billing.OrderStatus(o.Status), Number: o.Number, Currency: o.Currency, Total: o.Total,
		Lines: make([]billing.OrderLine, 0, len(o.Lines)), PaymentOptions: []billing.OrderPaymentOption{},
		ExpiresAt: o.ExpiresAt, PaidAt: o.PaidAt, CanceledAt: o.CanceledAt, ExpiredAt: o.ExpiredAt, CreatedAt: o.CreatedAt,
	}
	if o.Status == string(billing.OrderRequiresAction) {
		out.NextAction = nextAction
	}
	if o.Status == string(billing.OrderOpen) && options != nil {
		out.PaymentOptions = options
	}
	if len(o.LastPaymentError) > 0 {
		var failure billing.PaymentFailure
		if json.Unmarshal(o.LastPaymentError, &failure) == nil {
			out.LastPaymentError = &failure
		}
	}
	if o.PaymentMethodID != nil {
		id := billing.PaymentMethodID(*o.PaymentMethodID)
		out.PaymentMethodID = &id
	}
	if o.PaymentID != nil {
		id := billing.PaymentID(*o.PaymentID)
		out.PaymentID = &id
	}
	for _, l := range o.Lines {
		line := billing.OrderLine{ID: billing.OrderLineID(l.ID), PriceID: billing.PriceID(l.PriceID), ProductID: billing.ProductID(l.ProductID),
			Description: l.Description, Quantity: intPtr(l.Quantity), UnitAmount: l.UnitAmount, Amount: l.Amount,
			Ownership: catalog.Ownership(l.Ownership), BillingIntervalHours: intPtr(l.BillingIntervalHours), AccessDurationHours: intPtr(l.AccessDurationHours)}
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

func intPtr(v *int32) *int {
	if v == nil {
		return nil
	}
	out := int(*v)
	return &out
}

func int32Ptr(v *int) *int32 {
	if v == nil {
		return nil
	}
	out := int32(*v)
	return &out
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
