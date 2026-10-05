package alerting

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// LedgerRepair is a provider event OpenRails could not book, which the
// merchant must repair by hand.
type LedgerRepair struct {
	Provider          string
	Operation         string
	TransactionID     string
	CustomerID        string
	IdempotencyKey    string
	OriginalPaymentID *uuid.UUID
	SubscriptionID    *uuid.UUID
	Err               error
	Metadata          map[string]any
}

// ledgerRepairData is a repair notification's data.
type ledgerRepairData struct {
	Kind              string                 `json:"kind"`
	Provider          string                 `json:"provider,omitempty"`
	Operation         string                 `json:"operation,omitempty"`
	TransactionID     string                 `json:"transaction_id,omitempty"`
	CustomerID        billing.CustomerID     `json:"customer_id,omitzero"`
	OriginalPaymentID billing.PaymentID      `json:"original_payment_id,omitzero"`
	SubscriptionID    billing.SubscriptionID `json:"subscription_id,omitzero"`
	Error             string                 `json:"error,omitempty"`
	Metadata          map[string]any         `json:"metadata,omitempty"`
	FiredAt           time.Time              `json:"fired_at"`
}

// RecordLedgerRepair puts a critical notification in the merchant's inbox.
// A repeated IdempotencyKey writes nothing.
func RecordLedgerRepair(ctx context.Context, database *db.DB, now time.Time, repair LedgerRepair) error {
	if database == nil {
		return fmt.Errorf("ledger repair: database is required")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("ledger repair: %w", err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	data := ledgerRepairData{
		Kind:          "ledger_repair_required",
		Provider:      strings.TrimSpace(repair.Provider),
		Operation:     strings.TrimSpace(repair.Operation),
		TransactionID: strings.TrimSpace(repair.TransactionID),
		FiredAt:       now.UTC(),
	}
	if customer := strings.TrimSpace(repair.CustomerID); customer != "" {
		if data.CustomerID, err = billing.ParseCustomerID(customer); err != nil {
			return fmt.Errorf("ledger repair: customer: %w", err)
		}
	}
	if repair.OriginalPaymentID != nil {
		data.OriginalPaymentID = billing.PaymentID(*repair.OriginalPaymentID)
	}
	if repair.SubscriptionID != nil {
		data.SubscriptionID = billing.SubscriptionID(*repair.SubscriptionID)
	}
	if repair.Err != nil {
		data.Error = repair.Err.Error()
	}
	for key, value := range repair.Metadata {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if data.Metadata == nil {
			data.Metadata = map[string]any{}
		}
		data.Metadata[key] = value
	}
	n := Notification{
		Severity: SeverityCritical,
		Title:    strings.TrimSuffix("Ledger repair needed: "+data.Operation, ": "),
		Body:     ledgerRepairBody(data),
		Data:     data,
	}
	if key := strings.TrimSpace(repair.IdempotencyKey); key != "" {
		n.ID = uuidutil.DeterministicID(uuidutil.DeterministicNamespace, "ledger_repair", merchantID.String(), key)
	}
	if err := newStore(database).createNotification(ctx, n); err != nil {
		return fmt.Errorf("ledger repair: %w", err)
	}
	return nil
}

func ledgerRepairBody(d ledgerRepairData) string {
	body := d.Provider + " " + d.Operation
	if d.TransactionID != "" {
		body += " (transaction " + d.TransactionID + ")"
	}
	body = strings.TrimSpace(body) + " was not booked to the ledger and needs a manual repair."
	if d.Error != "" {
		body += " " + d.Error
	}
	return body
}

// WorkerStall is background work that stopped progressing. It names the job
// kind and the reason, never the job's error text, which can name another
// merchant's records.
type WorkerStall struct {
	WorkerKind     string
	Reason         string
	IdempotencyKey string
	Metadata       map[string]any
}

// RecordWorkerStall puts a critical notification in the merchant's inbox.
// A repeated IdempotencyKey writes nothing.
func RecordWorkerStall(ctx context.Context, database *db.DB, now time.Time, stall WorkerStall) error {
	if database == nil {
		return fmt.Errorf("worker stall: database is required")
	}
	merchantID, err := merchant.Require(ctx)
	if err != nil {
		return fmt.Errorf("worker stall: %w", err)
	}
	if now.IsZero() {
		now = time.Now()
	}
	n := Notification{
		Severity: SeverityCritical,
		Title:    "Background work stalled: " + stall.WorkerKind,
		Body:     fmt.Sprintf("%s is not progressing: %s.", stall.WorkerKind, stall.Reason),
		Data: map[string]any{
			"kind": "worker_stalled", "worker_kind": stall.WorkerKind, "reason": stall.Reason,
			"metadata": stall.Metadata, "fired_at": now.UTC(),
		},
	}
	if key := strings.TrimSpace(stall.IdempotencyKey); key != "" {
		n.ID = uuidutil.DeterministicID(uuidutil.DeterministicNamespace, "worker_stall", merchantID.String(), key)
	}
	if err := newStore(database).createNotification(ctx, n); err != nil {
		return fmt.Errorf("worker stall: %w", err)
	}
	return nil
}
