package intents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

const qualifiedInitialRefusalKey = "qualified_initial_refusal"

type InitialEnrollmentRefusal struct{ data initialEnrollmentRefusal }
type initialEnrollmentRefusal struct {
	Binding        receiptBinding `json:"binding"`
	Kind           string         `json:"kind"`
	ResponseCode   int            `json:"response_code"`
	LocalizationID string         `json:"localization_id"`
}

func (r InitialEnrollmentRefusal) Validate(in gen.OpenrailsRailIntent) error {
	if _, err := subscriptions.DecodeNMIInitialEnrollmentPayload(in); err != nil {
		return err
	}
	binding, err := collectionBinding(in)
	if err != nil || binding != r.data.Binding {
		return errors.New("initial refusal belongs to another accepted operation")
	}
	var evidence map[string]json.RawMessage
	if len(in.ResultEvidence) > 0 && json.Unmarshal(in.ResultEvidence, &evidence) != nil {
		return errors.New("invalid initial refusal custody")
	}
	if _, ok := evidence[qualifiedReceiptKey]; ok {
		return errors.New("initial refusal contradicts retained money")
	}
	if _, ok := evidence[qualifiedEnrollmentKey]; ok {
		return errors.New("initial refusal contradicts retained schedule")
	}
	switch r.data.Kind {
	case "provider_declined":
		if string(evidence["enrollment_submitted"]) != "true" || r.data.ResponseCode < 200 || r.data.ResponseCode >= 300 {
			return errors.New("initial decline has no exact submitted provider refusal")
		}
	case "not_submitted":
		if _, present := evidence["enrollment_submitted"]; present || r.data.ResponseCode != 0 || r.data.LocalizationID != "" {
			return errors.New("initial unsent closure contradicts submission")
		}
	default:
		return errors.New("unsupported initial refusal custody")
	}
	return nil
}

func (r InitialEnrollmentRefusal) Outcome() Outcome {
	if r.data.Kind == "provider_declined" {
		return TerminalWithEvidence("native enrollment declined", map[string]any{"declined": true, "response_code": r.data.ResponseCode, "localization_id": r.data.LocalizationID})
	}
	return TerminalWithEvidence("native enrollment was never submitted", map[string]any{"not_executed": true})
}

func LoadInitialEnrollmentRefusal(in gen.OpenrailsRailIntent) (InitialEnrollmentRefusal, bool, error) {
	var evidence map[string]json.RawMessage
	if len(in.ResultEvidence) == 0 {
		return InitialEnrollmentRefusal{}, false, nil
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return InitialEnrollmentRefusal{}, false, err
	}
	raw, found := evidence[qualifiedInitialRefusalKey]
	if !found {
		return InitialEnrollmentRefusal{}, false, nil
	}
	var out InitialEnrollmentRefusal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out.data); err != nil {
		return out, true, err
	}
	return out, true, out.Validate(in)
}

// RetainInitialEnrollmentDecline is called only with the account-bound native
// submission's structured rejection. Generic progress cannot create this key.
// response=3 errors are not promoted to proof of no provider-side effects.
func (s *Store) RetainInitialEnrollmentDecline(ctx context.Context, in gen.OpenrailsRailIntent, rejection *nmi.CustomerVaultError) error {
	if rejection == nil {
		return errors.New("initial decline requires a provider rejection")
	}
	fields, err := url.ParseQuery(rejection.RawResponse)
	if err != nil {
		return err
	}
	code, err := strconv.Atoi(fields.Get("response_code"))
	if err != nil || fields.Get("response") != "2" || code != rejection.ResponseCode {
		return errors.New("initial rejection is not a qualified provider decline")
	}
	current, err := s.Get(ctx, in.ID)
	if err != nil {
		return err
	}
	binding, err := collectionBinding(current)
	if err != nil {
		return err
	}
	fact := InitialEnrollmentRefusal{initialEnrollmentRefusal{Binding: binding, Kind: "provider_declined", ResponseCode: code, LocalizationID: rejection.LocalizationID}}
	if err := fact.Validate(current); err != nil {
		return err
	}
	_, err = s.retainQualifiedEvidence(ctx, current, qualifiedInitialRefusalKey, fact.data)
	return err
}

// RetainUnsubmittedInitialEnrollment runs under the canonical completion lock;
// SQL also checks the fence so a concurrent submission cannot turn into absence.
func (s *Store) RetainUnsubmittedInitialEnrollment(ctx context.Context, in gen.OpenrailsRailIntent) error {
	binding, err := collectionBinding(in)
	if err != nil {
		return err
	}
	fact := InitialEnrollmentRefusal{initialEnrollmentRefusal{Binding: binding, Kind: "not_submitted"}}
	if err := fact.Validate(in); err != nil {
		return err
	}
	_, err = s.retainQualifiedEvidence(ctx, in, qualifiedInitialRefusalKey, fact.data)
	return err
}
