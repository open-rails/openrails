package intents

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments"
)

// ValidateNMISaleTerminal rechecks the complete accepted purchase and retained
// payment custody when an archive reloads a terminal result.
func ValidateNMISaleTerminal(in gen.OpenrailsRailIntent) error {
	if _, err := payments.DecodeNMISalePayload(in); err != nil {
		return err
	}
	receipt, paid, err := LoadCollectedReceipt(in)
	if err != nil {
		return err
	}
	var evidence struct {
		TransactionID  string    `json:"transaction_id"`
		PaymentID      uuid.UUID `json:"payment_id"`
		Declined       bool      `json:"declined"`
		NotExecuted    bool      `json:"not_executed"`
		RequestRefused bool      `json:"request_refused"`
		Submitted      bool      `json:"sale_submitted"`
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return err
	}
	switch in.Status {
	case StatusSucceeded:
		if !paid || evidence.Declined || evidence.NotExecuted || evidence.RequestRefused || evidence.PaymentID == uuid.Nil || evidence.TransactionID != receipt.TransactionID() {
			return errors.New("sale terminal result contradicts its qualified payment")
		}
	case StatusFailedTerminal:
		if paid || (!evidence.Declined && !evidence.NotExecuted && !evidence.RequestRefused) || (evidence.NotExecuted && evidence.Submitted) {
			return errors.New("sale refusal is unqualified or contradicts retained payment")
		}
	default:
		return errors.New("sale archive requires a terminal payment decision")
	}
	return nil
}
