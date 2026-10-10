package catalog

// Stripe Features mirror product entitlements one way (OpenRails -> Stripe):
// each entitlement string is a Feature whose lookup_key is that string,
// attached to the synced Product. Stripe's per-customer active entitlements are
// never read back as authority.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	catalogwire "github.com/open-rails/openrails/catalog"
)

// StripeMetadataOpenRailsManaged marks a Stripe Feature as OpenRails-owned;
// SyncProductFeatures detaches only features carrying it.
const StripeMetadataOpenRailsManaged = "openrails_managed"

// StripeFeature is the subset of Stripe's entitlement Feature resource we read.
type StripeFeature struct {
	ID        string            `json:"id"`
	LookupKey string            `json:"lookup_key"`
	Name      string            `json:"name"`
	Metadata  map[string]string `json:"metadata"`
}

func (f StripeFeature) managed() bool {
	return strings.EqualFold(strings.TrimSpace(f.Metadata[StripeMetadataOpenRailsManaged]), "true")
}

// StripeProductFeature is the subset of Stripe's Product Feature (the
// feature<->product attachment) we read. The nested entitlement_feature is
// expanded inline by Stripe.
type StripeProductFeature struct {
	ID                 string        `json:"id"`
	EntitlementFeature StripeFeature `json:"entitlement_feature"`
}

// CreateFeature creates an OpenRails-managed Stripe Feature at the given
// lookup_key. Stripe requires a name; callers pass the entitlement string (or a
// friendlier display name). Returns the feature id.
func (s *StripeCatalogService) CreateFeature(ctx context.Context, lookupKey, name string) (string, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return "", fmt.Errorf("stripe is not configured")
	}
	if strings.TrimSpace(lookupKey) == "" {
		return "", fmt.Errorf("feature lookup_key required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = lookupKey
	}
	form := url.Values{}
	form.Set("lookup_key", lookupKey)
	form.Set("name", name)
	form.Set("metadata["+StripeMetadataOpenRailsManaged+"]", "true")
	obj, err := s.stripePostForm(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/entitlements/features", form, "openrails-feature-"+lookupKey)
	if err != nil {
		return "", err
	}
	return obj.ID, nil
}

// ListFeatures returns every entitlement Feature in the account (paginated).
// Used to find-or-create by lookup_key and to know which features OpenRails owns.
func (s *StripeCatalogService) ListFeatures(ctx context.Context) ([]StripeFeature, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	var out []StripeFeature
	cursor := ""
	for {
		endpoint := fmt.Sprintf("%s/v1/entitlements/features?limit=%d", s.baseURL(), stripeListPageLimit)
		if cursor != "" {
			endpoint += "&starting_after=" + url.QueryEscape(cursor)
		}
		body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, endpoint)
		if err != nil {
			return nil, err
		}
		if status >= 300 {
			return nil, fmt.Errorf("stripe feature list failed: %s", parseStripeError(body))
		}
		var resp struct {
			Data    []StripeFeature `json:"data"`
			HasMore bool            `json:"has_more"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if !resp.HasMore || len(resp.Data) == 0 {
			break
		}
		cursor = resp.Data[len(resp.Data)-1].ID
	}
	return out, nil
}

// ListProductFeatures returns the features attached to a Stripe Product (paginated).
func (s *StripeCatalogService) ListProductFeatures(ctx context.Context, stripeProductID string) ([]StripeProductFeature, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	stripeProductID = strings.TrimSpace(stripeProductID)
	if stripeProductID == "" {
		return nil, fmt.Errorf("stripe_product_id required")
	}
	var out []StripeProductFeature
	cursor := ""
	for {
		endpoint := fmt.Sprintf("%s/v1/products/%s/features?limit=%d", s.baseURL(), url.PathEscape(stripeProductID), stripeListPageLimit)
		if cursor != "" {
			endpoint += "&starting_after=" + url.QueryEscape(cursor)
		}
		body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, endpoint)
		if err != nil {
			return nil, err
		}
		if status >= 300 {
			return nil, fmt.Errorf("stripe product-feature list failed: %s", parseStripeError(body))
		}
		var resp struct {
			Data    []StripeProductFeature `json:"data"`
			HasMore bool                   `json:"has_more"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Data...)
		if !resp.HasMore || len(resp.Data) == 0 {
			break
		}
		cursor = resp.Data[len(resp.Data)-1].ID
	}
	return out, nil
}

// AttachFeatureToProduct attaches a Feature to a Stripe Product. Returns the
// product-feature id.
func (s *StripeCatalogService) AttachFeatureToProduct(ctx context.Context, stripeProductID, featureID string) (string, error) {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return "", fmt.Errorf("stripe is not configured")
	}
	stripeProductID = strings.TrimSpace(stripeProductID)
	featureID = strings.TrimSpace(featureID)
	if stripeProductID == "" || featureID == "" {
		return "", fmt.Errorf("stripe_product_id and feature_id required")
	}
	form := url.Values{}
	form.Set("entitlement_feature", featureID)
	endpoint := s.baseURL() + "/v1/products/" + url.PathEscape(stripeProductID) + "/features"
	obj, err := s.stripePostForm(ctx, stripeProc.SecretKey, endpoint, form, "openrails-attach-"+stripeProductID+"-"+featureID)
	if err != nil {
		return "", err
	}
	return obj.ID, nil
}

// DetachProductFeature removes a feature attachment from a Stripe Product. A 404
// is treated as success (already gone).
func (s *StripeCatalogService) DetachProductFeature(ctx context.Context, stripeProductID, productFeatureID string) error {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return fmt.Errorf("stripe is not configured")
	}
	stripeProductID = strings.TrimSpace(stripeProductID)
	productFeatureID = strings.TrimSpace(productFeatureID)
	if stripeProductID == "" || productFeatureID == "" {
		return fmt.Errorf("stripe_product_id and product_feature_id required")
	}
	endpoint := s.baseURL() + "/v1/products/" + url.PathEscape(stripeProductID) + "/features/" + url.PathEscape(productFeatureID)
	body, status, err := s.stripeDelete(ctx, stripeProc.SecretKey, endpoint)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return nil
	}
	if status >= 300 {
		return fmt.Errorf("stripe product-feature detach failed: %s", parseStripeError(body))
	}
	return nil
}

// SyncProductFeatures makes a Stripe Product's attached features match
// desiredKeys: it find-or-creates a Feature per key (lookup_key = the key),
// attaches missing ones and detaches undesired OpenRails-managed ones; other
// features are never detached. Empty desiredKeys detaches every managed one.
func (s *StripeCatalogService) SyncProductFeatures(ctx context.Context, stripeProductID string, desiredKeys []string) error {
	stripeProc := s.stripeRail(ctx)
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return fmt.Errorf("stripe is not configured")
	}
	stripeProductID = strings.TrimSpace(stripeProductID)
	if stripeProductID == "" {
		return fmt.Errorf("stripe_product_id required")
	}

	desiredKeys, err := catalogwire.NormalizeEntitlements(desiredKeys)
	if err != nil {
		return err
	}
	desired := make(map[string]struct{}, len(desiredKeys))
	for _, key := range desiredKeys {
		desired[key] = struct{}{}
	}

	// All account features, indexed by lookup_key, plus the managed subset.
	allFeatures, err := s.ListFeatures(ctx)
	if err != nil {
		return fmt.Errorf("list features: %w", err)
	}
	featureByKey := make(map[string]StripeFeature, len(allFeatures))
	managedKeys := make(map[string]struct{}, len(allFeatures))
	for _, f := range allFeatures {
		key := f.LookupKey
		if key == "" {
			continue
		}
		featureByKey[key] = f
		if f.managed() {
			managedKeys[key] = struct{}{}
		}
	}

	// Currently attached features for this product.
	attached, err := s.ListProductFeatures(ctx, stripeProductID)
	if err != nil {
		return fmt.Errorf("list product features: %w", err)
	}
	attachedKeys := make(map[string]struct{}, len(attached))
	for _, pf := range attached {
		if key := pf.EntitlementFeature.LookupKey; key != "" {
			attachedKeys[key] = struct{}{}
		}
	}

	// Attach every desired key that isn't already attached, creating the Feature
	// first if it doesn't exist yet.
	for key := range desired {
		if _, ok := attachedKeys[key]; ok {
			continue
		}
		feat, ok := featureByKey[key]
		featureID := feat.ID
		if !ok {
			// The Feature name is the entitlement string itself.
			id, err := s.CreateFeature(ctx, key, key)
			if err != nil {
				return fmt.Errorf("create feature %q: %w", key, err)
			}
			featureID = id
			managedKeys[key] = struct{}{} // we created it -> managed
		}
		if _, err := s.AttachFeatureToProduct(ctx, stripeProductID, featureID); err != nil {
			return fmt.Errorf("attach feature %q: %w", key, err)
		}
	}

	// Detach OpenRails-managed features that are attached but no longer desired.
	for _, pf := range attached {
		key := pf.EntitlementFeature.LookupKey
		if key == "" {
			continue
		}
		if _, wanted := desired[key]; wanted {
			continue
		}
		if _, isManaged := managedKeys[key]; !isManaged {
			continue // never detach a feature OpenRails doesn't own
		}
		if err := s.DetachProductFeature(ctx, stripeProductID, pf.ID); err != nil {
			return fmt.Errorf("detach feature %q: %w", key, err)
		}
	}
	return nil
}
