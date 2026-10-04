package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
)

// CheckEntitlements checks a bounded set of opaque resource/feature keys using
// retained access grants. Catalog edits and archival do not rewrite purchases.
func (c *Client) CheckEntitlements(ctx context.Context, customerID string, entitlements []string, at time.Time, requestOptions ...RequestOption) (map[string]bool, error) {
	customer, err := requireCustomerID(customerID)
	if err != nil {
		return nil, err
	}
	if len(entitlements) > billing.MaxEntitlementChecks {
		return nil, invalidErr("at most 100 entitlements are allowed")
	}
	for _, key := range entitlements {
		if strings.TrimSpace(key) == "" || len(key) > 256 {
			return nil, invalidErr("entitlement must be a nonempty key of at most 256 bytes")
		}
	}
	if len(entitlements) == 0 {
		return map[string]bool{}, nil
	}
	var result map[string]bool
	err = c.do(ctx, http.MethodPost, "/v1/merchant/customers/"+url.PathEscape(customer)+"/entitlements/check", struct {
		Entitlements []string  `json:"entitlements"`
		At           time.Time `json:"at,omitzero"`
	}{entitlements, at}, &result, requestOptions...)
	return result, err
}

// ListOffers returns one page of live offers for each requested entitlement
// (at most 100) in one request; every requested entitlement is in the result.
// PreferredCurrency ranks matching offers first; alternatives keep their own
// currency and amount.
func (c *Client) ListOffers(ctx context.Context, params billing.OfferListParams, requestOptions ...RequestOption) (billing.OfferPages, error) {
	if len(params.Entitlements) > billing.MaxEntitlementChecks {
		return nil, invalidErr("at most 100 entitlements are allowed")
	}
	for _, key := range params.Entitlements {
		if strings.TrimSpace(key) == "" || len(key) > 256 {
			return nil, invalidErr("entitlement must be a nonempty key of at most 256 bytes")
		}
	}
	if params.Kind != billing.OfferPermanent && params.Kind != billing.OfferFinite && params.Kind != billing.OfferRecurring {
		return nil, invalidErr("kind must be permanent, finite or recurring")
	}
	if params.Limit < 0 || params.Limit > 100 {
		return nil, invalidErr("limit must be between 1 and 100")
	}
	var out billing.OfferPages
	if err := c.do(ctx, http.MethodPost, c.catalogPath()+"/offers/lookup", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}
