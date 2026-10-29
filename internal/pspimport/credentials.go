package pspimport

import (
	"errors"

	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/reconcile"
)

// ErrCredentialsRejected is the provider refusing the PSP's credentials: an
// NMI v5 401/403 or Query API authentication error, a Stripe 401/403, a
// DataLink 401/403 or authentication failure.
var ErrCredentialsRejected = errors.New("the provider rejected the PSP's credentials")

// rejected reports whether err is a provider refusing the credentials.
func rejected(err error) bool {
	var stripe *reconcile.StripeAPIError
	return errors.Is(err, ErrCredentialsRejected) || errors.Is(err, nmi.ErrCredentialsRejected) || errors.Is(err, ccbill.ErrDataLinkAuth) ||
		errors.As(err, &stripe) && (stripe.Status == 401 || stripe.Status == 403)
}
