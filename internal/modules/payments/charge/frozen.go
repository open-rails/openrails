package charge

import (
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/shared/normalize"
)

// FrozenInstrument is the saved method as its collection operation froze
// it at enqueue: the account the charge settles through and how the card is
// addressed there. A charge is submitted only while the method still matches
// it, and every receipt is judged against it, never the method's current row.
type FrozenInstrument struct {
	StoredCredentialRecurringRef   string     `json:"stored_credential_recurring_ref"`
	StoredCredentialUnscheduledRef string     `json:"stored_credential_unscheduled_ref"`
	PSPID                          uuid.UUID  `json:"psp_id"`
	Custodian                      string     `json:"custodian"`
	CustodianID                    *uuid.UUID `json:"custodian_id,omitempty"`
	RailCustomerRef                string     `json:"rail_customer_ref"`
	RailMethodRef                  string     `json:"rail_method_ref"`
}

// ChargeableOn reports whether a charge through psp can use the method: the
// PSP holding a PSP-held card, or any PSP for one a custodian holds.
func ChargeableOn(method gen.BillingPaymentMethod, psp uuid.UUID) bool {
	if method.Custodian != "" && method.Custodian != models.CustodianPSP {
		return true
	}
	return method.PspID != nil && *method.PspID == psp
}

// ErrInstrumentChanged: the payment method no longer matches the
// instrument its operation froze. Raised before any provider traffic.
var ErrInstrumentChanged = errors.New("payment method no longer matches the operation's frozen instrument")

// ErrNotDispatched proves this call refused before attempting a provider write.
// It says nothing about any earlier call or durable submission marker.
var ErrNotDispatched = errors.New("charge refused before provider dispatch")

// FreezeInstrument freezes a saved method's instrument for a charge through
// psp: the PSP that holds a PSP-held card, or the one routing picked for a
// card a third-party custodian holds.
func FreezeInstrument(method gen.BillingPaymentMethod, psp uuid.UUID) FrozenInstrument {
	return FrozenInstrument{
		PSPID: psp, Custodian: method.Custodian, CustodianID: method.CustodianID,
		StoredCredentialRecurringRef: normalize.FromPtr(method.StoredCredentialRecurringRef), StoredCredentialUnscheduledRef: normalize.FromPtr(method.StoredCredentialUnscheduledRef),
		RailCustomerRef: normalize.FromPtr(method.RailCustomerRef), RailMethodRef: normalize.FromPtr(method.RailMethodRef),
	}
}

// FreezeCustody freezes who holds a saved method and how it is addressed
// there, for an operation on the instrument itself (a delete) that no PSP
// takes.
func FreezeCustody(method gen.BillingPaymentMethod) FrozenInstrument {
	return FreezeInstrument(method, uuid.Nil)
}

// ValidateCustody checks a frozen instrument's custody alone.
func (i FrozenInstrument) ValidateCustody() error {
	if !slices.Contains(models.Custodians(), i.Custodian) || (i.Custodian == models.CustodianPSP) != (i.CustodianID == nil) {
		return fmt.Errorf("frozen instrument custody %q is incomplete", i.Custodian)
	}
	return nil
}

func (i FrozenInstrument) Validate() error {
	if i.PSPID == uuid.Nil {
		return errors.New("frozen instrument names no provider account")
	}
	if !slices.Contains(models.Custodians(), i.Custodian) || (i.Custodian == models.CustodianPSP) != (i.CustodianID == nil) {
		return fmt.Errorf("frozen instrument custody %q is incomplete", i.Custodian)
	}
	return nil
}

// custodianHeld: the frozen charge addressed the card through a custodian
// proxy (or#879), so the gateway holds no vault for it.
func (i FrozenInstrument) CustodianHeld() bool { return i.Custodian != models.CustodianPSP }

func (i FrozenInstrument) Matches(method gen.BillingPaymentMethod, agreement Agreement) error {
	cur := FreezeInstrument(method, i.PSPID)
	switch agreement {
	case AgreementRecurring:
		if i.StoredCredentialRecurringRef != cur.StoredCredentialRecurringRef {
			return fmt.Errorf("%w: recurring credential changed", ErrInstrumentChanged)
		}
	case AgreementUnscheduled:
		if i.StoredCredentialUnscheduledRef != cur.StoredCredentialUnscheduledRef {
			return fmt.Errorf("%w: unscheduled credential changed", ErrInstrumentChanged)
		}
	default:
		return fmt.Errorf("frozen instrument requires a scoped credential agreement")
	}
	sameCustodian := (cur.CustodianID == nil && i.CustodianID == nil) ||
		(cur.CustodianID != nil && i.CustodianID != nil && *cur.CustodianID == *i.CustodianID)
	heldElsewhere := method.Custodian == models.CustodianPSP && (method.PspID == nil || *method.PspID != i.PSPID)
	if heldElsewhere || cur.Custodian != i.Custodian || !sameCustodian ||
		cur.RailCustomerRef != i.RailCustomerRef || cur.RailMethodRef != i.RailMethodRef {
		return fmt.Errorf("%w: payment method %s", ErrInstrumentChanged, method.ID)
	}
	return nil
}
