package server

import (
	"errors"
	"net/http"

	auth "github.com/open-rails/helpers/auth"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/merchanttarget"
)

// resolveMerchant is the merchant a request acts on, from what names one:
// its OpenRails-Merchant selector, the scope its credential is bound to
// (helpers/auth Bound: an API key's or a trusted issuer's group, whose id is
// the merchant's) and the deployment's configured merchant. They must all
// agree; none, or a merchant that is not live with its AuthKit group, is
// merchant_unresolved.
func (s *Server) resolveMerchant(r *http.Request, v billingauth.Verified) (billingauth.Target, error) {
	ctx := r.Context()
	var target billingauth.Target
	agree := func(t billingauth.Target) error {
		switch {
		case t.MerchantID.IsZero():
			return nil
		case target.MerchantID.IsZero():
			target = t
		case target.MerchantID != t.MerchantID:
			return billingauth.Refusal(billing.CodeMerchantBindingMismatch)
		case target.MerchantSlug == "":
			target.MerchantSlug = t.MerchantSlug
		}
		return nil
	}
	if selected, ok := merchanttarget.FromContext(ctx); ok {
		if err := agree(selected); err != nil {
			return billingauth.Target{}, err
		}
	}
	if b, ok := v.(auth.Bound); ok && b.BoundScope() != (billingauth.Scope{}) {
		m, isMerchant, err := s.controlPlane.MerchantOfScope(ctx, b.BoundScope())
		switch {
		case err != nil:
			return billingauth.Target{}, errors.Join(billingauth.Refusal(billing.CodeAuthorizationUnavailable), err)
		case !isMerchant:
			return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantBindingMismatch)
		}
		if err := agree(billingauth.Target{MerchantID: m.ID, MerchantSlug: m.Slug}); err != nil {
			return billingauth.Target{}, err
		}
	}
	if err := agree(billingauth.Target{MerchantID: s.runtime.ConfiguredMerchant()}); err != nil {
		return billingauth.Target{}, err
	}
	if target.MerchantID.IsZero() {
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	}
	switch _, err := s.controlPlane.MerchantScope(ctx, target.MerchantID); {
	case errors.Is(err, billing.ErrMerchantUnresolved):
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantUnresolved)
	case err != nil:
		return billingauth.Target{}, errors.Join(billingauth.Refusal(billing.CodeAuthorizationUnavailable), err)
	}
	return target, nil
}
