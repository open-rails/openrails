package nmi

import (
	"errors"
	"net/url"
	"strings"
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
	InitialTransactionID string // "" on the initial CIT or a legacy best-effort MIT fallback
	// AllowUnanchoredMIT must be set explicitly by a legacy recovery caller.
	// It never makes the request network-compliant; it only keeps historical
	// subscriptions chargeable when the original reference is unrecoverable.
	AllowUnanchoredMIT bool
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

// Validate rejects absent or malformed CIT/MIT field combinations. Every NMI
// charge in this package uses a stored credential, so nil is never a valid
// money-moving request. Exported for sibling rails composing the same form.
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
		if initialTransactionID == "" && !(sc.InitiatedBy == InitiatedByMerchant && sc.AllowUnanchoredMIT) {
			return errors.New("subsequent stored credential transaction requires initial_transaction_id")
		}
	}
	return nil
}

// IsUnanchoredMIT reports the legacy recovery posture where OpenRails still
// sends the required merchant/used indicators but cannot replay the original
// NMI transaction ID. NMI requires that ID for a fully compliant MIT; this
// exceptional path exists only to avoid stranding historical subscriptions
// whose anchor was never captured.
func (sc *StoredCredential) IsUnanchoredMIT() bool {
	return sc != nil &&
		sc.InitiatedBy == InitiatedByMerchant &&
		sc.Indicator == IndicatorUsed &&
		sc.AllowUnanchoredMIT &&
		strings.TrimSpace(sc.InitialTransactionID) == ""
}

// ApplyToForm stamps the credential-on-file fields onto a classic Direct Post
// form. It remains nil-safe as a form helper, but every money-moving caller
// validates the value first. Exported for the custodian-proxied transport
// (#795), which composes the same classic sale form for BT-proxy delivery.
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
