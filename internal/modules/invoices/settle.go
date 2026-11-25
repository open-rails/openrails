package invoices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/invoices/observed"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/money/books"
	"github.com/open-rails/openrails/internal/modules/payments"
	paycharge "github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/calendarutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// Charge is money a provider took for an invoice, as its qualified receipt
// reports it, or (Manual) money the merchant received outside OpenRails.
type Charge struct {
	Manual          bool
	PaymentID       uuid.UUID
	AttemptID       uuid.UUID
	PSPID           uuid.UUID
	Rail            string
	TransactionID   string
	Amount          int64
	Currency        string
	PaymentMethodID uuid.UUID
	// Card is the card reported for a new card no method keeps (Stripe's,
	// a Basis Theory collection's, or the one NMI's sale names).
	Card        models.Card
	PurchasedAt time.Time
	TokenType   string
	Metadata    map[string]any
	// Cites is the card's lineage the charge used; nil when it was its
	// agreement's storing transaction, referenced by StoringRef.
	Cites      *paycharge.Mandate
	StoringRef string
	// Reuse: the charge stored the card for the customer's one-click buys,
	// its card_on_file agreement.
	Reuse bool
	// Enrolled is the subscription the provider opened for the invoice's one
	// recurring line with this charge: the provider collects it.
	Enrolled *Enrolled
}

// Enrolled is a provider's subscription as its first payment opened it.
type Enrolled struct {
	RailSubscriptionID     string
	PeriodStart, PeriodEnd time.Time
	// PriceID is the provider price the provider bills, when it is not the
	// line's own (a CCBill FlexForm standing in for a price of ours).
	PriceID *uuid.UUID
}

// lineage is the agreement references the charge leaves on the card.
func (c *Charge) lineage() *paycharge.Mandate {
	if c.Cites != nil {
		return c.Cites
	}
	return &paycharge.Mandate{InitialTransactionID: c.StoringRef}
}

// Outcome is what a charge did to its invoice.
type Outcome string

const (
	// OutcomeComplete: the invoice is complete (now, or by an earlier delivery).
	OutcomeComplete Outcome = "complete"
	// OutcomeRefunded: the invoice had closed and what it bought is owned
	// meanwhile; the payment is recorded and its refund queued.
	OutcomeRefunded Outcome = "refunded"
)

// Paid settles an invoice a charge paid, in d's transaction (the caller's
// provider-intent completion): the payment, the number, the claims handed
// to what the lines produce, fulfilment and the invoice.completed event. A charge
// on a canceled or expired invoice, or one past its expiry, revives it while its
// claims are free; otherwise the payment is refunded with a finding.
func (s *Service) Paid(ctx context.Context, d *db.DB, invoiceID uuid.UUID, charge Charge) (Outcome, error) {
	if d == nil || d.Pool() != nil {
		return "", errors.New("an invoice is settled in its charge's transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return "", err
	}
	invoice, err := s.load(ctx, d.Gen(ctx), mid.UUID(), invoiceID, true)
	if err != nil {
		return "", err
	}
	if charge.Amount != invoice.Total || !strings.EqualFold(charge.Currency, invoice.Currency) || charge.PaymentID == uuid.Nil {
		return "", fmt.Errorf("invoice %s charge contradicts its total", invoiceID)
	}
	return s.settle(ctx, d, invoice, &charge)
}

// PaidAgain records, in d's transaction, a charge on an invoice another payment
// completed: the payment is recorded against the invoice, grants nothing, and
// its refund is queued with a finding. It reports whether it recorded the
// charge; a replay records nothing.
func (s *Service) PaidAgain(ctx context.Context, d *db.DB, invoiceID uuid.UUID, charge Charge) (bool, error) {
	if d == nil || d.Pool() != nil {
		return false, errors.New("an invoice is settled in its charge's transaction")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	q := d.Gen(ctx)
	invoice, err := s.load(ctx, q, mid.UUID(), invoiceID, true)
	if err != nil {
		return false, err
	}
	if invoice.Status != string(billing.InvoiceComplete) || invoice.PaymentID == nil || *invoice.PaymentID == charge.PaymentID {
		return false, fmt.Errorf("invoice %s is not paid by another payment", invoiceID)
	}
	if charge.Amount != invoice.Total || !strings.EqualFold(charge.Currency, invoice.Currency) || charge.PaymentID == uuid.Nil {
		return false, fmt.Errorf("invoice %s charge contradicts its total", invoiceID)
	}
	if _, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: charge.PaymentID}); err == nil {
		return false, nil
	} else if !db.IsNotFound(err) {
		return false, err
	}
	payment, err := s.recordPayment(ctx, d, invoice, &charge)
	if err != nil {
		return false, err
	}
	if err := s.attemptDone(ctx, q, invoice, "succeeded", &charge, s.now()); err != nil {
		return false, err
	}
	return true, intents.RefundLateInvoicePayment(ctx, d, payment, s.Clock)
}

// settle is Paid for a locked invoice; charge is nil for a free invoice.
func (s *Service) settle(ctx context.Context, d *db.DB, invoice *Invoice, charge *Charge) (Outcome, error) {
	q := d.Gen(ctx)
	now := s.now()
	if invoice.Status == string(billing.InvoiceComplete) {
		if charge != nil && uuidOf(invoice.PaymentID) != charge.PaymentID {
			return "", errLateApprovalDup
		}
		if charge != nil && charge.PaymentMethodID != uuid.Nil && charge.TokenType == paycharge.TokenTypePANViaProxy {
			if err := s.restoreInvoiceCard(ctx, d, invoice, charge, now); err != nil {
				return "", err
			}
		}
		return OutcomeComplete, nil
	}
	if Expired(invoice.BillingInvoice, now) {
		// Its expiry closed it before the money landed; the money moved, so
		// nothing is left to abandon.
		if _, err := s.closeLocked(ctx, d, invoice, billing.InvoiceExpired, now); err != nil {
			return "", err
		}
	}
	var payment *models.Payment
	if charge != nil {
		if invoice.PaymentID != nil {
			if *invoice.PaymentID != charge.PaymentID {
				return "", errLateApprovalDup
			}
			return OutcomeRefunded, nil
		}
		var err error
		if payment, err = s.recordPayment(ctx, d, invoice, charge); err != nil {
			return "", err
		}
	}
	if invoice.Status == string(billing.InvoiceCanceled) || invoice.Status == string(billing.InvoiceExpired) {
		free, err := s.reclaim(ctx, q, invoice, now)
		if err != nil {
			return "", err
		}
		if !free {
			if payment == nil {
				return "", fmt.Errorf("free invoice %s cannot be paid late", invoice.ID)
			}
			if _, err := q.SetInvoiceLatePayment(ctx, gen.SetInvoiceLatePaymentParams{MerchantID: invoice.MerchantID, ID: invoice.ID, PaymentID: payment.ID, Now: now}); err != nil {
				return "", err
			}
			if err := s.attemptDone(ctx, q, invoice, "succeeded", charge, now); err != nil {
				return "", err
			}
			return OutcomeRefunded, intents.RefundLateInvoicePayment(ctx, d, payment, s.Clock)
		}
	}
	number, err := q.NextInvoiceNumber(ctx)
	if err != nil {
		return "", err
	}
	var paymentID *uuid.UUID
	if payment != nil {
		paymentID = &payment.ID
	}
	if n, err := q.SetInvoiceComplete(ctx, gen.SetInvoiceCompleteParams{MerchantID: invoice.MerchantID, ID: invoice.ID, Number: number, PaymentID: paymentID, Now: now}); err != nil || n != 1 {
		if err == nil {
			err = fmt.Errorf("invoice %s did not complete", invoice.ID)
		}
		return "", err
	}
	invoice.Status, invoice.PaymentStatus = string(billing.InvoiceComplete), string(billing.InvoicePaymentSucceeded)
	if err := subscriptions.WithdrawInvoicePaymentNotices(ctx, d, invoice.CustomerID, invoice.ID); err != nil {
		return "", err
	}
	services := map[uuid.UUID]books.Service{}
	for _, line := range invoice.Lines {
		if err := s.fulfil(ctx, d, invoice, line, charge, services, now); err != nil {
			return "", fmt.Errorf("fulfil invoice line %s: %w", line.ID, err)
		}
	}
	if err := books.ApplyInvoice(ctx, d, invoice.ID, services); err != nil {
		return "", err
	}
	if charge != nil && !charge.Manual && charge.Reuse {
		if err := mandates.RecordStored(ctx, q, mandates.Stored{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, PaymentMethodID: charge.PaymentMethodID, PSPID: charge.PSPID,
			Rail: charge.Rail, Currency: invoice.Currency, Lineage: *charge.lineage(), AcceptedAt: now}); err != nil {
			return "", err
		}
	}
	if _, err := q.ReleaseInvoiceClaims(ctx, gen.ReleaseInvoiceClaimsParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, InvoiceID: invoice.ID}); err != nil {
		return "", err
	}
	if err := s.attemptDone(ctx, q, invoice, "succeeded", charge, now); err != nil {
		return "", err
	}
	if charge != nil && invoice.HasOneOff() {
		if err := subscriptions.QueuePurchaseReceipt(ctx, d, subscriptions.PurchaseReceipt{PaymentID: charge.PaymentID, CustomerID: invoice.CustomerID, Items: invoice.Items(),
			Amount: charge.Amount, Currency: invoice.Currency, Rail: charge.Rail, InvoiceID: invoice.ID, InvoiceNumber: number, PaidAt: now}); err != nil {
			return "", err
		}
	}
	extra := map[string]any{"number": number}
	if paymentID != nil {
		extra["payment_id"] = paymentID.String()
	}
	return OutcomeComplete, s.event(ctx, q, &invoice.BillingInvoice, "invoice.completed", now, extra)
}

// reclaim takes a closed invoice's claims again: it reports false when what a
// unique line buys is held meanwhile.
func (s *Service) reclaim(ctx context.Context, q *gen.Queries, invoice *Invoice, now time.Time) (bool, error) {
	for _, l := range invoice.Lines {
		if l.ClaimKey == nil {
			continue
		}
		n, err := q.ClaimOwnership(ctx, gen.ClaimOwnershipParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, ClaimKey: *l.ClaimKey, InvoiceID: invoice.ID, Now: now})
		if err != nil {
			return false, err
		}
		if n == 0 {
			held, err := q.GetOwnershipClaim(ctx, gen.GetOwnershipClaimParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, ClaimKey: *l.ClaimKey})
			if err != nil {
				return false, err
			}
			if held.InvoiceID != invoice.ID {
				return false, nil
			}
		}
	}
	refused, err := s.claimRefusal(ctx, q, invoice, now)
	return refused == nil, err
}

// StillBuyable refuses paying an open invoice whose unique lines the customer
// came to hold, or started buying elsewhere, since it was created, or whose
// products require entitlements the customer no longer holds. q is the
// caller's transaction, holding the customer lock.
func (s *Service) StillBuyable(ctx context.Context, q *gen.Queries, invoice *Invoice, at time.Time) error {
	refused, err := s.claimRefusal(ctx, q, invoice, at)
	if err != nil {
		return err
	}
	if refused != nil {
		return apperr.Conflictf("%s", refused.Message)
	}
	return entitled(ctx, q, invoice.MerchantID, invoice.CustomerID, invoice.Origin, invoice.Lines, at)
}

// claimRefusal is why another purchase holds one of invoice's unique lines,
// nil when none does.
func (s *Service) claimRefusal(ctx context.Context, q *gen.Queries, invoice *Invoice, at time.Time) (*billing.InvoiceLineRefusal, error) {
	held := &Quote{Lines: make([]QuotedLine, 0, len(invoice.Lines))}
	for _, l := range invoice.Lines {
		if l.ClaimKey == nil {
			continue
		}
		product, err := q.GetProductByID(ctx, gen.GetProductByIDParams{MerchantID: invoice.MerchantID, ID: models.DerefUUID(l.ProductID)})
		if err != nil {
			return nil, err
		}
		p, err := models.ProductFromGen(product)
		if err != nil {
			return nil, err
		}
		held.Lines = append(held.Lines, QuotedLine{Product: p, ClaimKey: *l.ClaimKey})
	}
	if err := s.refuseOwned(ctx, q, invoice.MerchantID, invoice.CustomerID, held, invoice.ID, at); err != nil {
		return nil, err
	}
	for _, l := range held.Lines {
		if l.Refusal != nil {
			return l.Refusal, nil
		}
	}
	return nil, nil
}

// recordPayment writes the charge as the invoice's payment.
func (s *Service) recordPayment(ctx context.Context, d *db.DB, invoice *Invoice, charge *Charge) (*models.Payment, error) {
	payment := &models.Payment{
		ID: charge.PaymentID, CustomerID: invoice.CustomerID, InvoiceID: &invoice.ID, TransactionID: charge.TransactionID,
		Amount: charge.Amount, ListAmount: invoice.Total, Currency: invoice.Currency, Status: payments.PaymentStatusSucceededValue,
		PurchasedAt: charge.PurchasedAt, CreatedAt: s.now(), Metadata: charge.Metadata,
		Channel: models.ChannelManual, MoneyMovement: models.MoneyMovementNone,
	}
	if !charge.Manual {
		psp, kind := charge.PSPID, payments.AttemptInitial
		payment.Channel, payment.Rail, payment.PspID, payment.AttemptKind, payment.MoneyMovement = models.ChannelRail, models.Rail(charge.Rail), &psp, &kind, models.MoneyMovementRail
	}
	if charge.TokenType != "" {
		payment.TokenType = &charge.TokenType
	}
	// The card it charged: its saved method's, else the one reported for a
	// new card no method keeps.
	if charge.PaymentMethodID != uuid.Nil {
		if err := payments.SetMethodCard(ctx, d.Gen(ctx), invoice.MerchantID, charge.PaymentMethodID, payment); err != nil {
			return nil, err
		}
	} else {
		payments.SetCard(payment, charge.Card)
	}
	if err := payments.NewPaymentService(d, s.Clock).Create(ctx, payment); err != nil {
		return nil, err
	}
	return payment, nil
}

// linePaid is what charge took for line, for its receipt: the charge when the
// invoice is the line alone, else the line's amount.
func linePaid(invoice *Invoice, line gen.BillingInvoiceLine, charge *Charge) *subscriptions.Paid {
	if len(invoice.Lines) == 1 && charge.Amount > 0 && charge.Currency != "" {
		return &subscriptions.Paid{Amount: charge.Amount, Currency: charge.Currency}
	}
	return &subscriptions.Paid{Amount: line.Amount, Currency: invoice.Currency}
}

// fulfil delivers one paid line: a subscription for a recurring line, a
// credit lot for a credit product, product access otherwise. A subscription's
// first period is the service its line's revenue is recognised over.
func (s *Service) fulfil(ctx context.Context, d *db.DB, invoice *Invoice, line gen.BillingInvoiceLine, charge *Charge, services map[uuid.UUID]books.Service, now time.Time) error {
	q := d.Gen(ctx)
	var holderType string
	var holder uuid.UUID
	switch {
	case line.BillingIntervalHours != nil || line.BillingIntervalMonths != nil:
		if charge == nil || s.Memberships == nil {
			return errors.New("a recurring line needs its charge and the membership service")
		}
		end := now
		if line.BillingIntervalMonths != nil {
			var err error
			end, err = calendarutil.NextBoundary(now, now, int(*line.BillingIntervalMonths))
			if err != nil {
				return err
			}
		} else {
			end = now.Add(time.Duration(*line.BillingIntervalHours) * time.Hour)
		}
		terms := subscriptions.InitialMembershipTerms{
			Rail: models.Rail(charge.Rail), CollectionPolicy: models.CollectionPolicyEngine, SubscriptionID: uuidutil.NewV7(), CustomerID: invoice.CustomerID, PSPID: charge.PSPID,
			ProductID: models.DerefUUID(line.ProductID), PriceID: models.DerefUUID(line.PriceID), PaymentMethodID: charge.PaymentMethodID, ProductName: line.Description,
			RecurringAmount: line.Amount, Currency: invoice.Currency, AccessDurationHours: intPtr(line.AccessDurationHours), ProviderAccessDefault: line.ProviderAccessDefault,
			RecurringIntervalHours: intPtr(line.BillingIntervalHours),
			AcceptedAt:             now, PeriodStart: now, PeriodEnd: end, Quantity: models.DerefIntPtr(line.Quantity),
		}
		if line.BillingIntervalMonths != nil {
			terms.RecurringIntervalMonths, terms.CalendarAnchor = intPtr(line.BillingIntervalMonths), &now
			terms.CalendarAccessDefault = line.AccessDurationHours == nil
			if terms.CalendarAccessDefault {
				terms.AccessDurationHours = new(int(end.Sub(now) / time.Hour))
			}
		}
		terms.AwaitingMethod = charge.PaymentMethodID == uuid.Nil && charge.TokenType == paycharge.TokenTypePANViaProxy
		params := &subscriptions.CreateMembershipParams{
			Prepared: &terms, UserID: invoice.CustomerID.String(), PriceID: models.DerefUUID(line.PriceID), Rail: models.Rail(charge.Rail), PurchasedAt: &now,
			ExistingInvoiceID: &invoice.ID, PaymentMetadata: map[string]any{"invoice_id": invoice.ID.String()}, Paid: linePaid(invoice, line, charge),
		}
		if e := charge.Enrolled; e != nil {
			if len(invoice.Lines) != 1 {
				return errors.New("a provider enrolls an invoice of one subscription")
			}
			terms.CollectionPolicy, terms.PeriodStart, terms.PeriodEnd = models.ProviderCollectionPolicy(charge.Rail), e.PeriodStart, e.PeriodEnd
			params.RailSubscriptionID = &e.RailSubscriptionID
			if e.PriceID != nil {
				terms.PriceID, params.PriceID = *e.PriceID, *e.PriceID
			}
		}
		sub, _, err := s.Memberships.CreateMembershipTx(db.WithPSPID(ctx, charge.PSPID), d, params)
		if err != nil {
			return err
		}
		// Its bills name the checkout that bought it.
		if invoice.PurchaseNumber != nil {
			if _, err := q.SetSubscriptionCheckout(ctx, gen.SetSubscriptionCheckoutParams{MerchantID: invoice.MerchantID, ID: sub.ID, InvoiceID: invoice.ID}); err != nil {
				return err
			}
		}
		// The engine's card agreement; a provider holds its own.
		if charge.Enrolled == nil && charge.PaymentMethodID != uuid.Nil {
			if _, err := mandates.Create(ctx, q, mandates.Agreement{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID, PaymentMethodID: charge.PaymentMethodID, PSPID: charge.PSPID,
				Rail: charge.Rail, Kind: paycharge.AgreementRecurring, SubscriptionID: &sub.ID, Lineage: charge.lineage(), AcceptedAt: now}); err != nil {
				return err
			}
		}
		holderType, holder = "subscription", sub.ID
		services[line.ID] = books.Service{Starts: terms.PeriodStart, Ends: terms.PeriodEnd}
		if line.BillingIntervalMonths != nil {
			services[line.ID] = books.Service{Starts: terms.PeriodStart, Ends: terms.PeriodEnd, Months: int(*line.BillingIntervalMonths)}
		}
		if err := q.SetInvoiceLineProduced(ctx, gen.SetInvoiceLineProducedParams{MerchantID: invoice.MerchantID, ID: line.ID, SubscriptionID: &sub.ID}); err != nil {
			return err
		}
		if charge.Enrolled != nil {
			owner := string(terms.CollectionPolicy)
			n, err := q.BindObservedInvoiceOwner(ctx, gen.BindObservedInvoiceOwnerParams{MerchantID: invoice.MerchantID, InvoiceID: invoice.ID,
				CollectionOwner: owner, PspID: charge.PSPID})
			if err != nil || n != 1 {
				return errors.Join(err, observed.ErrConflict)
			}
			invoice.CollectionOwner, invoice.PspID = owner, &charge.PSPID
			if charge.Rail == string(models.RailNMI) || charge.Rail == string(models.RailCCBill) {
				if err := observed.SeedSchedule(ctx, d, invoice.ID, sub.ID, terms.PeriodEnd, int(*line.BillingIntervalHours), terms.RecurringAmount, now); err != nil {
					return err
				}
			}
		}
		if len(invoice.Lines) == 1 && charge.PaymentID != uuid.Nil {
			if err := q.SetPaymentSubscription(ctx, gen.SetPaymentSubscriptionParams{MerchantID: invoice.MerchantID, ID: charge.PaymentID, SubscriptionID: sub.ID,
				PeriodStart: terms.PeriodStart.UTC().Format(time.RFC3339)}); err != nil {
				return err
			}
		}
	case len(line.CreditGrant) > 0 && string(line.CreditGrant) != "null":
		// Credit delivery is shared with recurring lines below. A one-time
		// credit purchase produces no subscription or product-access window.
	default:
		params := entitlements.PushAccessParams{UserID: invoice.CustomerID.String(), CustomerID: invoice.CustomerID, ProductID: models.DerefUUID(line.ProductID),
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
			if err := q.SetInvoiceLineProduced(ctx, gen.SetInvoiceLineProducedParams{MerchantID: invoice.MerchantID, ID: line.ID, ProductAccessID: &access.ID}); err != nil {
				return err
			}
		}
	}
	if len(line.CreditGrant) > 0 && string(line.CreditGrant) != "null" {
		if charge == nil {
			return errors.New("a credit line needs its charge")
		}
		if err := fundCreditLine(ctx, d, line, charge.PaymentID, now, s.Clock); err != nil {
			return err
		}
	}
	if line.ClaimKey == nil || holderType == "" || catalog.Ownership(models.DerefStr(line.Ownership)) != catalog.OwnershipUnique {
		return nil
	}
	_, err := q.HandOverOwnershipClaim(ctx, gen.HandOverOwnershipClaimParams{MerchantID: invoice.MerchantID, CustomerID: invoice.CustomerID,
		ClaimKey: *line.ClaimKey, InvoiceID: invoice.ID, HolderType: holderType, HolderID: holder, Now: now})
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

// attemptDone ends the invoice's live attempt.
func (s *Service) attemptDone(ctx context.Context, q *gen.Queries, invoice *Invoice, status string, charge *Charge, now time.Time) error {
	if charge == nil || charge.AttemptID == uuid.Nil {
		return nil
	}
	params := gen.SetInvoiceAttemptStatusParams{MerchantID: invoice.MerchantID, ID: charge.AttemptID, Status: status, Now: now}
	if status == "succeeded" {
		params.PaymentID, params.TransactionID = &charge.PaymentID, &charge.TransactionID
	}
	_, err := q.SetInvoiceAttemptStatus(ctx, params)
	return err
}

// Declined records a definite decline of the invoice's attempt, in d's
// transaction: the invoice is open again with the reason.
func (s *Service) Declined(ctx context.Context, d *db.DB, invoiceID, attemptID uuid.UUID, failure *billing.PaymentFailure) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := d.Gen(ctx)
	invoice, err := s.load(ctx, q, mid.UUID(), invoiceID, true)
	if err != nil {
		return err
	}
	now := s.now()
	if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: invoice.MerchantID, ID: attemptID, Status: "failed", Now: now}); err != nil {
		return err
	}
	if invoice.AttemptID == nil || *invoice.AttemptID != attemptID || !invoice.Live() {
		return nil
	}
	if failure == nil {
		failure = &billing.PaymentFailure{Reason: "generic_decline", Message: "The payment was declined."}
	}
	raw, err := json.Marshal(failure)
	if err != nil {
		return err
	}
	if _, err := q.SetInvoiceDeclined(ctx, gen.SetInvoiceDeclinedParams{MerchantID: invoice.MerchantID, ID: invoice.ID, AttemptID: attemptID, LastPaymentError: raw, Now: now}); err != nil {
		return err
	}
	invoice.Status, invoice.PaymentStatus = string(billing.InvoiceOpen), string(billing.InvoicePaymentRequiresPaymentMethod)
	return s.event(ctx, q, &invoice.BillingInvoice, "invoice.payment_failed", now, map[string]any{"reason": failure.Reason})
}

// Pending records that the invoice's live attempt waits for the customer
// (requires_action) or the provider (processing).
func (s *Service) Pending(ctx context.Context, invoiceID, attemptID uuid.UUID, status billing.InvoicePaymentStatus) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	q := s.DB.Gen(ctx)
	now := s.now()
	n, err := q.SetInvoicePending(ctx, gen.SetInvoicePendingParams{MerchantID: mid.UUID(), ID: invoiceID, AttemptID: attemptID, PaymentStatus: string(status), Now: now})
	if err != nil || n == 0 {
		return err
	}
	if _, err := q.SetInvoiceAttemptStatus(ctx, gen.SetInvoiceAttemptStatusParams{MerchantID: mid.UUID(), ID: attemptID, Status: string(status), Now: now}); err != nil {
		return err
	}
	if status != billing.InvoicePaymentRequiresAction {
		return nil
	}
	invoice, err := s.load(ctx, q, mid.UUID(), invoiceID, false)
	if err != nil {
		return err
	}
	return s.event(ctx, q, &invoice.BillingInvoice, "invoice.requires_action", now, nil)
}

// Remittance is money the merchant received for an invoice outside OpenRails.
// TransactionID is its identity: recording it again with the same terms
// answers the first payment.
type Remittance struct {
	InvoiceID     uuid.UUID
	Amount        int64
	TransactionID string
	// PaidAt defaults to now; a replay that omits it does not compare it.
	PaidAt *time.Time
}

// RecordRemittance pays an open invoice with money received outside OpenRails:
// a manual payment, then what Paid does. An invoice with a recurring line is
// refused, since its renewals need the customer's card. created is false
// when the same remittance was already recorded.
func (s *Service) RecordRemittance(ctx context.Context, in Remittance) (payment *models.Payment, created bool, err error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, false, err
	}
	in.TransactionID = strings.TrimSpace(in.TransactionID)
	if in.InvoiceID == uuid.Nil || in.Amount <= 0 || in.TransactionID == "" {
		return nil, false, errors.New("invoice, positive amount and transaction id are required")
	}
	err = s.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.DB.NewWithPgxTx(tx)
		q := d.Gen(ctx)
		head, err := q.GetInvoice(ctx, gen.GetInvoiceParams{MerchantID: mid.UUID(), ID: in.InvoiceID})
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: head.CustomerID}); err != nil {
			return err
		}
		invoice, err := s.load(ctx, q, mid.UUID(), in.InvoiceID, true)
		if err != nil {
			return err
		}
		if invoice.CollectionOwner != string(billing.InvoiceCollectionEngine) {
			return ErrNotPayable
		}
		prior, err := q.GetPaymentByPSPTransactionID(ctx, gen.GetPaymentByPSPTransactionIDParams{MerchantID: mid.UUID(), Channel: string(models.ChannelManual), TransactionID: in.TransactionID})
		switch {
		case err == nil:
			if prior.InvoiceID == nil || *prior.InvoiceID != in.InvoiceID || prior.Amount != in.Amount || in.PaidAt != nil && !prior.PurchasedAt.Equal(in.PaidAt.UTC().Truncate(time.Microsecond)) {
				return billing.ErrIdempotencyKeyReused
			}
			payment, err = models.PaymentFromGen(prior)
			return err
		case !db.IsNotFound(err):
			return err
		}
		now := s.now()
		switch {
		case invoice.HasRecurring():
			return ErrRecurringLine
		case invoice.AwaitsCustomer() || invoice.Status == string(billing.InvoiceProcessing):
			return ErrInProgress
		case !Payable(invoice.BillingInvoice, now):
			return ErrNotPayable
		case in.Amount != invoice.Total:
			return ErrAmountNotTotal
		}
		paidAt := now
		if in.PaidAt != nil {
			paidAt = in.PaidAt.UTC()
		}
		charge := Charge{Manual: true, PaymentID: uuidutil.NewV7(), TransactionID: in.TransactionID, Amount: in.Amount, Currency: invoice.Currency, PurchasedAt: paidAt.Truncate(time.Microsecond)}
		if _, err := s.settle(ctx, d, invoice, &charge); err != nil {
			if db.IsUniqueViolation(err) {
				return billing.ErrIdempotencyKeyReused
			}
			return err
		}
		row, err := q.GetPaymentByID(ctx, gen.GetPaymentByIDParams{MerchantID: mid.UUID(), ID: charge.PaymentID})
		if err != nil {
			return err
		}
		created = true
		payment, err = models.PaymentFromGen(row)
		return err
	})
	if err != nil {
		return nil, false, err
	}
	return payment, created, nil
}
