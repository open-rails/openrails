package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const MaxEntitlementChecks = 100

// CheckEntitlements checks a bounded set of opaque resource/feature keys using
// retained access grants. Catalog edits and archival do not rewrite purchases.
func (c *Client) CheckEntitlements(ctx context.Context, customerID string, entitlements []string, at time.Time, requestOptions ...RequestOption) (map[string]bool, error) {
	customer, err := requireCustomerID(customerID)
	if err != nil {
		return nil, err
	}
	if len(entitlements) > MaxEntitlementChecks {
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
	err = c.do(ctx, http.MethodPost, "/v1/merchant/users/"+url.PathEscape(customer)+"/entitlements/check", struct {
		Entitlements []string  `json:"entitlements"`
		At           time.Time `json:"at,omitzero"`
	}{entitlements, at}, &result, requestOptions...)
	return result, err
}

// OfferKind distinguishes commercial access terms; currency preference never
// substitutes a subscription or rental for permanent ownership.
type OfferKind string

const (
	OfferPermanent OfferKind = "permanent"
	OfferFinite    OfferKind = "finite"
	OfferRecurring OfferKind = "recurring"
)

type OfferListParams struct {
	Kind              OfferKind
	PreferredCurrency string
	Limit             int
	Cursor            string
}

// CatalogOffer is an active price and the product benefits it buys. The price
// ID is immutable; its key selects the current version when starting checkout.
type CatalogOffer struct {
	Kind                OfferKind       `json:"kind"`
	ProductID           string          `json:"product_id"`
	ProductKey          string          `json:"product_key"`
	ProductName         string          `json:"product_name"`
	PriceID             string          `json:"price_id"`
	PriceKey            string          `json:"price_key,omitempty"`
	UnitAmount          int64           `json:"unit_amount,string"`
	Currency            string          `json:"currency"`
	AccessDurationHours *int            `json:"access_duration_hours,omitempty"`
	AutoRenew           bool            `json:"auto_renew"`
	EntitlementsSpec    map[string]*int `json:"entitlements_spec"`
}

type OfferList struct {
	Data       []CatalogOffer `json:"data"`
	HasMore    bool           `json:"has_more"`
	NextCursor string         `json:"next_cursor,omitempty"`
}

// ListOffersForEntitlement returns one bounded page of active offers granting
// exactly this opaque resource key. PreferredCurrency ranks matching offers
// first; alternatives retain their actual native currency and amount.
func (c *Client) ListOffersForEntitlement(ctx context.Context, entitlement string, params OfferListParams, requestOptions ...RequestOption) (*OfferList, error) {
	if strings.TrimSpace(entitlement) == "" || len(entitlement) > 256 {
		return nil, invalidErr("entitlement must be a nonempty key of at most 256 bytes")
	}
	if params.Kind != OfferPermanent && params.Kind != OfferFinite && params.Kind != OfferRecurring {
		return nil, invalidErr("kind must be permanent, finite or recurring")
	}
	if params.Limit < 0 || params.Limit > 100 {
		return nil, invalidErr("limit must be between 1 and 100")
	}
	query := url.Values{"entitlement": {entitlement}, "kind": {string(params.Kind)}}
	if params.PreferredCurrency != "" {
		query.Set("preferred_currency", params.PreferredCurrency)
	}
	if params.Limit > 0 {
		query.Set("limit", strconv.Itoa(params.Limit))
	}
	if params.Cursor != "" {
		query.Set("cursor", params.Cursor)
	}
	var result OfferList
	if err := c.do(ctx, http.MethodGet, "/v1/merchant/catalog/offers?"+query.Encode(), nil, &result, requestOptions...); err != nil {
		return nil, err
	}
	return &result, nil
}
