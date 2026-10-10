//go:build e2e && integration

package ci_test

import (
	"context"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// heldKeys answers which of keys the customer holds at at (zero: now) by one
// entitlements read; every key is in the answer.
func heldKeys(ctx context.Context, client *openrails.Client, customer billing.CustomerID, at time.Time, keys ...string) (map[string]bool, error) {
	out := make(map[string]bool, len(keys))
	for _, key := range keys {
		out[key] = false
	}
	page, err := client.ListEntitlements(ctx, billing.EntitlementListParams{
		CustomerIDs: []billing.CustomerID{customer}, Entitlements: keys, At: at, PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit},
	})
	if err != nil {
		return nil, err
	}
	for _, row := range page.Items {
		out[row.Entitlement] = true
	}
	return out, nil
}

// heldProducts answers which of products the customer holds now: a live
// window of each.
func heldProducts(ctx context.Context, client *openrails.Client, customer billing.CustomerID, products ...billing.ProductID) (map[billing.ProductID]bool, error) {
	out := make(map[billing.ProductID]bool, len(products))
	for _, product := range products {
		out[product] = false
	}
	page, err := client.ListProductAccess(ctx, billing.ProductAccessListParams{
		CustomerIDs: []billing.CustomerID{customer}, ProductIDs: products, LiveOnly: true, PageRequest: billing.PageRequest{Limit: billing.MaxPageLimit},
	})
	if err != nil {
		return nil, err
	}
	for _, window := range page.Items {
		out[window.ProductID] = true
	}
	return out, nil
}
