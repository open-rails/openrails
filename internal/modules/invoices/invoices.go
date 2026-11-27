// Package invoices is the purchase: frozen lines and totals bought by one
// customer, ownership claims that keep two buys of one thing from both
// succeeding, the merchant's gapless document numbers, and fulfilment when an
// invoice is paid. Charging is purchase's: an invoice's attempts are purchase
// attempts, and their provider intents settle the invoice through Paid.
package invoices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/invoices/usage"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

const (
	// CustomerTTL is how long a customer's own purchase takes payment
	// (Stripe Checkout's session); no other invoice expires.
	CustomerTTL = 24 * time.Hour
	// MaxQuantity bounds one line.
	MaxQuantity = 10_000
)

var (
	ErrNotFound        = apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "Invoice not found.")
	ErrNotPayable      = apperr.New(http.StatusConflict, billing.CodeInvoiceNotPayable, "The invoice takes no payment.")
	ErrNotCancelable   = apperr.New(http.StatusConflict, billing.CodeInvoiceNotCancelable, "The invoice cannot be canceled.")
	ErrRenewalCancel   = apperr.New(http.StatusConflict, billing.CodeInvoiceNotCancelable, "A renewal is not canceled; cancel its subscription.")
	ErrInProgress      = apperr.New(http.StatusConflict, billing.CodeInvoicePaymentInProgress, "A payment on this invoice is unresolved.")
	ErrTotalChanged    = apperr.New(http.StatusConflict, billing.CodeInvoiceTotalChanged, "The invoice's total is not expected_total.")
	ErrOptionMissing   = apperr.New(http.StatusUnprocessableEntity, billing.CodePaymentOptionUnavailable, "No PSP that can take these lines accepts this payment.")
	ErrRecurringLine   = apperr.New(http.StatusConflict, billing.CodeInvoiceHasRecurringLine, "An invoice with a recurring line is paid by its customer; it cannot be recorded as paid.")
	ErrAmountNotTotal  = apperr.New(http.StatusConflict, billing.CodePaymentExceedsDue, "A recorded payment must equal the invoice's total.")
	errLateApprovalDup = errors.New("invoice payment contradicts the recorded one")
)

// LineError refuses a line of a create or pay: already owned (409), sold
// only to holders of entitlements the customer lacks (403) or not buyable
// (422). Param is lines[i].
type LineError struct {
	Index   int
	Refusal billing.InvoiceLineRefusal
}

func (e *LineError) Error() string {
	return fmt.Sprintf("lines[%d]: %s", e.Index, e.Refusal.Message)
}

// HeldBy is the customer's other invoice whose claim refused the line: the
// invoice of other lines FindOrCreate replaces when it can.
func (e *LineError) HeldBy() (uuid.UUID, bool) {
	if e.Refusal.Code != RefusalAlreadyOwned || e.Refusal.OwnedBy == nil {
		return uuid.Nil, false
	}
	id, err := billing.ParseInvoiceID(*e.Refusal.OwnedBy)
	return id.UUID(), err == nil
}

// Code is the wire error code.
func (e *LineError) Code() billing.ErrorCode {
	switch e.Refusal.Code {
	case RefusalAlreadyOwned:
		return billing.CodeAlreadyOwned
	case RefusalQuantityNotAllowed:
		return billing.CodeQuantityNotAllowed
	case RefusalEntitlementRequired:
		return billing.CodeEntitlementRequired
	}
	return billing.CodeInvoiceLineUnavailable
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
	RefusalQuantityNotAllowed = string(billing.CodeQuantityNotAllowed)
)

// Service reads and writes invoices for the context's merchant.
type Service struct {
	DB    *db.DB
	Clock clockwork.Clock
	// Memberships creates the subscription a paid recurring line produces.
	Memberships *subscriptions.SubscriptionLifecycleService
	// Abandon closes, at the provider, the attempt of an invoice that closed
	// while its payment awaited the customer.
	Abandon func(ctx context.Context, attemptID uuid.UUID) error
	Log     *slog.Logger
}

// New is the invoices service.
func New(database *db.DB, memberships *subscriptions.SubscriptionLifecycleService, clocks ...clockwork.Clock) *Service {
	return &Service{DB: database, Memberships: memberships, Clock: timeutil.FirstClock(clocks...), Log: database.Log}
}

func (s *Service) now() time.Time {
	return s.Clock.Now().UTC().Truncate(time.Microsecond)
}

// Invoice is one stored invoice with its lines.
type Invoice struct {
	gen.BillingInvoice
	Lines           []gen.BillingInvoiceLine
	PriceReferences map[uuid.UUID]billing.PriceID
	ProviderBills   []billing.InvoiceProviderBill
}

// Payable reports an engine-collected open invoice not past its expiry.
func Payable(o gen.BillingInvoice, now time.Time) bool {
	return o.CollectionOwner == string(billing.InvoiceCollectionEngine) && o.Status == string(billing.InvoiceOpen) && !Expired(o, now)
}

// Expired reports an engine-collected open invoice past its expiry: closed,
// whether or not the expiry pass has written it yet.
func Expired(o gen.BillingInvoice, now time.Time) bool {
	return o.CollectionOwner == string(billing.InvoiceCollectionEngine) && o.Status == string(billing.InvoiceOpen) && o.ExpiresAt != nil && !o.ExpiresAt.After(now)
}

// AsOf is the invoice as read at now: one past its expiry reads as the expiry
// pass leaves it.
func (o *Invoice) AsOf(now time.Time) *Invoice {
	if Expired(o.BillingInvoice, now) {
		at := *o.ExpiresAt
		o.Status, o.PaymentStatus, o.ExpiredAt = string(billing.InvoiceExpired), string(billing.InvoicePaymentRequiresPaymentMethod), &at
	}
	return o
}

// Live reports an invoice that may still take payment.
func (o *Invoice) Live() bool {
	return o.Status == string(billing.InvoiceOpen) || o.Status == string(billing.InvoiceProcessing)
}

// AwaitsCustomer reports an open invoice whose payment waits for the customer's
// action.
func (o *Invoice) AwaitsCustomer() bool {
	return o.Status == string(billing.InvoiceOpen) && o.PaymentStatus == string(billing.InvoicePaymentRequiresAction)
}

// HasRecurring reports a line that renews.
// recurringLines counts the invoice's subscriptions.
func (o *Invoice) HasRecurring() bool {
	for _, l := range o.Lines {
		if l.BillingIntervalHours != nil || l.BillingIntervalMonths != nil {
			return true
		}
	}
	return false
}

// HasOneOff reports a line bought once: the invoice's payment is a one-off
// purchase with its own receipt, beside any subscription it starts.
func (o *Invoice) HasOneOff() bool {
	for _, l := range o.Lines {
		if l.BillingIntervalHours == nil && l.BillingIntervalMonths == nil {
			return true
		}
	}
	return false
}

// Items names what the invoice bought, as its receipt does.
func (o *Invoice) Items() string { return payments.InvoiceItems(o.Lines) }

// View is the invoice as its customer reads it. nextAction and options are
// the caller's: they come from the live attempt and the routing.
func (o *Invoice) View(nextAction *billing.NextAction, options []billing.InvoicePaymentOption) billing.Invoice {
	out := billing.Invoice{
		ID: billing.InvoiceID(o.ID), CustomerID: billing.CustomerID(o.CustomerID), Origin: billing.InvoiceOrigin(o.Origin),
		Status: billing.InvoiceStatus(o.Status), Number: o.Number, Currency: catalog.Currency(o.Currency), Total: o.Total,
		CollectionOwner: billing.InvoiceCollectionOwner(o.CollectionOwner),
		ProviderBills:   append(make([]billing.InvoiceProviderBill, 0, len(o.ProviderBills)), o.ProviderBills...),
		Lines:           make([]billing.InvoiceLine, 0, len(o.Lines)), PaymentOptions: []billing.InvoicePaymentOption{},
		Payment:   billing.InvoicePayment{Status: billing.InvoicePaymentStatus(o.PaymentStatus)},
		ExpiresAt: o.ExpiresAt, CompletedAt: o.CompletedAt, CanceledAt: o.CanceledAt, ExpiredAt: o.ExpiredAt, VoidedAt: o.VoidedAt,
		UncollectibleAt: o.UncollectibleAt, CreatedAt: o.CreatedAt,
	}
	if o.PspID != nil {
		psp := billing.PSPID(*o.PspID)
		out.CollectionPSPID = &psp
	}
	if o.SubscriptionID != nil && o.PeriodStartsAt != nil && o.PeriodEndsAt != nil && o.DueAt != nil {
		out.Renewal = &billing.InvoiceRenewal{SubscriptionID: billing.SubscriptionID(*o.SubscriptionID), PeriodStartsAt: *o.PeriodStartsAt, PeriodEndsAt: *o.PeriodEndsAt, DueAt: *o.DueAt}
		if o.CheckoutInvoiceID != nil {
			checkout := billing.InvoiceID(*o.CheckoutInvoiceID)
			out.Renewal.CheckoutInvoiceID = &checkout
		}
	}
	if o.Origin == string(billing.InvoiceOriginUsage) {
		out.Usage = usageView(o.BillingInvoice)
	}
	if o.CollectionOwner == string(billing.InvoiceCollectionEngine) && o.AwaitsCustomer() {
		out.Payment.NextAction = nextAction
	}
	if o.PayUrlExpiresAt != nil && o.SavedPaymentMethods != nil && o.Embedded != nil {
		expires := o.PayUrlExpiresAt.UTC()
		out.PayURLExpiresAt, out.Embedded, out.SavedPaymentMethods = &expires, *o.Embedded, *o.SavedPaymentMethods
		out.SuccessURL, out.CancelURL = o.SuccessUrl, o.CancelUrl
	}
	if o.CollectionOwner == string(billing.InvoiceCollectionEngine) && o.Status == string(billing.InvoiceOpen) && options != nil {
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
		line := billing.InvoiceLine{ID: billing.InvoiceLineID(l.ID), Description: l.Description, Quantity: intPtr(l.Quantity), UnitAmount: l.UnitAmount, Amount: l.Amount,
			BillingIntervalHours: intPtr(l.BillingIntervalHours), BillingIntervalMonths: intPtr(l.BillingIntervalMonths), AccessDurationHours: intPtr(l.AccessDurationHours), Source: l.Source}
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

func usageView(o gen.BillingInvoice) *billing.InvoiceUsage {
	out := &billing.InvoiceUsage{AmountPaid: usage.Paid(o), AmountDue: usage.Due(o), NextCollectionAttemptAt: o.NextCollectionAttemptAt,
		PONumber: o.PoNumber, Memo: o.Memo, Tax: map[string]any{}, BillingContacts: []billing.InvoiceContact{}}
	if o.PeriodStartsAt != nil && o.PeriodEndsAt != nil && o.DueAt != nil {
		out.PeriodStartsAt, out.PeriodEndsAt, out.DueAt = *o.PeriodStartsAt, *o.PeriodEndsAt, *o.DueAt
	}
	if o.Status == string(billing.InvoiceVoid) {
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
		return nil, fmt.Errorf("invoice line value %d is out of range", *v)
	}
	out := int32(*v)
	return &out, nil
}

// creditOf reads a line's frozen credit benefit.
func creditOf(l gen.BillingInvoiceLine) (*models.CreditGrantSnapshot, error) {
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
