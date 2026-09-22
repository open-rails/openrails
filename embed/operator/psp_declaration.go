package operator

import (
	"context"
	"github.com/google/uuid"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/pkg/merchant"
)

// DeclarePSP supplies attribution for imported facts after a local control plane
// has provisioned the merchant. Call during setup before serving requests or
// starting workers. It does not configure credentials, rename existing provider
// aliases, reactivate archived accounts, or arm providers for checkout.
// Ordinary payment-provider configuration uses the merchant's configuration API.
func (r *Operator) DeclarePSP(ctx context.Context, merchantID merchant.ID, declaration embed.PSPDeclaration) (uuid.UUID, error) {
	if err := r.initialized(); err != nil {
		return uuid.Nil, err
	}
	return hosttools.DeclarePSP(ctx, r.app, merchantID, declaration)
}
