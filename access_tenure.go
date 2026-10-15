package openrails

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// GetAccessTenure returns the customer's elapsed access to the requested
// entitlement keys, including past product access and retired keys. At must
// not be in the future; zero uses OpenRails' clock.
func (c *Client) GetAccessTenure(ctx context.Context, params billing.AccessTenureParams, options ...RequestOption) (*billing.AccessTenure, error) {
	path, err := customerIDPath(params.CustomerID)
	if err != nil {
		return nil, err
	}
	if len(params.Entitlements) == 0 || len(params.Entitlements) > billing.MaxEntitlementChecks {
		return nil, invalidErr(fmt.Sprintf("entitlements must name 1 to %d keys", billing.MaxEntitlementChecks))
	}
	keys, err := catalog.NormalizeEntitlements(params.Entitlements)
	if err != nil {
		return nil, invalidErr(err.Error())
	}
	query := url.Values{"entitlement": keys}
	if !params.At.IsZero() {
		query.Set("at", params.At.UTC().Format(time.RFC3339Nano))
	}
	var out billing.AccessTenure
	if err := c.do(ctx, http.MethodGet, withQuery(path+"/access-tenure", query), nil, &out, options...); err != nil {
		return nil, err
	}
	return &out, nil
}
