package charge

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"slices"
	"strings"
)

// FrozenInstrument is the saved method as its collection operation froze
// it at enqueue: the account the charge settles through and how the card is
// addressed there. A charge is submitted only while the method still matches
// it, and every receipt is judged against it, never the method's current row.
type FrozenInstrument struct {
	PSPID           uuid.UUID  `json:"psp_id"`
	Custodian       string     `json:"custodian"`
	CustodianID     *uuid.UUID `json:"custodian_id,omitempty"`
	RailCustomerRef string     `json:"rail_customer_ref"`
	RailMethodRef   string     `json:"rail_method_ref"`
}

// ErrInstrumentChanged: the payment method no longer matches the
// instrument its operation froze. Raised before any provider traffic.
var ErrInstrumentChanged = errors.New("payment method no longer matches the operation's frozen instrument")

// FreezeInstrument freezes a saved method's instrument.
func FreezeInstrument(method gen.OpenrailsPaymentMethod) FrozenInstrument {
	return FrozenInstrument{
		PSPID: method.PspID, Custodian: method.Custodian, CustodianID: method.CustodianID,
		RailCustomerRef: strings.TrimSpace(method.RailCustomerRef), RailMethodRef: strings.TrimSpace(method.RailMethodRef),
	}
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

func (i FrozenInstrument) Matches(method gen.OpenrailsPaymentMethod) error {
	cur := FreezeInstrument(method)
	sameCustodian := (cur.CustodianID == nil && i.CustodianID == nil) ||
		(cur.CustodianID != nil && i.CustodianID != nil && *cur.CustodianID == *i.CustodianID)
	if cur.PSPID != i.PSPID || cur.Custodian != i.Custodian || !sameCustodian ||
		cur.RailCustomerRef != i.RailCustomerRef || cur.RailMethodRef != i.RailMethodRef {
		return fmt.Errorf("%w: payment method %s", ErrInstrumentChanged, method.ID)
	}
	return nil
}
