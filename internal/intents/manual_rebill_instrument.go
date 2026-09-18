package intents

import (
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"strings"
)

// RebillInstrument is the saved method a rebill was frozen on, in the shape
// invoice collection freezes (money.CollectionInstrument): a rebill is sent
// on THIS instrument and judged against it, never against whatever the
// method row says later. A #297 custody flip keeps the provider account and
// the old vault reference while moving custody, rail_method_ref and
// charge_via, so custody is part of the identity, not a detail.
type RebillInstrument struct {
	PSPID           uuid.UUID  `json:"psp_id"`
	Custodian       string     `json:"custodian"`
	CustodianID     *uuid.UUID `json:"custodian_id,omitempty"`
	RailCustomerRef string     `json:"rail_customer_ref"`
	RailMethodRef   string     `json:"rail_method_ref"`
}

// RebillInstrumentOf freezes a saved method's instrument.
func RebillInstrumentOf(pm *models.PaymentMethod) RebillInstrument {
	if pm == nil {
		return RebillInstrument{}
	}
	return RebillInstrument{
		PSPID: pm.PspID, Custodian: pm.Custodian, CustodianID: pm.CustodianID,
		RailCustomerRef: strings.TrimSpace(pm.RailCustomerRef), RailMethodRef: strings.TrimSpace(pm.RailMethodRef),
	}
}

// Validate refuses an instrument a rebill cannot be sent on. A rebill is an
// NMI vault charge (customer vault + billing id); a custodian-held card is
// charged by card data through its proxy, which this path does not speak, so
// it is refused here rather than silently sent on a stale vault.
func (i RebillInstrument) Validate() error {
	switch {
	case i.PSPID == uuid.Nil:
		return errors.New("frozen instrument names no provider account")
	case i.Custodian != models.CustodianPSP || i.CustodianID != nil:
		return fmt.Errorf("frozen instrument is held by %q; a rebill charges the provider's own vault", i.Custodian)
	case i.RailCustomerRef == "" || i.RailMethodRef == "":
		return errors.New("frozen instrument has no customer vault and billing reference")
	}
	return nil
}

// Matches reports whether the method still is the frozen instrument.
func (i RebillInstrument) Matches(method gen.OpenrailsPaymentMethod) error {
	cur := RebillInstrument{
		PSPID: method.PspID, Custodian: method.Custodian, CustodianID: method.CustodianID,
		RailCustomerRef: strings.TrimSpace(method.RailCustomerRef), RailMethodRef: strings.TrimSpace(method.RailMethodRef),
	}
	sameCustodian := (cur.CustodianID == nil && i.CustodianID == nil) ||
		(cur.CustodianID != nil && i.CustodianID != nil && *cur.CustodianID == *i.CustodianID)
	if cur.PSPID != i.PSPID || cur.Custodian != i.Custodian || !sameCustodian ||
		cur.RailCustomerRef != i.RailCustomerRef || cur.RailMethodRef != i.RailMethodRef {
		return fmt.Errorf("payment method %s no longer matches the rebill's frozen instrument", method.ID)
	}
	return nil
}
