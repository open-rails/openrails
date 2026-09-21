package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// CollectionNonexecutionProof cannot be constructed from a reloaded row. It comes
// only from owning a fresh submission fence or a positive provider confirmation.
// The owner may use its capability only after a typed pre-dispatch refusal.
type CollectionNonexecutionProof struct {
	binding      receiptBinding
	submittedAt  string
	code, reason string
}

func (p CollectionNonexecutionProof) permits(in gen.OpenrailsRailIntent) bool {
	binding, err := collectionBinding(in)
	return err == nil && p.binding == binding && p.submittedAt == EvidenceString(in, "submitted_at")
}

// BeginCollectedPayment mints authority only for the writer of a fresh fence.
func (s *Store) BeginCollectedPayment(ctx context.Context, in gen.OpenrailsRailIntent, now time.Time) (CollectionNonexecutionProof, bool, error) {
	if in.IntentType != "invoice_collection" && in.IntentType != subscriptions.TypeSubscriptionCollection {
		return CollectionNonexecutionProof{}, false, errors.New("submission received an unsupported collected-payment kind")
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return CollectionNonexecutionProof{}, false, err
	}
	marker := now.UTC().Format(time.RFC3339Nano)
	first, err := s.RecordProgressIfAbsent(ctx, in.ID, "submitted_at", marker)
	if err != nil || !first {
		return CollectionNonexecutionProof{}, false, err
	}
	return CollectionNonexecutionProof{binding: binding, submittedAt: marker}, true, nil
}

// ConfirmCollectedPaymentNotExecuted preserves the existing positive provider-read
// authority. A missing search result is not a confirmation under this contract.
func (s *Store) ConfirmCollectedPaymentNotExecuted(ctx context.Context, in gen.OpenrailsRailIntent, verifier interface {
	ConfirmCollectionNotExecuted(context.Context, gen.OpenrailsRailIntent) error
}) (CollectionNonexecutionProof, error) {
	if verifier == nil || (in.IntentType != "invoice_collection" && in.IntentType != subscriptions.TypeSubscriptionCollection) {
		return CollectionNonexecutionProof{}, errors.New("invoice nonexecution verifier is unavailable")
	}
	current, err := s.Get(ctx, in.ID)
	if err != nil {
		return CollectionNonexecutionProof{}, err
	}
	binding, err := collectionBinding(current)
	if err != nil {
		return CollectionNonexecutionProof{}, err
	}
	expected, err := collectionBinding(in)
	if err != nil {
		return CollectionNonexecutionProof{}, err
	}
	if binding != expected {
		return CollectionNonexecutionProof{}, errors.New("invoice confirmation changed operation binding")
	}
	if EvidenceString(current, "submitted_at") == "" {
		return CollectionNonexecutionProof{}, errors.New("unsubmitted invoice must use atomic unsent resolution")
	}
	if err := verifier.ConfirmCollectionNotExecuted(ctx, current); err != nil {
		return CollectionNonexecutionProof{}, err
	}
	return CollectionNonexecutionProof{binding: binding, submittedAt: EvidenceString(current, "submitted_at")}, nil
}

// Keep the existing persisted key while both collected-payment kinds share its sealed proof.
const qualifiedCollectionNonexecutionKey = "qualified_invoice_nonexecution"

type collectionNonexecution struct {
	Binding     receiptBinding `json:"binding"`
	SubmittedAt string         `json:"submitted_at"`
	Code        string         `json:"code"`
	Reason      string         `json:"reason"`
}

// LoadCollectionNonexecution validates sealed custody against this operation and fence.
func LoadCollectionNonexecution(in gen.OpenrailsRailIntent) (CollectionNonexecutionProof, bool, error) {
	var evidence map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return CollectionNonexecutionProof{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return CollectionNonexecutionProof{}, false, err
	}
	raw, found := evidence[qualifiedCollectionNonexecutionKey]
	if !found {
		return CollectionNonexecutionProof{}, false, nil
	}
	var fact collectionNonexecution
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fact); err != nil {
		return CollectionNonexecutionProof{}, true, err
	}
	proof := CollectionNonexecutionProof{binding: fact.Binding, submittedAt: fact.SubmittedAt, code: fact.Code, reason: fact.Reason}
	if fact.SubmittedAt == "" || !proof.permits(in) || !validCollectionNonexecutionCode(fact.Code) || fact.Reason == "" {
		return CollectionNonexecutionProof{}, true, errors.New("invalid invoice nonexecution custody")
	}
	return proof, true, nil
}

// Refusal returns the qualified refusal retained for local completion.
func (p CollectionNonexecutionProof) Refusal() (string, string) { return p.code, p.reason }
func validCollectionNonexecutionCode(code string) bool {
	return code == "not_dispatched" || code == "instrument_changed" || code == "not_executed"
}

// RetainCollectionNonexecution uses the existing sealed-evidence writer, before
// local effects. Generic progress/outcome maps cannot manufacture this custody.
func (s *Store) RetainCollectionNonexecution(ctx context.Context, in gen.OpenrailsRailIntent, proof CollectionNonexecutionProof, code, reason string) error {
	binding, err := collectionBinding(in)
	if err != nil {
		return err
	}
	if binding != proof.binding || !validCollectionNonexecutionCode(code) || reason == "" {
		return errors.New("invalid invoice nonexecution capability")
	}
	current, err := s.retainQualifiedEvidence(ctx, in, qualifiedCollectionNonexecutionKey, collectionNonexecution{proof.binding, proof.submittedAt, code, reason})
	if err != nil {
		return err
	}
	_, found, err := LoadCollectionNonexecution(current)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("invoice nonexecution custody disappeared")
	}
	return nil
}
