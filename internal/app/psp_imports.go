package app

import "github.com/open-rails/openrails/internal/pspimport"

// PSPImporter runs PSP imports over the runtime's current merchants, NMI
// clients and directory.
func (r *Runtime) PSPImporter() *pspimport.Importer {
	if r == nil || r.DB == nil {
		return nil
	}
	return &pspimport.Importer{DB: r.DB, Merchants: r.Merchants, NMI: r.NMIClients, Directory: r.Contacts, Clock: r.Clock}
}
