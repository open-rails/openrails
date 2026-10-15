package webhooks

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Query dates are UTC and share the history reader's contract. Receipt time is
// not the refund date, and the webhook date's timezone is not confirmed.
func (s *NMIWebhookService) recordNMIRefund(ctx context.Context, original *models.Payment, transactionID string, amount int64) (*models.Payment, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	if original.PspID == nil || *original.PspID != psp {
		return nil, fmt.Errorf("NMI refund account does not own the original payment")
	}
	if s.NMIResolver == nil {
		return nil, fmt.Errorf("NMI refund reader is unavailable")
	}
	client, found, err := s.NMIResolver.ResolveNMIClient(ctx, mid.UUID(), &psp)
	if err != nil {
		return nil, fmt.Errorf("resolve NMI refund account: %w", err)
	}
	if !found || client == nil {
		return nil, fmt.Errorf("NMI refund account is unavailable")
	}
	report, err := client.TransactionReport(ctx, nmi.QueryFilter{TransactionID: transactionID})
	if err != nil {
		return nil, fmt.Errorf("read NMI refund: %w", err)
	}
	at, err := nmiRefundTime(report, original, transactionID, amount, s.now())
	if err != nil {
		return nil, err
	}
	return s.PaymentService.RecordObservedRefund(ctx, original.ID, transactionID, amount, payments.ReversalRefund, at, nil)
}

func nmiRefundTime(report nmi.TransactionReport, original *models.Payment, transactionID string, amount int64, now time.Time) (time.Time, error) {
	var occurredAt time.Time
	for _, transaction := range report.Transactions {
		if strings.TrimSpace(transaction.TransactionID) != transactionID {
			continue
		}
		if transactionID == original.TransactionID || strings.TrimSpace(transaction.OriginalTransactionID) != original.TransactionID ||
			moneyutil.NormalizeCurrency(transaction.Currency) != moneyutil.NormalizeCurrency(original.Currency) {
			return time.Time{}, fmt.Errorf("NMI refund report conflicts with the original payment")
		}
		for _, action := range transaction.Actions {
			if !action.Is("refund") || !action.Succeeded() {
				continue
			}
			minor, err := moneyutil.DecimalToRailMinor(original.Currency, action.Amount)
			if err != nil {
				return time.Time{}, fmt.Errorf("read NMI refund report amount: %w", err)
			}
			if minor < 0 {
				minor = -minor
			}
			native, err := moneyutil.RailMinorToNative(original.Currency, minor)
			if err != nil || native != amount || amount <= 0 {
				return time.Time{}, fmt.Errorf("NMI refund report amount conflicts with the notification")
			}
			at, valid := action.At()
			if !valid || at.IsZero() || at.Before(original.PurchasedAt.Truncate(time.Second)) || at.After(now) {
				return time.Time{}, fmt.Errorf("NMI refund report has no valid occurrence date")
			}
			if !occurredAt.IsZero() {
				return time.Time{}, fmt.Errorf("NMI refund report has multiple successful refund actions")
			}
			occurredAt = at
		}
	}
	if occurredAt.IsZero() {
		return time.Time{}, fmt.Errorf("NMI refund is not yet confirmed in the transaction report")
	}
	return occurredAt, nil
}
