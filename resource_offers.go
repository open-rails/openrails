package openrails

import (
	"context"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/billing"
)

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
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/catalog/offers/lookup", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return out, nil
}
