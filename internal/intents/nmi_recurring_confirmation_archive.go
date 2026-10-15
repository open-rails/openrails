package intents

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// ValidateNMIRecurringConfirmationTerminal preserves the accepted confirmation
// and its answer at export/restore. It never turns unfinished work into a
// portable success or discards evidence to make an archive pass.
func ValidateNMIRecurringConfirmationTerminal(in gen.BillingProviderIntent) error {
	p, err := decodeConfirmation(in)
	if err != nil {
		return err
	}
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || p.CustomerID == uuid.Nil || p.PaymentMethodID == uuid.Nil || p.SubscriptionID == uuid.Nil || !rails.IsNMI(models.Rail(p.Rail)) || strings.TrimSpace(p.Schedule) == "" || in.Origin != string(OriginUser) || in.Actor == nil || *in.Actor != p.CustomerID.String() {
		return errors.New("confirmation has no valid customer authority")
	}
	if err := p.Instrument.Validate(); err != nil {
		return err
	}
	if p.Instrument.Custodian != models.CustodianPSP || p.Instrument.Mandate != nil || strings.TrimSpace(p.Instrument.RailCustomerRef) == "" {
		return errors.New("confirmation does not address a PSP-held card")
	}
	raw, err := json.Marshal(p.confirmationTerms)
	if err != nil {
		return err
	}
	episode := uuid.NewSHA1(in.MerchantID, raw).String()
	if in.IdempotencyKey != TypeNMIRecurringConfirmation+":"+episode || p.OrderReference != "pmr-"+episode {
		return errors.New("confirmation identity does not match accepted terms")
	}
	var evidence struct {
		Submitted     string                    `json:"confirmation_submitted_at"`
		Answer        *nmi.VerificationEvidence `json:"confirmation_answer"`
		NotExecuted   bool                      `json:"not_executed"`
		TransactionID string                    `json:"transaction_id"`
		ResponseCode  int                       `json:"response_code"`
	}
	if err := json.Unmarshal(in.ResultEvidence, &evidence); err != nil {
		return err
	}
	if in.Status != StatusSucceeded && in.Status != StatusFailedTerminal {
		return errors.New("confirmation is not terminal")
	}
	if evidence.NotExecuted {
		if in.Status != StatusFailedTerminal || evidence.Answer != nil || evidence.Submitted != "" || evidence.TransactionID != "" || evidence.ResponseCode != 0 {
			return errors.New("confirmation nonexecution contradicts retained evidence")
		}
		return nil
	}
	submitted, err := time.Parse(time.RFC3339Nano, evidence.Submitted)
	if err != nil || submitted.IsZero() || evidence.Answer == nil {
		return errors.New("confirmation has no retained submission and answer")
	}
	answer := *evidence.Answer
	if answer.TransactionID != evidence.TransactionID || answer.ResponseCode != evidence.ResponseCode {
		return errors.New("confirmation result contradicts retained answer")
	}
	approved := answer.ResponseCode == 100 && strings.TrimSpace(answer.TransactionID) != ""
	declined := answer.ResponseCode >= 200 && answer.ResponseCode < 300
	if !approved && !declined || in.Status == StatusSucceeded && !approved {
		return errors.New("confirmation has no conclusive terminal answer")
	}
	return nil
}
