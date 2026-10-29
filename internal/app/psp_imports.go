package app

import (
	"github.com/open-rails/openrails/internal/config"
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
	return &providerobjects.Observer{DB: r.DB, Merchants: r.Merchants, NMI: r.NMIClients, Stripe: r.StripeClients, Clock: r.Clock}
}

// PSPImporter runs PSP imports over the runtime's current merchants, NMI
// clients and directory.
func (r *Runtime) PSPImporter() *pspimport.Importer {
	if r == nil || r.DB == nil {
		return nil
	}
	return &pspimport.Importer{DB: r.DB, Merchants: r.Merchants, NMI: r.NMIClients, Stripe: r.StripeClients, Objects: r.ProviderObjects(),
		CCBillDataLinkURL: config.SandboxCCBillDataLinkURL(r.Config), Directory: r.Contacts, Proposals: proposals.New(r.DB, r.Config, r.Clock), Clock: r.Clock}
}
