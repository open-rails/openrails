// Package merchanttarget resolves a requested billing book before authorization.
// It never grants authority or accepts an ambient host context as a selector.
package merchanttarget

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
)

type Directory interface {
	Get(context.Context, billing.MerchantID) (*merchants.Merchant, error)
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

func Resolve(ctx context.Context, r *http.Request, directory Directory, bound billing.MerchantID, defaultSlug string) (billingauth.Target, error) {
	if target, ok := FromContext(ctx); ok {
		if err := Assert(r, target); err != nil {
			return billingauth.Target{}, err
		}
		if !bound.IsZero() && bound != target.MerchantID {
			return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantBindingMismatch, "configured merchant binding mismatch")
		}
		if defaultSlug != "" && billing.NormalizeMerchantSlug(defaultSlug) != target.MerchantSlug {
			return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantBindingMismatch, "configured merchant slug mismatch")
		}
		return target, nil
	}
	var id billing.MerchantID
	var slug string
	if r != nil {
		rawID := strings.TrimSpace(r.Header.Get(merchant.BindingHeader))
		if values, present := r.Header[http.CanonicalHeaderKey(merchant.BindingHeader)]; present && (len(values) != 1 || rawID == "") {
			return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantSelectorInvalid, "invalid merchant ID selector")
		}
		slug = strings.TrimSpace(r.Header.Get(merchant.SlugHeader))
		if values, present := r.Header[http.CanonicalHeaderKey(merchant.SlugHeader)]; present && (len(values) != 1 || slug == "") {
			return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantSelectorInvalid, "invalid merchant slug selector")
		}
		if rawID != "" && slug != "" {
			return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantSelectorInvalid, "choose one merchant selector")
		}
		if rawID != "" {
			var err error
			id, err = billing.ParseMerchantID(rawID)
			if err != nil || id.IsZero() {
				return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantSelectorInvalid, "invalid merchant ID")
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
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantSelectorInvalid, "merchant selector is required")
	}
	if !bound.IsZero() && !id.IsZero() && id != bound {
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantBindingMismatch, fmt.Sprintf("configured merchant binding mismatch: requested %s, configured %s", id, bound))
	}
	if directory == nil {
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantDirectoryUnavailable, "merchant directory unavailable")
	}
	var selected *merchants.Merchant
	var err error
	if slug != "" {
		if err := billing.ValidateMerchantSlug(slug); err != nil {
			return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantSelectorInvalid, "invalid merchant slug")
		}
		selected, err = directory.GetBySlug(ctx, slug)
	} else {
		selected, err = directory.Get(ctx, id)
	}
	if err != nil && !errors.Is(err, merchants.ErrMerchantNotFound) {
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantDirectoryUnavailable, "merchant directory unavailable")
	}
	if err != nil || selected == nil || selected.Status != merchants.StatusActive {
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantNotFound, "merchant not found")
	}
	if !bound.IsZero() && selected.ID != bound {
		return billingauth.Target{}, billingauth.Refusal(billing.CodeMerchantBindingMismatch, "configured merchant binding mismatch")
	}
	target := billingauth.Target{MerchantID: selected.ID, MerchantSlug: selected.Slug, AuthorityGroupID: selected.PermissionGroupID}
	if r != nil {
		// Preserve the actually resolved name separately from the canonical name.
		// Active aliases may differ; changing the header later cannot widen it.
		*r = *r.WithContext(context.WithValue(r.Context(), contextKey{}, resolvedSelection{Target: target, slug: billing.NormalizeMerchantSlug(slug)}))
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
		return billingauth.Refusal(billing.CodeMerchantSelectorInvalid, "choose one merchant selector")
	}
	if rawID != "" {
		id, err := billing.ParseMerchantID(rawID)
		if err != nil {
			return billingauth.Refusal(billing.CodeMerchantSelectorInvalid, "invalid merchant ID")
		}
		if id != target.MerchantID {
			return billingauth.Refusal(billing.CodeMerchantBindingMismatch, "merchant binding mismatch")
		}
	}
	expectedSlug := target.MerchantSlug
	if captured, ok := r.Context().Value(contextKey{}).(resolvedSelection); ok && captured.Target == target && captured.slug != "" {
		expectedSlug = captured.slug
	}
	if rawSlug != "" && billing.NormalizeMerchantSlug(rawSlug) != expectedSlug {
		return billingauth.Refusal(billing.CodeMerchantBindingMismatch, "merchant binding mismatch")
	}
	return nil
}
