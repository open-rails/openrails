package nmi

import (
	"errors"
	"net/url"
)

// StoredCredential carries NMI's credential-on-file (CIT/MIT) wire fields,
// verified verbatim against the NMI integration portal ("Credential on File
// Information", 2026-07-06):
//
//	initiated_by                customer | merchant
//	stored_credential_indicator stored | used        (SINGULAR "credential")
//	initial_transaction_id      the gateway transactionid of the sequence's
//	                            initial CIT — NMI's own id, which the gateway
//	                            maps to the network transaction identifier
//	                            internally; never a raw network NTID
//	billing_method              recurring — sent on recurring-agreement charges
//	                            only; unscheduled CoF sends NO billing_method
//
// The portal's canonical combinations:
//
//	recurring initial CIT:  billing_method=recurring initiated_by=customer stored_credential_indicator=stored
//	recurring MIT:          billing_method=recurring initiated_by=merchant stored_credential_indicator=used initial_transaction_id=…
//	unscheduled initial CIT:                         initiated_by=customer stored_credential_indicator=stored
//	unscheduled CIT reuse:                           initiated_by=customer stored_credential_indicator=used
//	unscheduled MIT:                                 initiated_by=merchant stored_credential_indicator=used initial_transaction_id=…
//
// References do NOT cross agreement types ("This transaction ID cannot be
// used for 'unscheduled' … credential-on-file transactions").
type StoredCredential struct {
	InitiatedBy          string // "customer" | "merchant"
	Indicator            string // "stored" | "used"
	InitialTransactionID string // "" on initial transactions and legacy reference-less charges
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

// Validate rejects malformed CIT/MIT field combinations. nil is valid (no
// stored-credential data). Exported for sibling rails composing the same form.
func (sc *StoredCredential) Validate() error {
	if sc == nil {
		return nil
	}
	if sc.InitiatedBy != InitiatedByCustomer && sc.InitiatedBy != InitiatedByMerchant {
		return errors.New("stored credential initiated_by must be customer or merchant")
	}
	if sc.Indicator != IndicatorStored && sc.Indicator != IndicatorUsed {
		return errors.New("stored credential indicator must be stored or used")
	}
	return nil
}

// ApplyToForm stamps the credential-on-file fields onto a classic Direct Post
// form. nil = caller sends no stored-credential data (legacy shape). Exported
// for the custodian-proxied transport (#795), which composes the same classic sale form
// for BT-proxy delivery.
func (sc *StoredCredential) ApplyToForm(values url.Values) {
	if sc == nil {
		return
	}
	values.Set("initiated_by", sc.InitiatedBy)
	values.Set("stored_credential_indicator", sc.Indicator)
	if sc.InitialTransactionID != "" {
		values.Set("initial_transaction_id", sc.InitialTransactionID)
	}
	if sc.Recurring {
		values.Set("billing_method", "recurring")
	}
}
