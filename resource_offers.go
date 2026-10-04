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

// ListOffersForEntitlements returns one bounded page of active offers per
// opaque resource key (at most 100 keys) in one request. Every requested key
// is present in the result. PreferredCurrency ranks matching offers first;
// alternatives retain their actual native currency and amount.
func (c *Client) ListOffersForEntitlements(ctx context.Context, entitlements []string, params billing.OfferListParams, requestOptions ...RequestOption) (map[string]billing.OfferList, error) {
	if len(entitlements) > billing.MaxEntitlementChecks {
		return nil, invalidErr("at most 100 entitlements are allowed")
	}
	for _, key := range entitlements {
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
	if len(entitlements) == 0 {
		return map[string]billing.OfferList{}, nil
	}
	var result map[string]billing.OfferList
	err := c.do(ctx, http.MethodPost, c.catalogPath()+"/offers/lookup", billing.OfferLookupRequest{Entitlements: entitlements, Kind: params.Kind, PreferredCurrency: params.PreferredCurrency, PageSize: params.Limit, Cursors: params.Cursors}, &result, requestOptions...)
	return result, err
}
