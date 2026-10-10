package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
)

// CustodianProxyCollectionAdapter collects invoices from custodian-held
// instruments: a merchant-initiated unscheduled CoF charge, detokenized through
// the custodian's proxy into the PSP's own NMI gateway. It is the NMI rail,
// with the NMI adapter's anchor semantics.
type CustodianProxyCollectionAdapter struct {
	Charger *nmiproxy.Charger
}

func NewCustodianProxyCollectionAdapter(charger *nmiproxy.Charger) *CustodianProxyCollectionAdapter {
	return &CustodianProxyCollectionAdapter{Charger: charger}
}

func (a *CustodianProxyCollectionAdapter) Prepare(_ context.Context, method gen.BillingPaymentMethod, req ChargeRequest) (PreparedCharge, error) {
	if a == nil || a.Charger == nil {
		return nil, fmt.Errorf("custodian-proxy collection adapter not initialized")
	}
	if strings.TrimSpace(models.DerefStr(method.RailMethodRef)) == "" {
		return nil, fmt.Errorf("custodian-held payment method missing its custodian token reference")
	}
	if !paymentmethods.Chargeable(method) {
		return nil, fmt.Errorf("custodian-held instrument cannot be charged")
	}
	charger := a.Charger.WithSource(nmiproxy.Source{
		TokenID: strings.TrimSpace(models.DerefStr(method.RailMethodRef)), Via: method.ChargeVia,
		NetworkTokenID: strings.TrimSpace(models.DerefStr(method.NetworkTokenID)),
	})
	return prepareUnscheduledCollection(method, req, charger)
}
