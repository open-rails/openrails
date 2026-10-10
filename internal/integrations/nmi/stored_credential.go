package nmi

import (
	"errors"
	"net/url"
	"strings"
)

// StoredCredential carries NMI's credential-on-file (CIT/MIT) wire fields, as
// the NMI portal's "Credential on File Information" documents them:
//
//	initiated_by                customer | merchant
//	stored_credential_indicator stored | used   (singular "credential")
//	initial_transaction_id      NMI's transactionid of the sequence's initial
//	                            CIT (NMI maps it to the network id), never a raw NTID
//	billing_method              recurring, on recurring-agreement charges only
//
// The portal's canonical combinations:
//
//	recurring initial CIT:  billing_method=recurring initiated_by=customer stored_credential_indicator=stored
//	recurring MIT:          billing_method=recurring initiated_by=merchant stored_credential_indicator=used initial_transaction_id=…
//	unscheduled initial CIT:                         initiated_by=customer stored_credential_indicator=stored
//	unscheduled CIT reuse:                           initiated_by=customer stored_credential_indicator=used
//	unscheduled MIT:                                 initiated_by=merchant stored_credential_indicator=used initial_transaction_id=…
//
// References do not cross agreement types.
type StoredCredential struct {
	InitiatedBy          string // "customer" | "merchant"
	Indicator            string // "stored" | "used"
	InitialTransactionID string // "" only on the initial customer-present transaction
	// Recurring: stamp billing_method=recurring (recurring-agreement charge).
	// False = unscheduled credential-on-file: no billing_method on the wire.
	Recurring bool
}

// StoredCredential wire values.
const (
	InitiatedByCustomer = "customer"
	InitiatedByMerchant = "merchant"
	IndicatorStored     = "stored"
	IndicatorUsed       = "used"
)

// Validate rejects absent or malformed CIT/MIT field combinations. Lanes that
// always run on a stored credential (schedules, rebills, verifications) refuse
// nil. Exported for sibling rails composing the same form.
func (sc *StoredCredential) Validate() error {
	if sc == nil {
		return errors.New("stored credential indicators are required")
	}
	if sc.InitiatedBy != InitiatedByCustomer && sc.InitiatedBy != InitiatedByMerchant {
		return errors.New("stored credential initiated_by must be customer or merchant")
	}
	if sc.Indicator != IndicatorStored && sc.Indicator != IndicatorUsed {
		return errors.New("stored credential indicator must be stored or used")
	}

	initialTransactionID := strings.TrimSpace(sc.InitialTransactionID)
	switch sc.Indicator {
	case IndicatorStored:
		if sc.InitiatedBy != InitiatedByCustomer {
			return errors.New("initial stored credential transaction must be customer initiated")
		}
		if initialTransactionID != "" {
			return errors.New("initial stored credential transaction must not carry initial_transaction_id")
		}
	case IndicatorUsed:
		if initialTransactionID == "" {
			return errors.New("subsequent stored credential transaction requires initial_transaction_id")
		}
	}
	return nil
}

// ApplyToForm stamps the credential-on-file fields onto a classic Direct Post
// form. nil-safe, but money-moving callers validate first. The
// custodian-proxied transport composes the same form.
func (sc *StoredCredential) ApplyToForm(values url.Values) {
	if sc == nil {
		return
	}
	values.Set("initiated_by", sc.InitiatedBy)
	values.Set("stored_credential_indicator", sc.Indicator)
	if initialTransactionID := strings.TrimSpace(sc.InitialTransactionID); initialTransactionID != "" {
		values.Set("initial_transaction_id", initialTransactionID)
	}
	if sc.Recurring {
		values.Set("billing_method", "recurring")
	}
}
