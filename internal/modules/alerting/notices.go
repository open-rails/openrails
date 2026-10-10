package alerting

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/reconcile"
)

// Operational problems a person must look at are findings, in the one queue
// the console and the findings API read.
const (
	// FindingLedgerUnbooked is a provider event OpenRails could not book; the
	// merchant repairs the ledger by hand and resolves the finding.
	FindingLedgerUnbooked reconcile.FindingType = "consistency.ledger.unbooked"
	// FindingWorkerStalled is background work that stopped progressing. It
	// resolves itself when the work progresses again.
	FindingWorkerStalled reconcile.FindingType = "life.worker.stalled"
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

// RecordLedgerRepair raises a consistency.ledger.unbooked finding. Its subject
// is the IdempotencyKey, else the provider's transaction: a repeat observes
// the same finding again.
func RecordLedgerRepair(ctx context.Context, database *db.DB, now time.Time, repair LedgerRepair) error {
	if database == nil {
		return fmt.Errorf("ledger repair: database is required")
	}
	provider := strings.TrimSpace(repair.Provider)
	operation := strings.TrimSpace(repair.Operation)
	transaction := strings.TrimSpace(repair.TransactionID)
	evidence := map[string]any{"provider": provider, "operation": operation, "fired_at": now.UTC().Format(time.RFC3339)}
	if transaction != "" {
		evidence["transaction_id"] = transaction
	}
	if customer := strings.TrimSpace(repair.CustomerID); customer != "" {
		id, err := billing.ParseCustomerID(customer)
		if err != nil {
			return fmt.Errorf("ledger repair: customer: %w", err)
		}
		evidence["customer_id"] = id.String()
	}
	if repair.OriginalPaymentID != nil {
		evidence["original_payment_id"] = billing.PaymentID(*repair.OriginalPaymentID).String()
	}
	if repair.SubscriptionID != nil {
		evidence["subscription_id"] = billing.SubscriptionID(*repair.SubscriptionID).String()
	}
	if repair.Err != nil {
		evidence["error"] = repair.Err.Error()
	}
	metadata := map[string]any{}
	for key, value := range repair.Metadata {
		if strings.TrimSpace(key) != "" {
			metadata[key] = value
		}
	}
	if len(metadata) > 0 {
		evidence["metadata"] = metadata
	}
	subject := strings.TrimSpace(repair.IdempotencyKey)
	if subject == "" {
		subject = strings.Join([]string{provider, operation, transaction}, ":")
		if transaction == "" {
			subject += ":" + uuid.NewString()
		}
	}
	action := strings.TrimSpace(provider + " " + operation)
	if transaction != "" {
		action += " (transaction " + transaction + ")"
	}
	action += " was not booked to the ledger and needs a manual repair."
	if repair.Err != nil {
		action += " " + repair.Err.Error()
	}
	if _, err := (&reconcile.PGStore{DB: database}).RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingLedgerUnbooked, SubjectKey: subject, Severity: reconcile.SeverityCritical,
		RecommendedAction: action, Evidence: evidence,
	}); err != nil {
		return fmt.Errorf("ledger repair: %w", err)
	}
	return nil
}

// WorkerStall is background work that stopped progressing. It names the job
// kind and the reason, never the job's error text, which can name another
// merchant's records.
type WorkerStall struct {
	WorkerKind string
	Reason     string
	Metadata   map[string]any
}

// RecordWorkerStall raises a life.worker.stalled finding for the worker kind.
func RecordWorkerStall(ctx context.Context, database *db.DB, now time.Time, stall WorkerStall) error {
	if database == nil {
		return fmt.Errorf("worker stall: database is required")
	}
	evidence := map[string]any{"worker_kind": stall.WorkerKind, "reason": stall.Reason, "fired_at": now.UTC().Format(time.RFC3339)}
	if len(stall.Metadata) > 0 {
		evidence["metadata"] = stall.Metadata
	}
	if _, err := (&reconcile.PGStore{DB: database}).RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingWorkerStalled, SubjectKey: stall.WorkerKind, Severity: reconcile.SeverityCritical,
		RecommendedAction: fmt.Sprintf("%s is not progressing: %s. It resolves itself when the work progresses again.", stall.WorkerKind, stall.Reason),
		Evidence:          evidence,
	}); err != nil {
		return fmt.Errorf("worker stall: %w", err)
	}
	return nil
}

// ResolveWorkerStall resolves the worker kind's stall finding once its work
// progresses again.
func ResolveWorkerStall(ctx context.Context, database *db.DB, workerKind string) error {
	if database == nil {
		return fmt.Errorf("worker stall: database is required")
	}
	return (&reconcile.PGStore{DB: database}).ResolveRaisedFinding(ctx, FindingWorkerStalled, workerKind)
}
