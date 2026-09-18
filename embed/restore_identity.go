package embed

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrMerchantRestoreConflict indicates an existing destination has a different
// UUID, name or authority. Restore provisioning never changes existing identity.
var ErrMerchantRestoreConflict = merchants.ErrMerchantRestoreConflict

// RegisterMerchantForRestore explicitly registers a preserved merchant UUID for
// an AuthKit-free host, then binds this runtime to it. Call during startup before
// constructing Clients or starting workers. Ordinary manifest/config creation
// continues allocating its own merchant UUIDs. This registers no provider
// accounts or credentials; ImportMerchantBilling must still require an empty book.
func (r *Runtime) RegisterMerchantForRestore(ctx context.Context, id merchant.ID, slug string) (merchant.ID, error) {
	if r == nil || r.app == nil || r.app.Runtime == nil || r.app.Runtime.DB == nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: runtime database not initialized")
	}
	a := r.app
	if a.ControlPlane != nil {
		return merchant.ID{}, fmt.Errorf("openrails embed: attached control plane requires ProvisionMerchantForRestore with destination group authority")
	}
	if bound := a.Runtime.ConfiguredMerchant(); !bound.IsZero() && bound != id {
		return merchant.ID{}, ErrMerchantRestoreConflict
	}
	directory, err := merchants.NewDirectoryService(a.Runtime.DB.DataPool())
	if err != nil {
		return merchant.ID{}, err
	}
	m, _, err := directory.RegisterForRestore(ctx, id, slug)
	if err != nil {
		return merchant.ID{}, err
	}
	a.Runtime.SetConfiguredMerchant(m.ID)
	return m.ID, nil
}
