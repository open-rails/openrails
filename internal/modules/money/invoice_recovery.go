package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
	"github.com/open-rails/openrails/pkg/merchant"
)

// RecoveryPaymentMethodRows lists the payer's saved methods a customer
// recovery charge may run through: vaulted, not parked, on a rail OpenRails
// drives (RecoveryRailSupported).
func (s *MoneyService) RecoveryPaymentMethodRows(ctx context.Context, payer identity.CustomerID) ([]gen.OpenrailsPaymentMethod, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Gen(ctx).ListPaymentMethodsByCustomer(ctx, gen.ListPaymentMethodsByCustomerParams{MerchantID: tid.UUID(), CustomerID: payer.UUID()})
	if err != nil {
		return nil, fmt.Errorf("list payment methods: %w", err)
	}
	out := make([]gen.OpenrailsPaymentMethod, 0, len(rows))
	for _, row := range rows {
		descriptor, ok := rails.Lookup(models.Rail(row.Rail))
		if !ok || !RecoveryRailSupported(descriptor) || strings.TrimSpace(row.ParkReason) != "" || strings.TrimSpace(row.RailCustomerRef) == "" {
			continue
		}
		out = append(out, row)
	}
	return out, nil
}

// RecoveryPaymentMethods is RecoveryPaymentMethodRows as wire ids.
func (s *MoneyService) RecoveryPaymentMethods(ctx context.Context, payer identity.CustomerID) ([]openrails.PaymentMethodID, error) {
	rows, err := s.RecoveryPaymentMethodRows(ctx, payer)
	if err != nil {
		return nil, err
	}
	out := make([]openrails.PaymentMethodID, 0, len(rows))
	for _, row := range rows {
		out = append(out, openrails.PaymentMethodID(row.ID))
	}
	return out, nil
}

// InvoiceRecovery projects the customer's pay-now state onto each invoice:
// eligibility and its blocker, the engine's own next attempt, attempt history
// facts and the methods pay-now accepts.
func (s *MoneyService) InvoiceRecovery(ctx context.Context, payer identity.CustomerID, invoices []*models.Invoice) (map[uuid.UUID]*openrails.PaymentRecovery, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	out := make(map[uuid.UUID]*openrails.PaymentRecovery, len(invoices))
	if len(invoices) == 0 {
		return out, nil
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	methods, err := s.RecoveryPaymentMethods(ctx, payer)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(invoices))
	for _, invoice := range invoices {
		ids = append(ids, invoice.ID)
	}
	q := s.db.Gen(ctx)
	counts, err := q.CountInvoicePaymentAttemptsByPayerForInvoices(ctx, gen.CountInvoicePaymentAttemptsByPayerForInvoicesParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceIds: ids, RowLimit: int32(len(ids))})
	if err != nil {
		return nil, fmt.Errorf("count invoice attempts: %w", err)
	}
	failures, err := q.ListLatestFailedInvoicePaymentAttemptsByPayer(ctx, gen.ListLatestFailedInvoicePaymentAttemptsByPayerParams{MerchantID: tid.UUID(), CustomerID: payer.UUID(), InvoiceIds: ids, RowLimit: int32(len(ids))})
	if err != nil {
		return nil, fmt.Errorf("list failed invoice attempts: %w", err)
	}
	attemptCount := make(map[uuid.UUID]int, len(counts))
	for _, row := range counts {
		attemptCount[row.InvoiceID] = int(row.AttemptCount)
	}
	lastFailure := make(map[uuid.UUID]gen.OpenrailsInvoicePayment, len(failures))
	for _, row := range failures {
		lastFailure[row.InvoiceID] = row
	}
	for _, invoice := range invoices {
		recovery := &openrails.PaymentRecovery{
			AttemptCount:               attemptCount[invoice.ID],
			NextAttemptAt:              invoice.NextCollectionAttemptAt,
			CompatiblePaymentMethodIDs: methods,
		}
		if failed, ok := lastFailure[invoice.ID]; ok {
			recovery.FailureCategory = derefStr(failed.FailureReason)
			recovery.LastFailureCode = derefStr(failed.FailureCode)
			at := failed.AttemptedAt
			recovery.LastFailedAt = &at
		}
		if invoice.CollectionIntentID != nil {
			live, err := q.GetRailIntent(ctx, *invoice.CollectionIntentID)
			if err != nil {
				return nil, fmt.Errorf("load live collection operation: %w", err)
			}
			recovery.Operation = &openrails.PaymentOperation{ID: live.ID, Status: live.Status}
		}
		recovery.Retryable, recovery.BlockedReason = invoiceRecoveryEligibility(invoice, recovery)
		out[invoice.ID] = recovery
	}
	return out, nil
}

func invoiceRecoveryEligibility(invoice *models.Invoice, recovery *openrails.PaymentRecovery) (bool, string) {
	switch {
	case invoice.Status == "uncollectible":
		return false, openrails.RecoveryBlockedUncollectible
	case recovery.Operation != nil && recovery.Operation.Status == intents.StatusUnknownNeedsVerify:
		return false, openrails.RecoveryBlockedOutcomeUnknown
	case recovery.Operation != nil:
		return false, openrails.RecoveryBlockedInProgress
	case !invoicePayableNow(invoice):
		return false, openrails.RecoveryBlockedNotDue
	case len(recovery.CompatiblePaymentMethodIDs) == 0:
		return false, openrails.RecoveryBlockedNoPaymentMethod
	}
	return true, ""
}
