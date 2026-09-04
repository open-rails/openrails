package service

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/identity"
)

type MerchantInvoiceFilter = money.MerchantInvoiceFilter
type InvoiceAdminMutation = money.InvoiceAdminMutation
type InvoiceAdminAction = money.InvoiceAdminAction

const (
	InvoiceAdminVoid            = money.InvoiceAdminVoid
	InvoiceAdminUncollectible   = money.InvoiceAdminUncollectible
	InvoiceAdminRecordPayment   = money.InvoiceAdminRecordPayment
	InvoiceAdminRetryCollection = money.InvoiceAdminRetryCollection
)

type MerchantInvoiceDTO struct {
	InvoiceDTO
	UnitDecimals     int                  `json:"unit_decimals"`
	CustomerID       uuid.UUID            `json:"customer_id"`
	AvailableActions []InvoiceAdminAction `json:"available_actions"`
}

func merchantInvoiceDTO(invoice *models.Invoice) (MerchantInvoiceDTO, error) {
	decimals, ok := moneyutil.CurrencyScale(invoice.Currency)
	if !ok {
		return MerchantInvoiceDTO{}, fmt.Errorf("invoice currency is not registered")
	}
	return MerchantInvoiceDTO{InvoiceDTO: invoiceToDTO(invoice), CustomerID: invoice.CustomerID, UnitDecimals: decimals, AvailableActions: money.InvoiceAdminActions(invoice)}, nil
}

func (s *Service) ListMerchantInvoices(ctx context.Context, filter MerchantInvoiceFilter, limit, offset int) ([]MerchantInvoiceDTO, int64, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	rows, total, err := s.moneyService().ListMerchantInvoices(ctx, filter, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]MerchantInvoiceDTO, 0, len(rows))
	for i := range rows {
		dto, err := merchantInvoiceDTO(&rows[i])
		if err != nil {
			return nil, 0, err
		}
		out = append(out, dto)
	}
	return out, total, nil
}
func (s *Service) GetMerchantInvoice(ctx context.Context, id uuid.UUID) (*MerchantInvoiceDTO, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	inv, err := s.moneyService().GetMerchantInvoice(ctx, id)
	if err != nil {
		return nil, err
	}
	out, err := merchantInvoiceDTO(inv)
	return &out, err
}
func (s *Service) ApplyMerchantInvoiceMutation(ctx context.Context, payer identity.CustomerID, id uuid.UUID, in InvoiceAdminMutation) (*MerchantInvoiceDTO, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	inv, err := s.moneyService().ApplyInvoiceAdminMutation(ctx, payer, id, in)
	if err != nil {
		return nil, err
	}
	out, err := merchantInvoiceDTO(inv)
	return &out, err
}
