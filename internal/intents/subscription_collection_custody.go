package intents

import (
	"encoding/json"
	"errors"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// ValidateSubscriptionCollectionTerminal retains the accepted obligation and
// positive terminal proof through archive/reload. Generic terminal statuses,
// disappeared provider searches and expired leases are not retry authority.
func ValidateSubscriptionCollectionTerminal(in gen.OpenrailsRailIntent) error {
	if _, err := subscriptions.DecodeSubscriptionCollectionPayload(in); err != nil {
		return err
	}
	_, paid, err := LoadCollectedReceipt(in)
	if err != nil {
		return err
	}
	refusal, declined, err := loadRebillDecline(in)
	if err != nil {
		return err
	}
	stripeCode, stripePI, stripeDeclined, err := LoadStripeRecurringDecline(in)
	if err != nil {
		return err
	}
	proof, notExecuted, err := LoadCollectionNonexecution(in)
	if err != nil {
		return err
	}
	if paid && (declined || stripeDeclined || notExecuted) {
		return errors.New("engine receipt contradicts retained refusal")
	}
	if in.Status == StatusSucceeded {
		if !paid {
			return errors.New("engine success has no qualified receipt")
		}
		return nil
	}
	if in.Status != StatusFailedTerminal || paid {
		return errors.New("engine operation has no qualified terminal outcome")
	}
	var result struct {
		Declined        bool   `json:"declined"`
		ResponseCode    int    `json:"response_code"`
		NotExecuted     bool   `json:"not_executed"`
		NotExecutedCode string `json:"not_executed_code"`
	}
	if err := json.Unmarshal(in.ResultEvidence, &result); err != nil {
		return err
	}
	if stripeDeclined {
		if !result.Declined || result.NotExecuted || result.ResponseCode != 0 || EvidenceString(in, "failure_code") != stripeCode || EvidenceString(in, "stripe_payment_intent_id") != stripePI {
			return errors.New("Stripe decline projection contradicts custody")
		}
		return nil
	}
	if declined {
		if !result.Declined || result.NotExecuted || result.ResponseCode != refusal.ResponseCode {
			return errors.New("engine decline projection contradicts custody")
		}
		return nil
	}
	if !result.NotExecuted || result.Declined || !validCollectionNonexecutionCode(result.NotExecutedCode) {
		return errors.New("engine nonexecution is unproven")
	}
	if EvidenceString(in, "submitted_at") != "" {
		code, _ := proof.Refusal()
		if !notExecuted || code != result.NotExecutedCode {
			return errors.New("fenced engine operation cannot be released without positive nonexecution")
		}
	}
	return nil
}
