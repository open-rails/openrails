package openrails

import (
	"context"
	"errors"
	"fmt"

	"github.com/open-rails/authkit/auth"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
)

// ResolveCustomer reads the existing customer proved by the original verified
// request. Authority is the same trusted provider's metadata, never client input.
// It requires auth.CredentialChecker, performs no authentication or admission,
// and grants no platform link or permission beyond returning the proven pair.
func (e *Engine) ResolveCustomer(ctx context.Context, authority auth.Authenticator, verified auth.Verified) (CustomerProof, error) {
	if e == nil || e.engine == nil {
		return CustomerProof{}, fmt.Errorf("%w: an initialized Engine is required", billing.ErrUnreachable)
	}
	binding, err := e.engine.ResolveCustomer(ctx, authority, verified)
	var refusal billingauth.GateError
	if errors.As(err, &refusal) {
		info, _ := billing.LookupErrorCode(refusal.Code)
		err = errors.Join(&billing.StatusError{Status: refusal.Status, ErrorDetails: billing.ErrorDetails{Code: refusal.Code, Type: info.Type, Message: refusal.Message, Metadata: refusal.Metadata}}, refusal.Err)
	}
	return binding, err
}
