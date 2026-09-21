package intents

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// ValidateInitialMembershipTerminal revalidates retained immutable custody. A
// future schedule and a free initial phase never stand in for collected money.
func ValidateInitialMembershipTerminal(in gen.OpenrailsRailIntent) error {
	p, err := subscriptions.DecodeInitialMembershipPayload(in)
	if err != nil {
		return err
	}
	schedule, scheduled, err := LoadNMIEnrollmentReceipt(in)
	if err != nil {
		return err
	}
	receipt, paid, err := LoadCollectedReceipt(in)
	if err != nil {
		return err
	}
	var evidence struct {
		SubscriptionID         uuid.UUID `json:"subscription_id"`
		ProviderSubscriptionID string    `json:"provider_subscription_id"`
		TransactionID          string    `json:"transaction_id"`
		Status                 string    `json:"status"`
		DelayedStart           string    `json:"delayed_start"`
		Submitted              bool      `json:"initial_submitted"`
		Declined               bool      `json:"declined"`
		NotExecuted            bool      `json:"not_executed"`
		RequestRefused         bool      `json:"request_refused"`
		ResponseCode           int       `json:"response_code"`
		LocalizationID         string    `json:"localization_id"`
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return err
	}
	switch in.Status {
	case StatusSucceeded:
		if !scheduled || !evidence.Submitted || evidence.SubscriptionID != p.Terms.SubscriptionID || evidence.ProviderSubscriptionID != schedule.SubscriptionID() || evidence.Declined || evidence.NotExecuted || evidence.RequestRefused || (p.Terms.Amount > 0) != paid {
			return errors.New("initial terminal result contradicts accepted schedule and payment custody")
		}
		if paid && evidence.TransactionID != receipt.TransactionID() || !paid && evidence.TransactionID != "" {
			return errors.New("initial terminal result has another payment")
		}
		if p.Terms.Pending {
			if evidence.Status != "pending" || evidence.DelayedStart != p.DelayedStart().UTC().Format("2006-01-02T15:04:05.999999999Z07:00") {
				return errors.New("initial delayed result contradicts accepted schedule")
			}
		} else if evidence.Status != "success" || evidence.DelayedStart != "" {
			return errors.New("initial immediate result has another phase")
		}
	case StatusFailedTerminal:
		refusal, refused, err := LoadInitialMembershipRefusal(in)
		if err != nil {
			return err
		}
		if !refused {
			return errors.New("initial terminal refusal has no bound custody")
		}
		declined := refusal.data.Kind == "provider_declined"
		if scheduled || paid || evidence.RequestRefused || evidence.Declined != declined || evidence.NotExecuted == declined || evidence.Submitted != declined || evidence.ResponseCode != refusal.data.ResponseCode || evidence.LocalizationID != refusal.data.LocalizationID || evidence.SubscriptionID != uuid.Nil || evidence.ProviderSubscriptionID != "" || evidence.TransactionID != "" {
			return errors.New("initial refusal contradicts provider custody or its submission fence")
		}
	default:
		return errors.New("initial archive requires a terminal enrollment decision")
	}
	return nil
}
