package money

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

type MerchantInvoiceFilter struct {
	CustomerID *uuid.UUID
	Currency   *string
	Status     *string
	PeriodFrom *time.Time
	PeriodTo   *time.Time
}

func (s *MoneyService) ListMerchantInvoices(ctx context.Context, filter MerchantInvoiceFilter, limit, offset int) ([]models.Invoice, int64, error) {
	if limit < 1 || limit > 100 || offset < 0 || offset > 2147483647 {
		return nil, 0, fmt.Errorf("invalid invoice pagination")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, 0, err
	}
	limit32, _ := safecast.Convert[int32](limit)
	offset32, _ := safecast.Convert[int32](offset)
	var invoices []models.Invoice
	var total int64
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		q := s.db.Gen(ctx)
		var err error
		total, err = q.CountMerchantInvoices(ctx, gen.CountMerchantInvoicesParams{MerchantID: mid.UUID(), CustomerID: filter.CustomerID, Currency: filter.Currency, Status: filter.Status, PeriodFrom: filter.PeriodFrom, PeriodTo: filter.PeriodTo})
		if err != nil {
			return err
		}
		rows, err := q.ListMerchantInvoices(ctx, gen.ListMerchantInvoicesParams{MerchantID: mid.UUID(), CustomerID: filter.CustomerID, Currency: filter.Currency, Status: filter.Status, PeriodFrom: filter.PeriodFrom, PeriodTo: filter.PeriodTo, PageLimit: limit32, PageOffset: offset32})
		if err != nil {
			return err
		}
		invoices = make([]models.Invoice, 0, len(rows))
		for _, row := range rows {
			invoice, e := invoiceFromGen(row)
			if e != nil {
				return e
			}
			invoices = append(invoices, *invoice)
		}
		return nil
	})
	return invoices, total, err
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

type InvoiceAdminAction string

const (
	InvoiceAdminVoid            InvoiceAdminAction = "void"
	InvoiceAdminUncollectible   InvoiceAdminAction = "mark_uncollectible"
	InvoiceAdminRecordPayment   InvoiceAdminAction = "record_payment"
	InvoiceAdminRetryCollection InvoiceAdminAction = "retry_collection"
)

var (
	ErrInvoiceActionNotAllowed     = errors.New("invoice action is not allowed in its current state")
	ErrInvoicePaymentReferenceUsed = errors.New("manual payment reference already applied")
	ErrInvoicePaymentExceedsDue    = errors.New("payment amount exceeds invoice amount_due")
	ErrInvoicePaymentInvalid       = errors.New("positive amount and payment reference are required")
)

// InvoiceAdminActions describes support operations without granting permission.
// Unknown/in-flight collections require reconciliation before any support mutation.
func InvoiceAdminActions(invoice *models.Invoice) []InvoiceAdminAction {
	actions := make([]InvoiceAdminAction, 0, 4)
	if invoice == nil {
		return actions
	}
	code := derefStr(invoice.LastCollectionFailureCode)
	if code == collectionAttemptInProgress || code == collectionOutcomeUnknown {
		return actions
	}
	if invoiceCollectionRetryable(invoice) {
		actions = append(actions, InvoiceAdminRetryCollection)
	}
	switch invoice.Status {
	case "draft":
		actions = append(actions, InvoiceAdminVoid)
	case "open", "past_due":
		actions = append(actions, InvoiceAdminVoid, InvoiceAdminUncollectible, InvoiceAdminRecordPayment)
	}
	return actions
}

type InvoiceAdminMutation struct {
	Action    InvoiceAdminAction
	Amount    int64
	Reference string
}

// ApplyInvoiceAdminMutation locks state before delegating to existing ledger-aware
// operations. Collection itself uses RetryInvoiceCollectionIdempotent separately,
// so no provider operation runs inside this local transaction.
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
		code := derefStr(current.LastCollectionFailureCode)
		if code == collectionAttemptInProgress || code == collectionOutcomeUnknown {
			return ErrInvoiceActionNotAllowed
		}
		if (in.Action == InvoiceAdminVoid && current.Status == "voided") || (in.Action == InvoiceAdminUncollectible && current.Status == "uncollectible") {
			out = current
			return nil
		}
		if !slices.Contains(InvoiceAdminActions(current), in.Action) || in.Action == InvoiceAdminRetryCollection {
			return ErrInvoiceActionNotAllowed
		}
		local := NewMoneyService(s.db.NewWithPgxTx(tx), s.Clock())
		switch in.Action {
		case InvoiceAdminVoid:
			out, e = local.VoidInvoice(ctx, payer, id)
		case InvoiceAdminUncollectible:
			out, e = local.MarkInvoiceUncollectible(ctx, payer, id)
		case InvoiceAdminRecordPayment:
			if in.Amount <= 0 || strings.TrimSpace(in.Reference) == "" {
				return ErrInvoicePaymentInvalid
			}
			out, e = local.RecordOutOfBandInvoicePayment(ctx, payer, id, in.Amount, in.Reference)
		default:
			return ErrInvoiceActionNotAllowed
		}
		return e
	})
	return out, err
}
