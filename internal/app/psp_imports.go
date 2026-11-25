package app

import (
	"time"

	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/catalog/providerobjects"
	"github.com/open-rails/openrails/internal/proposals"
	"github.com/open-rails/openrails/internal/pspimport"
)

// ProviderObjects observes the merchants' PSP catalogs with the runtime's
// current merchants and clients.
func (r *Runtime) ProviderObjects() *providerobjects.Observer {
	if r == nil || r.DB == nil {
		return nil
	}
	return &providerobjects.Observer{DB: r.DB, Merchants: r.Merchants, NMI: r.NMIClients, Stripe: r.StripeClients, Clock: r.Clock, Log: r.Log}
}

// PSPImporter runs PSP imports over the runtime's current merchants, NMI
// clients and directory.
func (r *Runtime) PSPImporter() *pspimport.Importer {
	if r == nil || r.DB == nil {
		return nil
	}
	return &pspimport.Importer{DB: r.DB, Merchants: r.Merchants, NMI: r.NMIClients, Stripe: r.StripeClients, Objects: r.ProviderObjects(),
		CCBillDataLinkURL: r.ProviderEndpoints.CCBillDataLinkURL, DataLinkPacing: r.DataLinkPacing(), Directory: r.Contacts,
		Proposals: proposals.New(r.DB, r.Config, r.Clock, r.DestructiveAdmissions), Clock: r.Clock, Log: r.Log}
}

// DataLinkPacing keeps live CCBill accounts' DataLink slots in the abuse
// state, which every instance shares through Redis, on the runtime's clock.
// A slot lost with Redis costs one request CCBill refuses, which waits like
// any other.
func (r *Runtime) DataLinkPacing() ccbill.Pacing {
	var pacing ccbill.Pacing
	if r == nil {
		return pacing
	}
	if r.AbuseState != nil {
		pacing.Slots = r.AbuseState
	}
	if r.Clock != nil {
		pacing.Now = r.Clock.Now
	} else {
		pacing.Now = time.Now
	}
	return pacing
}
