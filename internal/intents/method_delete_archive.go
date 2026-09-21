package intents

import (
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
)

// DeletedMethod validates a terminal instrument decision without requiring the
// removed row. Archives use its immutable payer/ID to explain nullable history.
func DeletedMethod(in gen.OpenrailsRailIntent) (uuid.UUID, uuid.UUID, error) {
	invalid := errors.New("invalid terminal payment-method deletion")
	if in.ID == uuid.Nil || in.MerchantID == uuid.Nil || in.Status != StatusSucceeded {
		return uuid.Nil, uuid.Nil, invalid
	}
	switch in.IntentType {
	case TypeHyperSwitchMethodDelete:
		p, err := DecodeHyperSwitchMethodDelete(in)
		if err != nil {
			return uuid.Nil, uuid.Nil, err
		}
		var receipt struct {
			Deleted  bool   `json:"physically_deleted"`
			Detached bool   `json:"detached"`
			Method   string `json:"vendor_method_id"`
		}
		if json.Unmarshal(in.ResultEvidence, &receipt) != nil || receipt.Detached != p.DetachOnly || receipt.Deleted == p.DetachOnly || receipt.Method != p.Instrument.RailMethodRef {
			return uuid.Nil, uuid.Nil, invalid
		}
		if !deletedMethodActorMatches(in, p.CustomerID) {
			return uuid.Nil, uuid.Nil, invalid
		}
		return p.PaymentMethodID, p.CustomerID, nil
	case TypeNMIPaymentMethodDelete:
		p, err := decodeNMIVaultDeletePayload(in)
		if err != nil {
			return uuid.Nil, uuid.Nil, err
		}
		customer, err := uuid.Parse(p.UserID)
		if err != nil || in.Rail != "nmi" || customer == uuid.Nil || in.PspID == nil || *in.PspID == uuid.Nil || in.CustodianID != nil || in.IdempotencyKey != NMIPaymentMethodDeleteIdempotencyKey(p.PaymentMethodID) {
			return uuid.Nil, uuid.Nil, invalid
		}
		var receipt struct {
			Deleted       bool   `json:"deleted"`
			Absent        bool   `json:"verified_absent"`
			EntryAbsent   bool   `json:"verified_entry_absent"`
			AlreadyAbsent bool   `json:"already_absent"`
			NoReference   bool   `json:"no_rail_customer_ref"`
			Vault         string `json:"vault_id"`
			Billing       string `json:"billing_id"`
			Scoped        string `json:"scoped_to_billing_entry"`
		}
		if json.Unmarshal(in.ResultEvidence, &receipt) != nil {
			return uuid.Nil, uuid.Nil, invalid
		}
		if receipt.NoReference {
			if p.RailCustomerRef != "" || receipt.Deleted || receipt.Absent || receipt.EntryAbsent || receipt.AlreadyAbsent {
				return uuid.Nil, uuid.Nil, invalid
			}
		} else if !(receipt.Deleted || receipt.Absent || receipt.EntryAbsent || receipt.AlreadyAbsent) || receipt.Vault != p.RailCustomerRef || receipt.Billing != "" && receipt.Billing != p.RailMethodRef || receipt.Scoped != "" && receipt.Scoped != p.RailMethodRef {
			return uuid.Nil, uuid.Nil, invalid
		}
		if !deletedMethodActorMatches(in, customer) {
			return uuid.Nil, uuid.Nil, invalid
		}
		return p.PaymentMethodID, customer, nil
	}
	return uuid.Nil, uuid.Nil, invalid
}

func deletedMethodActorMatches(in gen.OpenrailsRailIntent, customer uuid.UUID) bool {
	switch Origin(in.Origin) {
	case OriginUser:
		if in.Actor == nil {
			return false
		}
		actor, err := uuid.Parse(*in.Actor)
		return err == nil && actor == customer
	case OriginAdmin, OriginSystem:
		return true
	default:
		return false
	}
}
