package service

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/decline"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/money"
)

// InvoiceView is an invoice on the wire, with the merchant actions its state
// allows; Recovery is left to single-invoice reads.
func InvoiceView(inv *models.Invoice) billing.Invoice {
	items := make([]billing.InvoiceLineItem, 0, len(inv.LineItems))
	for _, li := range inv.LineItems {
		items = append(items, billing.InvoiceLineItem{EventType: li.EventType, Amount: li.Amount, Count: li.Count, Dimensions: li.Dimensions})
	}
	contacts := make([]billing.InvoiceContact, 0, len(inv.BillingContacts))
	for _, c := range inv.BillingContacts {
		contacts = append(contacts, billing.InvoiceContact{Name: c.Name, Email: c.Email})
	}
	return billing.Invoice{
		ID:                        billing.InvoiceID(inv.ID),
		CustomerID:                billing.CustomerID(inv.CustomerID),
		Currency:                  inv.Currency,
		InvoiceNumber:             inv.InvoiceNumber,
		PeriodFrom:                inv.PeriodFrom,
		PeriodTo:                  inv.PeriodTo,
		UsageTotal:                inv.UsageTotal,
		DepositsTotal:             inv.DepositsTotal,
		OwedAccrued:               inv.OwedAccrued,
		OwedPaid:                  inv.OwedPaid,
		ClosingBalance:            inv.ClosingBalance,
		SubtotalAmount:            inv.SubtotalAmount,
		TotalAmount:               inv.TotalAmount,
		AmountPaid:                inv.AmountPaid,
		AmountDue:                 inv.AmountDue,
		LineItems:                 items,
		MoneyMovements:            inv.MoneyMovements,
		PONumber:                  inv.PONumber,
		Tax:                       inv.Tax,
		BillingContacts:           contacts,
		Memo:                      inv.Memo,
		Status:                    billing.InvoiceStatus(inv.Status),
		CollectionMethod:          billing.InvoiceCollectionMethod(inv.CollectionMethod),
		IssuedAt:                  inv.IssuedAt,
		DueAt:                     inv.DueAt,
		PaidAt:                    inv.PaidAt,
		VoidedAt:                  inv.VoidedAt,
		UncollectibleAt:           inv.UncollectibleAt,
		FinalizedAt:               inv.FinalizedAt,
		ExternalInvoiceID:         inv.ExternalInvoiceID,
		CollectionFailureCount:    inv.CollectionFailureCount,
		CollectionFailedAt:        inv.CollectionFailedAt,
		NextCollectionAttemptAt:   inv.NextCollectionAttemptAt,
		LastCollectionFailureCode: inv.LastCollectionFailureCode,
		AvailableActions:          money.InvoiceActions(inv),
		CreatedAt:                 inv.CreatedAt,
	}
}

// InvoicePaymentView is a payment applied to an invoice on the wire.
func InvoicePaymentView(a models.InvoicePaymentAttempt) billing.InvoicePayment {
	return billing.InvoicePayment{
		ID:              billing.InvoicePaymentID(a.ID),
		InvoiceID:       billing.InvoiceID(a.InvoiceID),
		Currency:        a.Currency,
		Amount:          a.Amount,
		Status:          billing.InvoicePaymentStatus(a.Status),
		PaymentMethodID: (*billing.PaymentMethodID)(a.PaymentMethodID),
		Rail:            a.Rail,
		TransactionID:   a.RailPaymentID,
		FailureCode:     a.FailureCode,
		FailureReason:   a.FailureReason,
		AttemptedAt:     a.AttemptedAt,
		SettledAt:       a.SettledAt,
	}
}

// ListInvoices is one page of invoices, newest period first.
func (s *Service) ListInvoices(ctx context.Context, p billing.InvoiceListParams) (billing.ListPage[billing.Invoice], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.Invoice]{}, err
	}
	defer release()
	page, err := s.moneyService().ListInvoices(ctx, p)
	if err != nil {
		return billing.ListPage[billing.Invoice]{}, err
	}
	out := billing.ListPage[billing.Invoice]{Items: make([]billing.Invoice, 0, len(page.Items)), Next: page.Next}
	for i := range page.Items {
		out.Items = append(out.Items, InvoiceView(&page.Items[i]))
	}
	return out, nil
}

// GetInvoice reads one invoice, with whether its customer can pay it now. A
// zero payer reads any customer's invoice (the merchant); otherwise another
// customer's invoice is not found.
func (s *Service) GetInvoice(ctx context.Context, payer identity.CustomerID, id uuid.UUID) (*billing.Invoice, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	var inv *models.Invoice
	if payer.IsZero() {
		inv, err = s.moneyService().GetMerchantInvoice(ctx, id)
	} else {
		inv, err = s.moneyService().GetInvoiceByID(ctx, payer, id)
	}
	if err != nil {
		return nil, err
	}
	out := InvoiceView(inv)
	if out.Recovery, err = s.invoiceRecovery(ctx, inv); err != nil {
		return nil, err
	}
	return &out, nil
}

// invoiceRecovery is whether the invoice's customer can pay it now.
func (s *Service) invoiceRecovery(ctx context.Context, invoice *models.Invoice) (*billing.PaymentRecovery, error) {
	out := &billing.PaymentRecovery{}
	switch {
	case invoice.CollectionIntentID != nil:
		row, err := intents.NewStore(s.rt.DB).Get(ctx, *invoice.CollectionIntentID)
		if err != nil {
			return nil, err
		}
		out.Operation = &billing.PaymentOperation{ID: row.ID, Status: row.Status}
		out.BlockedReason = "payment_in_progress"
	case invoice.AmountDue <= 0 || (invoice.Status != "open" && invoice.Status != "past_due" && invoice.Status != "uncollectible"):
		out.BlockedReason = "invoice_not_payable"
	default:
		out.Retryable = true
		// The newest payment is the invoice's standing; a later pending or
		// settled one does not inherit an older decline.
		mid, err := merchant.Require(ctx)
		if err != nil {
			return nil, err
		}
		latest, err := s.rt.DB.Gen(ctx).ListInvoicePaymentsPage(ctx, gen.ListInvoicePaymentsPageParams{MerchantID: mid.UUID(), CustomerID: invoice.CustomerID, InvoiceID: invoice.ID, RowLimit: 1})
		if err != nil {
			return nil, err
		}
		if len(latest) > 0 && latest[0].Status == "failed" && latest[0].Rail != nil && latest[0].FailureCode != nil {
			out.LastFailureReason = decline.ReasonFor(*latest[0].Rail, *latest[0].FailureCode)
		}
	}
	return out, nil
}

// ListInvoicePayments is one page of an invoice's payments, newest first.
func (s *Service) ListInvoicePayments(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, page billing.PageRequest) (billing.ListPage[billing.InvoicePayment], error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return billing.ListPage[billing.InvoicePayment]{}, err
	}
	defer release()
	rows, err := s.moneyService().ListInvoicePayments(ctx, payer, invoiceID, page)
	if err != nil {
		return billing.ListPage[billing.InvoicePayment]{}, err
	}
	out := billing.ListPage[billing.InvoicePayment]{Items: make([]billing.InvoicePayment, 0, len(rows.Items)), Next: rows.Next}
	for _, row := range rows.Items {
		out.Items = append(out.Items, InvoicePaymentView(row))
	}
	return out, nil
}

// ApplyInvoiceAction voids an invoice, marks it uncollectible or records a
// payment received outside collection.
func (s *Service) ApplyInvoiceAction(ctx context.Context, payer identity.CustomerID, id uuid.UUID, in money.InvoiceAdminMutation) (*billing.Invoice, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	inv, err := s.moneyService().ApplyInvoiceAdminMutation(ctx, payer, id, in)
	if err != nil {
		return nil, err
	}
	out := InvoiceView(inv)
	return &out, nil
}

// RetryInvoiceCollection charges an open invoice to one of its customer's
// cards through the invoice_collection operation.
func (s *Service) RetryInvoiceCollection(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, p billing.RetryInvoiceCollectionParams) (*billing.InvoiceCollection, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.MoneyCharger == nil {
		return nil, fmt.Errorf("invoice collection charger not configured")
	}
	var out *billing.InvoiceCollection
	err = s.rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		result, err := s.moneyService().RetryInvoiceCollection(ctx, rt.IntentRunner(), payer, money.InvoiceCollectionRetryRequest{
			InvoiceID: invoiceID, IdempotencyKey: p.IdempotencyKey, PaymentMethodID: p.PaymentMethodID.UUID(),
		})
		if err != nil {
			return err
		}
		out = &billing.InvoiceCollection{Invoice: InvoiceView(result.Invoice), Payment: InvoicePaymentView(result.Attempt), Replayed: result.Replayed}
		return nil
	})
	return out, err
}

// PayInvoice is the customer paying an invoice now. The HTTP adapter
// establishes verified customer action authority before passing the payer.
func (s *Service) PayInvoice(ctx context.Context, payer identity.CustomerID, invoiceID uuid.UUID, p billing.PayInvoiceParams) (*billing.InvoicePayNow, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	var out *billing.InvoicePayNow
	err = rt.DB.RunInMerchantConn(ctx, func(ctx context.Context) error {
		result, err := s.moneyService().PayInvoiceNow(ctx, rt.IntentRunner(), payer, money.InvoiceCollectionRetryRequest{InvoiceID: invoiceID, PaymentMethodID: p.PaymentMethodID.UUID(), IdempotencyKey: p.IdempotencyKey})
		if err != nil {
			return err
		}
		if err := customerPaymentRefusal(result.Operation); err != nil {
			return err
		}
		out = &billing.InvoicePayNow{Invoice: InvoiceView(result.Invoice), Payment: InvoicePaymentView(result.Attempt), Operation: billing.PaymentOperation{ID: result.Operation.ID, Status: result.Operation.Status}, Replayed: result.Replayed}
		return nil
	})
	return out, err
}

// GetInvoiceProfile reads a customer's invoice profile; nil when none
// is set.
func (s *Service) GetInvoiceProfile(ctx context.Context, payer identity.CustomerID) (*billing.InvoiceProfile, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	p, err := s.moneyService().GetInvoiceProfile(ctx, payer)
	if err != nil || p == nil {
		return nil, err
	}
	contacts := make([]billing.InvoiceContact, 0, len(p.BillingContacts))
	for _, c := range p.BillingContacts {
		contacts = append(contacts, billing.InvoiceContact{Name: c.Name, Email: c.Email})
	}
	return &billing.InvoiceProfile{
		NetTermsDays: p.NetTermsDays, CollectionMethod: billing.InvoiceCollectionMethod(p.CollectionMethod), PONumber: p.PONumber,
		Tax: p.Tax, BillingContacts: contacts, Memo: p.Memo,
	}, nil
}

// SetInvoiceProfile replaces a customer's invoice profile, or with
// IfAbsent creates it only when none is set. created reports a new profile.
// Operator surface: a customer never grants itself credit terms.
func (s *Service) SetInvoiceProfile(ctx context.Context, payer identity.CustomerID, p billing.SetInvoiceProfileParams) (profile *billing.InvoiceProfile, created bool, err error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	if payer.IsZero() {
		return nil, false, fmt.Errorf("payer required")
	}
	contacts := make([]models.InvoiceContact, 0, len(p.BillingContacts))
	for _, c := range p.BillingContacts {
		contacts = append(contacts, models.InvoiceContact{Name: c.Name, Email: c.Email})
	}
	in := money.CustomerInvoiceProfile{
		NetTermsDays: p.NetTermsDays, CollectionMethod: string(p.CollectionMethod), PONumber: p.PONumber,
		Tax: p.Tax, BillingContacts: contacts, Memo: p.Memo,
	}
	if p.IfAbsent {
		if created, err = s.moneyService().EnsureCustomerInvoiceProfile(ctx, payer, in); err != nil {
			return nil, false, err
		}
	} else if err = s.moneyService().SetInvoiceProfile(ctx, payer, in); err != nil {
		return nil, false, err
	}
	profile, err = s.GetInvoiceProfile(ctx, payer)
	return profile, created, err
}
