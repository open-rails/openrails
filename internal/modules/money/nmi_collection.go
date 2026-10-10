package money

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmidirect"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// NMICollectionAdapter collects invoices from NMI customer vault payment
// methods: every collection is a merchant-initiated unscheduled
// credential-on-file charge carrying the instrument's stored-credential replay
// reference.
type NMICollectionAdapter struct {
	Charger *nmidirect.Charger
}

func NewNMICollectionAdapter(client *nmi.NMIClient) *NMICollectionAdapter {
	return &NMICollectionAdapter{Charger: nmidirect.New(client)}
}

func (a *NMICollectionAdapter) Prepare(_ context.Context, method gen.BillingPaymentMethod, req ChargeRequest) (PreparedCharge, error) {
	// No default: a guessed currency would mint a real charge in a currency
	// nobody established.
	currency := normalizeCurrency(req.Currency)
	if err := moneyutil.RequireFiatCurrency(currency); err != nil {
		return nil, fmt.Errorf("nmi collection: refusing to charge without an established currency: %w", err)
	}
	if a == nil || a.Charger == nil || a.Charger.Client == nil {
		return nil, fmt.Errorf("nmi collection adapter not initialized")
	}
	if a.Charger.Client.ReadOnly {
		return nil, fmt.Errorf("nmi client is read-only (mode=readonly)")
	}
	if strings.TrimSpace(models.DerefStr(method.RailCustomerRef)) == "" {
		return nil, fmt.Errorf("nmi payment method missing customer vault id")
	}
	return prepareUnscheduledCollection(method, req, a.Charger)
}
