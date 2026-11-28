package intents

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/history"
)

// OperationPhase is one phase of an attempt of a provider operation: an
// operation entry's action.
type OperationPhase string

const (
	OperationAttempting OperationPhase = "attempting"
	OperationSucceeded  OperationPhase = "succeeded"
	OperationFailed     OperationPhase = "failed"
	OperationUnknown    OperationPhase = "unknown"
	OperationParked     OperationPhase = "parked"
	// OperationExpired drops an operation never sent whose relevance window
	// passed: no attempt follows.
	OperationExpired OperationPhase = "expired"
)

// OperationPhases are the phases, in the order an attempt reaches them.
func OperationPhases() []OperationPhase {
	return []OperationPhase{OperationAttempting, OperationSucceeded, OperationFailed, OperationUnknown, OperationParked, OperationExpired}
}

var (
	sensitiveOperationKey = regexp.MustCompile(`(?i)(authorization|api[_-]?key|security[_-]?key|secret|password|private[_-]?key|card|pan|cvv|cvc|token)`)
	sensitiveAssignment   = regexp.MustCompile(`(?i)\b(authorization|api[_-]?key|security[_-]?key|secret|password|private[_-]?key|card[_-]?number|pan|cvv|cvc|token)\s*[:=]\s*[^,\s&]+`)
)

// LogOperation writes one phase of the intent's current attempt to the
// operations history ahead: as a statement of its own, before the provider
// call (attempting) or after it (the result), kept whatever follows. Reason
// and evidence are scrubbed of credentials and card data.
func (s *Store) LogOperation(ctx context.Context, intent gen.BillingProviderIntent, phase OperationPhase, reason string, evidence map[string]any) error {
	p, entry, err := operationEntry(ctx, intent, phase, reason, evidence)
	if err != nil {
		return err
	}
	return history.WriteAhead(ctx, s.db.DataPool(), history.OperationLog, intent.MerchantID, p, entry)
}

// recordOperation writes the phase in q's transaction, for a phase that
// commits with the intent's transition.
func recordOperation(ctx context.Context, q *gen.Queries, intent gen.BillingProviderIntent, phase OperationPhase, reason string) error {
	p, entry, err := operationEntry(ctx, intent, phase, reason, nil)
	if err != nil {
		return err
	}
	return history.Record(ctx, q, history.OperationLog, intent.MerchantID, p, entry)
}

func operationEntry(ctx context.Context, intent gen.BillingProviderIntent, phase OperationPhase, reason string, evidence map[string]any) (history.Provenance, history.Entry, error) {
	// operation_log_addressed_check: an entry records which account the
	// attempt was sent to.
	if intent.PspID == nil && intent.CustodianID == nil {
		return history.Provenance{}, history.Entry{}, fmt.Errorf("intents: operation log for %s: %w", intent.Rail, db.ErrNoPSPInContext)
	}
	payload := history.Operation{Version: history.Version, Kind: intent.IntentType, Attempt: intent.Attempts, Origin: intent.Origin,
		Executor: history.FromContext(ctx).Executor, IdempotencyKey: intent.IdempotencyKey, Reason: scrubOperationString(reason)}
	if len(evidence) > 0 {
		payload.Evidence = scrubOperationEvidence(evidence)
	}
	// An entry too large for the log is cut, never dropped: the attempt's
	// record is what an operator reads.
	payload, actor, err := payload.Fit(intentActor(intent))
	if err != nil {
		return history.Provenance{}, history.Entry{}, fmt.Errorf("intents: operation log entry: %w", err)
	}
	// The database clock stamps it, ordering it with the holds.
	p := history.ProvenanceAt(ctx, time.Time{})
	p.Actor, p.Source, p.OperationID = actor, billing.LogSourceIntent, &intent.ID
	return p, history.Entry{
		ObjectType: history.ObjectProviderIntent, ObjectID: intent.ID.String(), Action: billing.LogAction(phase), Changes: payload,
		Address: &history.Address{Rail: intent.Rail, PSPID: intent.PspID, CustodianID: intent.CustodianID, SubscriptionID: intent.SubscriptionID},
	}, nil
}

// MarkSent records, in the breaker's transaction and under its lock, that
// the claim holding attempt goes to the provider: the breaker counts it from
// then. A claim that lost the intent marks nothing.
func (s *Store) MarkSent(ctx context.Context, merchantID, id uuid.UUID, attempt int32) error {
	rows, err := s.db.Gen(ctx).MarkProviderIntentSent(ctx, gen.MarkProviderIntentSentParams{MerchantID: merchantID, ID: id, Attempt: attempt})
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("intents: attempt %d of %s is no longer in flight", attempt, id)
	}
	return nil
}

// intentActor is who wanted the intent, as history names actors; an intent
// a staff member or customer produced without naming them is the system's.
func intentActor(in gen.BillingProviderIntent) billing.LogActor {
	actor := ""
	if in.Actor != nil {
		actor = *in.Actor
	}
	switch {
	case in.Origin == string(OriginAdmin) && actor != "":
		return history.Staff(actor)
	case in.Origin == string(OriginUser) && actor != "":
		return history.Customer(actor)
	case in.Origin == string(OriginProvider):
		return history.Provider(billing.Rail(in.Rail))
	}
	return billing.LogActorSystem
}

func scrubOperationEvidence(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = scrubOperationValue(k, v)
	}
	return out
}

func scrubOperationValue(key string, v any) any {
	if sensitiveOperationKey.MatchString(key) {
		return "[redacted]"
	}
	switch x := v.(type) {
	case string:
		return scrubOperationString(x)
	case map[string]any:
		return scrubOperationEvidence(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = scrubOperationValue("", item)
		}
		return out
	default:
		return v
	}
}

func scrubOperationString(s string) string {
	return sensitiveAssignment.ReplaceAllString(s, "$1=[redacted]")
}
