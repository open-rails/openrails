package money

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/open-rails/openrails/billing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// ListInvoices is one page of invoices, newest period first.
func (s *MoneyService) ListInvoices(ctx context.Context, p billing.InvoiceListParams) (billing.ListPage[models.Invoice], error) {
	var out billing.ListPage[models.Invoice]
	mid, err := merchant.Require(ctx)
	if err != nil {
		return out, err
	}
	if p.IDs != nil {
		err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
			rows, err := s.db.Gen(ctx).ListInvoicesByIDs(ctx, gen.ListInvoicesByIDsParams{MerchantID: mid.UUID(), Ids: uuidutil.Of(p.IDs)})
			if err != nil {
				return err
			}
			for _, row := range rows {
				inv, err := invoiceFromGen(row)
				if err != nil {
					return err
				}
				out.Items = append(out.Items, *inv)
			}
			return nil
		})
		return out, err
	}
	limit, err := pagination.Limit(p.PageRequest)
	if err != nil {
		return out, err
	}
	afterAt, afterID, err := pagination.After(p.PageRequest.Cursor)
	if err != nil {
		return out, err
	}
	params := gen.ListInvoicesPageParams{MerchantID: mid.UUID(), PeriodStartsAfter: p.PeriodStartsAfter, PeriodStartsBefore: p.PeriodStartsBefore, AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit)}
	if !p.CustomerID.IsZero() {
		id := p.CustomerID.UUID()
		params.CustomerID = &id
	}
	if p.Currency != "" {
		params.Currency = &p.Currency
	}
	if p.Status != "" {
		status := string(p.Status)
		params.Status = &status
	}
	if p.Overdue {
		now := s.now()
		params.OverdueAt = &now
	}
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		rows, err := s.db.Gen(ctx).ListInvoicesPage(ctx, params)
		if err != nil {
			return err
		}
		invoices := make([]models.Invoice, 0, len(rows))
		for _, row := range rows {
			inv, err := invoiceFromGen(row)
			if err != nil {
				return err
			}
			invoices = append(invoices, *inv)
		}
		out = pagination.Cut(invoices, limit, func(inv models.Invoice) any { return pagination.TimeID{At: inv.PeriodStartsAt, ID: inv.ID} })
		return nil
	})
	return out, err
}

func (s *MoneyService) GetMerchantInvoice(ctx context.Context, id uuid.UUID) (*models.Invoice, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var invoice *models.Invoice
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, e := s.db.Gen(ctx).GetMerchantInvoice(ctx, gen.GetMerchantInvoiceParams{MerchantID: mid.UUID(), ID: id})
		if e != nil {
			return e
		}
		invoice, e = invoiceFromGen(row)
		return e
	})
	return invoice, err
}

var (
	ErrInvoiceActionNotAllowed = errors.New("invoice action is not allowed in its current state")
	ErrPaymentExceedsDue       = errors.New("payment amount exceeds invoice amount_due")
)

// InvoiceAdminActions describes support operations without granting permission.
// Unknown/in-flight collections require reconciliation before any support mutation.
func InvoiceActions(invoice *models.Invoice, now time.Time) []billing.InvoiceAction {
	actions := make([]billing.InvoiceAction, 0, 4)
	if invoice == nil {
		return actions
	}
	if invoice.CollectionIntentID != nil {
		return actions
	}
	if invoiceCollectionRetryable(invoice, now) {
		actions = append(actions, billing.InvoiceActionRetryCollection)
	}
	switch invoice.Status {
	case "draft":
		actions = append(actions, billing.InvoiceActionVoid)
	case "open":
		actions = append(actions, billing.InvoiceActionVoid)
		// A statement waiting only on other invoices has nothing due to pay
		// or write off.
		if invoice.AmountDue > 0 {
			actions = append(actions, billing.InvoiceActionUncollectible, billing.InvoiceActionRecordPayment)
		}
	}
	return actions
}

type InvoiceAdminMutation struct {
	Action billing.InvoiceAction
}

// ApplyInvoiceAdminMutation keeps administrative changes local and transaction-locked.
// Collection runs separately, so no provider operation runs inside a local
// transaction.
func (s *MoneyService) ApplyInvoiceAdminMutation(ctx context.Context, payer identity.CustomerID, id uuid.UUID, in InvoiceAdminMutation) (*models.Invoice, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out *models.Invoice
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row, e := gen.New(tx).GetInvoiceForPayerForUpdate(ctx, gen.GetInvoiceForPayerForUpdateParams{MerchantID: mid.UUID(), CustomerID: payer.UUID(), ID: id})
		if e != nil {
			return e
		}
		current, e := invoiceFromGen(row)
		if e != nil {
			return e
		}
		if current.CollectionIntentID != nil {
			return ErrInvoiceActionNotAllowed
		}
		if (in.Action == billing.InvoiceActionVoid && current.Status == "voided") || (in.Action == billing.InvoiceActionUncollectible && current.Status == "uncollectible") {
			out = current
			return nil
		}
		if !slices.Contains(InvoiceActions(current, s.now()), in.Action) || in.Action == billing.InvoiceActionRetryCollection {
			return ErrInvoiceActionNotAllowed
		}
		local := NewMoneyService(s.db.NewWithPgxTx(tx), s.Clock())
		switch in.Action {
		case billing.InvoiceActionVoid:
			out, e = local.VoidInvoice(ctx, payer, id)
		case billing.InvoiceActionUncollectible:
			out, e = local.MarkInvoiceUncollectible(ctx, payer, id)
		default:
			return ErrInvoiceActionNotAllowed
		}
		return e
	})
	return out, err
}
