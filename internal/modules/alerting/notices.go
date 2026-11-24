package alerting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/reconcile"
)

// Operational problems a person must look at are findings, in the one queue
// the console and the findings API read.
const (
	// FindingLedgerUnbooked is a provider event OpenRails could not book; the
	// merchant repairs the ledger by hand and resolves the finding.
	FindingLedgerUnbooked reconcile.FindingType = "consistency.ledger.unbooked"
	// FindingSolanaCrankerLowGas is a Solana cranker wallet low on the SOL its
	// pulls pay fees with. It resolves itself once a pull pass finds it funded.
	FindingSolanaCrankerLowGas reconcile.FindingType = "life.solana.cranker_low_gas"
	// FindingCustomerMessagesRateLimited is a customer whose messages reached
	// the hourly cap: something sends them far more than billing should.
	FindingCustomerMessagesRateLimited reconcile.FindingType = "life.customer.messages_rate_limited"
	// FindingCustomerMessagesSenderRefused is a mail server that refused the
	// merchant's sender, so its customer messages wait for it.
	FindingCustomerMessagesSenderRefused reconcile.FindingType = "life.customer.messages_sender_refused"
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
			// The same unbookable event is the same finding.
			subject += ":" + repairDigest(evidence["customer_id"], evidence["original_payment_id"], evidence["subscription_id"], evidence["error"])
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
	rec, err := (&reconcile.PGStore{DB: database}).RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingLedgerUnbooked, SubjectKey: subject, Severity: reconcile.SeverityCritical,
		RecommendedAction: action, Evidence: evidence,
	})
	if err != nil {
		return fmt.Errorf("ledger repair: %w", err)
	}
	alertRaised(ctx, database, rec, now)
	return nil
}

// repairDigest names an unbookable event by what it is about.
func repairDigest(parts ...any) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

// alertRaised tells the merchant's operator of a finding a person must look
// at. The finding stays in the console whatever happens to its alert.
func alertRaised(ctx context.Context, database *db.DB, rec reconcile.FindingRecord, now time.Time) {
	if err := notifyFinding(ctx, database, rec, now); err != nil && ctx.Err() == nil && database.Log != nil {
		database.Log.WarnContext(ctx, "alerting the operator failed; the finding stays in the console", logx.Err(err), "finding_id", rec.ID, "finding_type", rec.Type)
	}
}

// CrankerGas is one Solana cranker wallet's SOL balance on a merchant's PSP.
type CrankerGas struct {
	PSPID     uuid.UUID
	Wallet    string
	Network   string
	Lamports  uint64
	Threshold uint64
}

func (g CrankerGas) subject() string { return g.PSPID.String() + ":" + g.Wallet }

// RecordCrankerGas raises the wallet's low-gas finding when it is below its
// threshold, and resolves it when it is not.
func RecordCrankerGas(ctx context.Context, database *db.DB, now time.Time, gas CrankerGas) error {
	if database == nil {
		return fmt.Errorf("cranker gas: database is required")
	}
	store := &reconcile.PGStore{DB: database}
	if gas.Lamports >= gas.Threshold {
		return store.ResolveRaisedFinding(ctx, FindingSolanaCrankerLowGas, gas.subject())
	}
	rec, err := store.RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingSolanaCrankerLowGas, SubjectKey: gas.subject(), Severity: reconcile.SeverityHigh,
		RecommendedAction: fmt.Sprintf("Solana cranker %s on %s has %d lamports, below %d: top it up with SOL or its pulls fail. It resolves itself once a pull pass finds it funded.",
			gas.Wallet, gas.Network, gas.Lamports, gas.Threshold),
		Evidence: map[string]any{"psp_id": gas.PSPID.String(), "wallet": gas.Wallet, "network": gas.Network,
			"lamports": gas.Lamports, "threshold_lamports": gas.Threshold, "observed_at": now.UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return fmt.Errorf("cranker gas: %w", err)
	}
	alertRaised(ctx, database, rec, now)
	return nil
}

// MessageRateLimit is a customer message held back by the hourly cap.
type MessageRateLimit struct {
	CustomerID uuid.UUID
	MessageID  uuid.UUID
	EventType  string
	Cap        int
	Window     time.Duration
}

// RecordMessageRateLimit raises the customer's rate-limited finding; a later
// message held back observes it again.
func RecordMessageRateLimit(ctx context.Context, database *db.DB, now time.Time, limit MessageRateLimit) error {
	if database == nil {
		return fmt.Errorf("message rate limit: database is required")
	}
	customer := billing.CustomerID(limit.CustomerID).String()
	rec, err := (&reconcile.PGStore{DB: database}).RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingCustomerMessagesRateLimited, SubjectKey: customer, Severity: reconcile.SeverityMedium,
		RecommendedAction: fmt.Sprintf("Customer %s reached %d messages in %s, so a %s message was not sent. Find what records so many billing facts for them.",
			customer, limit.Cap, limit.Window, limit.EventType),
		Evidence: map[string]any{"customer_id": customer, "message_id": limit.MessageID.String(), "event_type": limit.EventType,
			"cap": limit.Cap, "window_seconds": int64(limit.Window / time.Second), "observed_at": now.UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return fmt.Errorf("message rate limit: %w", err)
	}
	alertRaised(ctx, database, rec, now)
	return nil
}

// MessageSenderRefusal is a mail server's refusal of the sender of a
// customer message: its credentials (AUTH) or its from address (MAIL FROM).
type MessageSenderRefusal struct {
	// From is the merchant's from address; empty is the sender's own.
	From      string
	Step      string
	Code      int
	Reply     string
	MessageID uuid.UUID
	EventType string
}

// senderSubject is the sender-refused finding's subject for from, the
// merchant's from address; empty is the deployment's sender.
func senderSubject(from string) string {
	if from == "" {
		return "deployment_sender"
	}
	return from
}

// RecordMessageSenderRefused raises the merchant's sender-refused finding,
// one per from address; a later refusal observes it again. Its alert email
// goes through the sender that refused, so it may not arrive; the console
// and webhooks show it.
func RecordMessageSenderRefused(ctx context.Context, database *db.DB, now time.Time, r MessageSenderRefusal) error {
	if database == nil {
		return fmt.Errorf("message sender refused: database is required")
	}
	subject, sender := senderSubject(r.From), "from_email "+r.From
	if r.From == "" {
		sender = "the deployment's sender"
	}
	rec, err := (&reconcile.PGStore{DB: database}).RaiseFinding(ctx, reconcile.RaisedFinding{
		Type: FindingCustomerMessagesSenderRefused, SubjectKey: subject, Severity: reconcile.SeverityHigh,
		RecommendedAction: fmt.Sprintf("The mail server refused %s at %s (%d %s), so customer messages are not sent; each is retried. Fix the sender's credentials or let it send as this address.",
			sender, r.Step, r.Code, r.Reply),
		Evidence: map[string]any{"from_email": r.From, "step": r.Step, "reply_code": r.Code, "reply": r.Reply,
			"message_id": r.MessageID.String(), "event_type": r.EventType, "observed_at": now.UTC().Format(time.RFC3339)},
	})
	if err != nil {
		return fmt.Errorf("message sender refused: %w", err)
	}
	alertRaised(ctx, database, rec, now)
	return nil
}

// ResolveMessageSenderRefused closes the sender-refused finding of from once
// the sender accepts a message from it.
func ResolveMessageSenderRefused(ctx context.Context, database *db.DB, from string) error {
	if err := (&reconcile.PGStore{DB: database}).ResolveRaisedFinding(ctx, FindingCustomerMessagesSenderRefused, senderSubject(from)); err != nil {
		return fmt.Errorf("message sender recovered: %w", err)
	}
	return nil
}
