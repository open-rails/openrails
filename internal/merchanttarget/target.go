// Package merchanttarget resolves a requested billing book before authorization.
// It never grants authority or accepts an ambient host context as a selector.
package merchanttarget

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/billingauth"
	"github.com/open-rails/openrails/pkg/merchant"
)

type Directory interface {
	Get(context.Context, merchant.ID) (*merchants.Merchant, error)
	GetBySlug(context.Context, string) (*merchants.Merchant, error)
}

type contextKey struct{}
type resolvedSelection struct {
	Target billingauth.Target
	slug   string
}

func WithResolved(ctx context.Context, target billingauth.Target) context.Context {
	if captured, ok := ctx.Value(contextKey{}).(resolvedSelection); ok && captured.Target == target {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, resolvedSelection{Target: target})
}
func FromContext(ctx context.Context) (billingauth.Target, bool) {
	selection, ok := ctx.Value(contextKey{}).(resolvedSelection)
	return selection.Target, ok
}

func Resolve(ctx context.Context, r *http.Request, directory Directory, bound merchant.ID, defaultSlug string) (billingauth.Target, error) {
	if target, ok := FromContext(ctx); ok {
		if err := Assert(r, target); err != nil {
			return billingauth.Target{}, err
		}
		if !bound.IsZero() && bound != target.MerchantID {
			return billingauth.Target{}, billingauth.GateError{Status: 409, Message: "configured merchant binding mismatch"}
		}
		if defaultSlug != "" && merchant.NormalizeSlug(defaultSlug) != target.MerchantSlug {
			return billingauth.Target{}, billingauth.GateError{Status: 409, Message: "configured merchant slug mismatch"}
		}
		return target, nil
	}
	var id merchant.ID
	var slug string
	if r != nil {
		rawID := strings.TrimSpace(r.Header.Get(merchant.BindingHeader))
		if values, present := r.Header[http.CanonicalHeaderKey(merchant.BindingHeader)]; present && (len(values) != 1 || rawID == "") {
			return billingauth.Target{}, billingauth.GateError{Status: 400, Message: "invalid merchant ID selector"}
		}
		slug = strings.TrimSpace(r.Header.Get(merchant.SlugHeader))
		if values, present := r.Header[http.CanonicalHeaderKey(merchant.SlugHeader)]; present && (len(values) != 1 || slug == "") {
			return billingauth.Target{}, billingauth.GateError{Status: 400, Message: "invalid merchant slug selector"}
		}
		if rawID != "" && slug != "" {
			return billingauth.Target{}, billingauth.GateError{Status: 400, Message: "choose one merchant selector"}
		}
		if rawID != "" {
			var err error
			id, err = merchant.ParseID(rawID)
			if err != nil || id.IsZero() {
				return billingauth.Target{}, billingauth.GateError{Status: 400, Message: "invalid merchant ID"}
			}
		}
	}
	if slug == "" && id.IsZero() {
		slug = strings.TrimSpace(defaultSlug)
		if slug == "" {
			id = bound
		}
	}
	if slug == "" && id.IsZero() {
		return billingauth.Target{}, billingauth.GateError{Status: 400, Message: "merchant selector is required"}
	}
	if directory == nil {
		return billingauth.Target{}, billingauth.GateError{Status: 503, Message: "merchant directory unavailable"}
	}
	var selected *merchants.Merchant
	var err error
	if slug != "" {
		if err := merchant.ValidateSlug(slug); err != nil {
			return billingauth.Target{}, billingauth.GateError{Status: 400, Message: "invalid merchant slug"}
		}
		selected, err = directory.GetBySlug(ctx, slug)
	} else {
		selected, err = directory.Get(ctx, id)
	}
	if err != nil && !errors.Is(err, merchants.ErrMerchantNotFound) {
		return billingauth.Target{}, billingauth.GateError{Status: 503, Message: "merchant directory unavailable"}
	}
	if err != nil || selected == nil || selected.Status != merchants.StatusActive {
		return billingauth.Target{}, billingauth.GateError{Status: 404, Message: "merchant not found"}
	}
	if !bound.IsZero() && selected.ID != bound {
		return billingauth.Target{}, billingauth.GateError{Status: 409, Message: "configured merchant binding mismatch"}
	}
	target := billingauth.Target{MerchantID: selected.ID, MerchantSlug: selected.Slug, AuthorityGroupID: selected.PermissionGroupID}
	if r != nil {
		// Preserve the actually resolved name separately from the canonical name.
		// Active aliases may differ; changing the header later cannot widen it.
		*r = *r.WithContext(context.WithValue(r.Context(), contextKey{}, resolvedSelection{Target: target, slug: merchant.NormalizeSlug(slug)}))
	}
	return target, nil
}

// Assert compares untrusted optional selectors to a previously resolved target.
func Assert(r *http.Request, target billingauth.Target) error {
	if r == nil {
		return nil
	}
	rawID := strings.TrimSpace(r.Header.Get(merchant.BindingHeader))
	rawSlug := strings.TrimSpace(r.Header.Get(merchant.SlugHeader))
	if rawID != "" && rawSlug != "" {
		return billingauth.GateError{Status: 400, Message: "choose one merchant selector"}
	}
	if rawID != "" {
		id, err := merchant.ParseID(rawID)
		if err != nil {
			return billingauth.GateError{Status: 400, Message: "invalid merchant ID"}
		}
		if id != target.MerchantID {
			return billingauth.GateError{Status: 409, Message: "merchant binding mismatch"}
		}
	}
	expectedSlug := target.MerchantSlug
	if captured, ok := r.Context().Value(contextKey{}).(resolvedSelection); ok && captured.Target == target && captured.slug != "" {
		expectedSlug = captured.slug
	}
	if rawSlug != "" && merchant.NormalizeSlug(rawSlug) != expectedSlug {
		return billingauth.GateError{Status: 409, Message: "merchant binding mismatch"}
	}
	return nil
}
