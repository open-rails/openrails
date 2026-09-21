package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
)

// CustodianProxyCollectionAdapter collects invoices from custodian-held
// instruments (#795) through the #297 charge seam: a merchant-initiated
// unscheduled CoF charge, detokenized through the custodian's proxy into the
// PSP's own NMI gateway. Same anchor semantics as the NMI adapter — it IS the
// NMI rail (or#879), reached differently.
type CustodianProxyCollectionAdapter struct {
	Charger *nmiproxy.Charger
}

func NewCustodianProxyCollectionAdapter(charger *nmiproxy.Charger) *CustodianProxyCollectionAdapter {
	return &CustodianProxyCollectionAdapter{Charger: charger}
}

func (a *CustodianProxyCollectionAdapter) Prepare(_ context.Context, method gen.OpenrailsPaymentMethod, req ChargeRequest) (PreparedCharge, error) {
	if a == nil || a.Charger == nil {
		return nil, fmt.Errorf("custodian-proxy collection adapter not initialized")
	}
	if strings.TrimSpace(method.RailMethodRef) == "" {
		return nil, fmt.Errorf("custodian-held payment method missing its custodian token reference")
	}
	if strings.TrimSpace(method.ParkReason) != "" {
		return nil, fmt.Errorf("custodian-held instrument is parked")
	}
	charger := a.Charger.WithSource(nmiproxy.Source{
		TokenID: strings.TrimSpace(method.RailMethodRef), Via: method.ChargeVia,
		NetworkTokenID: strings.TrimSpace(method.NetworkTokenID),
	})
	return prepareUnscheduledCollection(method, req, charger)
}
