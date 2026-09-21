package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// InvoiceNonexecutionProof cannot be constructed from a reloaded row. It comes
// only from owning a fresh submission fence or a positive provider confirmation.
// The owner may use its capability only after a typed pre-dispatch refusal.
type InvoiceNonexecutionProof struct {
	binding      receiptBinding
	submittedAt  string
	code, reason string
}

func (p InvoiceNonexecutionProof) permits(in gen.OpenrailsRailIntent) bool {
	binding, err := collectionBinding(in)
	return err == nil && p.binding == binding && p.submittedAt == EvidenceString(in, "submitted_at")
}

// BeginInvoiceCollection mints authority only for the writer of a fresh fence.
func (s *Store) BeginInvoiceCollection(ctx context.Context, in gen.OpenrailsRailIntent, now time.Time) (InvoiceNonexecutionProof, bool, error) {
	if in.IntentType != "invoice_collection" {
		return InvoiceNonexecutionProof{}, false, errors.New("invoice submission received another operation kind")
	}
	binding, err := collectionBinding(in)
	if err != nil {
		return InvoiceNonexecutionProof{}, false, err
	}
	marker := now.UTC().Format(time.RFC3339Nano)
	first, err := s.RecordProgressIfAbsent(ctx, in.ID, "submitted_at", marker)
	if err != nil || !first {
		return InvoiceNonexecutionProof{}, false, err
	}
	return InvoiceNonexecutionProof{binding: binding, submittedAt: marker}, true, nil
}

// ConfirmInvoiceNotExecuted preserves the existing positive provider-read
// authority. A missing search result is not a confirmation under this contract.
func (s *Store) ConfirmInvoiceNotExecuted(ctx context.Context, in gen.OpenrailsRailIntent, verifier interface {
	ConfirmCollectionNotExecuted(context.Context, gen.OpenrailsRailIntent) error
}) (InvoiceNonexecutionProof, error) {
	if verifier == nil || in.IntentType != "invoice_collection" {
		return InvoiceNonexecutionProof{}, errors.New("invoice nonexecution verifier is unavailable")
	}
	current, err := s.Get(ctx, in.ID)
	if err != nil {
		return InvoiceNonexecutionProof{}, err
	}
	binding, err := collectionBinding(current)
	if err != nil {
		return InvoiceNonexecutionProof{}, err
	}
	expected, err := collectionBinding(in)
	if err != nil {
		return InvoiceNonexecutionProof{}, err
	}
	if binding != expected {
		return InvoiceNonexecutionProof{}, errors.New("invoice confirmation changed operation binding")
	}
	if EvidenceString(current, "submitted_at") == "" {
		return InvoiceNonexecutionProof{}, errors.New("unsubmitted invoice must use atomic unsent resolution")
	}
	if err := verifier.ConfirmCollectionNotExecuted(ctx, current); err != nil {
		return InvoiceNonexecutionProof{}, err
	}
	return InvoiceNonexecutionProof{binding: binding, submittedAt: EvidenceString(current, "submitted_at")}, nil
}

const qualifiedInvoiceNonexecutionKey = "qualified_invoice_nonexecution"

type invoiceNonexecution struct {
	Binding     receiptBinding `json:"binding"`
	SubmittedAt string         `json:"submitted_at"`
	Code        string         `json:"code"`
	Reason      string         `json:"reason"`
}

// LoadInvoiceNonexecution validates sealed custody against this operation and fence.
func LoadInvoiceNonexecution(in gen.OpenrailsRailIntent) (InvoiceNonexecutionProof, bool, error) {
	var evidence map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return InvoiceNonexecutionProof{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return InvoiceNonexecutionProof{}, false, err
	}
	raw, found := evidence[qualifiedInvoiceNonexecutionKey]
	if !found {
		return InvoiceNonexecutionProof{}, false, nil
	}
	var fact invoiceNonexecution
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fact); err != nil {
		return InvoiceNonexecutionProof{}, true, err
	}
	proof := InvoiceNonexecutionProof{binding: fact.Binding, submittedAt: fact.SubmittedAt, code: fact.Code, reason: fact.Reason}
	if fact.SubmittedAt == "" || !proof.permits(in) || !validInvoiceNonexecutionCode(fact.Code) || fact.Reason == "" {
		return InvoiceNonexecutionProof{}, true, errors.New("invalid invoice nonexecution custody")
	}
	return proof, true, nil
}

// Refusal returns the qualified refusal retained for local completion.
func (p InvoiceNonexecutionProof) Refusal() (string, string) { return p.code, p.reason }
func validInvoiceNonexecutionCode(code string) bool {
	return code == "not_dispatched" || code == "instrument_changed" || code == "not_executed"
}

// RetainInvoiceNonexecution uses the existing sealed-evidence writer, before
// local effects. Generic progress/outcome maps cannot manufacture this custody.
func (s *Store) RetainInvoiceNonexecution(ctx context.Context, in gen.OpenrailsRailIntent, proof InvoiceNonexecutionProof, code, reason string) error {
	binding, err := collectionBinding(in)
	if err != nil {
		return err
	}
	if binding != proof.binding || !validInvoiceNonexecutionCode(code) || reason == "" {
		return errors.New("invalid invoice nonexecution capability")
	}
	current, err := s.retainQualifiedEvidence(ctx, in, qualifiedInvoiceNonexecutionKey, invoiceNonexecution{proof.binding, proof.submittedAt, code, reason})
	if err != nil {
		return err
	}
	_, found, err := LoadInvoiceNonexecution(current)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("invoice nonexecution custody disappeared")
	}
	return nil
}
