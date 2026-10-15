package webhooks

import (
	"context"
	"errors"
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

var errNMIRefundOriginalNotFound = errors.New("original NMI payment not found")

// recordNMIRefund returns the original payment so the caller can apply its
// existing access policy. A real NMI refund notice may name only the refund ID.
func (s *NMIWebhookService) recordNMIRefund(ctx context.Context, body *NMITransactionEventBody, transactionID string) (*models.Payment, error) {
	originalID := getOriginalTransactionID(body)
	var report nmi.TransactionReport
	var err error
	if originalID == "" {
		report, err = s.readNMIRefundReport(ctx, transactionID)
		if err != nil {
			return nil, err
		}
		originalID, err = nmiRefundOriginalID(report, transactionID)
		if err != nil {
			return nil, err
		}
	}
	if originalID == transactionID {
		return nil, fmt.Errorf("NMI refund does not identify a distinct original payment")
	}
	original, err := s.PaymentService.GetByPSPTransactionID(ctx, models.Rail(s.Rail), originalID)
	if err != nil {
		if db.IsNotFound(err) {
			return nil, fmt.Errorf("%w: %q", errNMIRefundOriginalNotFound, originalID)
		}
		return nil, fmt.Errorf("resolve original NMI payment %q: %w", originalID, err)
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return nil, err
	}
	if original.PspID == nil || *original.PspID != psp {
		return nil, fmt.Errorf("NMI refund account does not own the original payment")
	}
	amount, err := s.refundAmount(ctx, body, original)
	if err != nil {
		return nil, err
	}
	if getOriginalTransactionID(body) != "" {
		report, err = s.readNMIRefundReport(ctx, transactionID)
		if err != nil {
			return nil, err
		}
	}
	at, err := nmiRefundTime(report, original, transactionID, amount, s.now())
	if err != nil {
		return nil, err
	}
	if _, err := s.PaymentService.RecordObservedRefund(ctx, original.ID, transactionID, amount, payments.ReversalRefund, at, nil); err != nil {
		return nil, err
	}
	return original, nil
}

func (s *NMIWebhookService) readNMIRefundReport(ctx context.Context, transactionID string) (nmi.TransactionReport, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nmi.TransactionReport{}, err
	}
	psp, err := db.RequirePSPID(ctx)
	if err != nil {
		return nmi.TransactionReport{}, err
	}
	if s.NMIResolver == nil {
		return nmi.TransactionReport{}, fmt.Errorf("NMI refund reader is unavailable")
	}
	client, found, err := s.NMIResolver.ResolveNMIClient(ctx, mid.UUID(), &psp)
	if err != nil {
		return nmi.TransactionReport{}, fmt.Errorf("resolve NMI refund account: %w", err)
	}
	if !found || client == nil {
		return nmi.TransactionReport{}, fmt.Errorf("NMI refund account is unavailable")
	}
	report, err := client.TransactionReport(ctx, nmi.QueryFilter{TransactionID: transactionID})
	if err != nil {
		return nmi.TransactionReport{}, fmt.Errorf("read NMI refund: %w", err)
	}
	return report, nil
}

func nmiRefundOriginalID(report nmi.TransactionReport, transactionID string) (string, error) {
	var originalID string
	for _, transaction := range report.Transactions {
		if strings.TrimSpace(transaction.TransactionID) != transactionID {
			continue
		}
		if originalID != "" {
			return "", fmt.Errorf("NMI refund report has multiple matching transactions")
		}
		originalID = strings.TrimSpace(transaction.OriginalTransactionID)
		if originalID == "" || originalID == transactionID {
			return "", fmt.Errorf("NMI refund report does not identify a distinct original payment")
		}
	}
	if originalID == "" {
		return "", fmt.Errorf("NMI refund is not yet confirmed in the transaction report")
	}
	return originalID, nil
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
