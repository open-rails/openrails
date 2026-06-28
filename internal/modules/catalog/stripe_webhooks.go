package catalog

// Stripe webhook-endpoint management (#590). OpenRails registers + reconciles its
// OWN Stripe webhook endpoint so the operator only has to supply the API key — no
// manual dashboard steps to create the endpoint, pick events, set the version, or
// copy the signing secret. The endpoint's api_version is pinned to
// stripeapi.APIVersion (closing #587 for the inbound side too).
//
// Identity for find-or-create is metadata[openrails_managed]=true, NOT the URL —
// the URL is the field that drifts (redeploys), so it can't be the key. Cost
// asymmetry baked into ReconcileWebhookEndpoint: url / enabled_events / disabled
// are patched IN PLACE (signing secret survives); only an api_version bump (or a
// lost secret) forces delete+recreate, which rotates the secret — so the caller
// must persist the returned secret on Created/Recreated.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// webhookPost POSTs a form and returns the full response body + status (unlike
// stripePostForm, which parses only {id} — webhook create needs the `secret`).
func (s *StripeCatalogService) webhookPost(ctx context.Context, secretKey, endpoint string, form url.Values) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// StripeWebhookEndpoint is the subset of Stripe's webhook_endpoint resource we read.
type StripeWebhookEndpoint struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Status        string            `json:"status"` // "enabled" | "disabled"
	APIVersion    string            `json:"api_version"`
	EnabledEvents []string          `json:"enabled_events"`
	Metadata      map[string]string `json:"metadata"`
}

func (e StripeWebhookEndpoint) managed() bool {
	return strings.EqualFold(strings.TrimSpace(e.Metadata[StripeMetadataOpenRailsManaged]), "true")
}

// CreateWebhookEndpoint registers a new OpenRails-managed webhook endpoint at
// webhookURL for the given event types, pinned to stripeapi.APIVersion. It returns
// the endpoint AND its signing secret — the secret is only ever returned here (on
// create), so the caller MUST persist it for inbound signature verification.
func (s *StripeCatalogService) CreateWebhookEndpoint(ctx context.Context, webhookURL string, events []string) (StripeWebhookEndpoint, string, error) {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return StripeWebhookEndpoint{}, "", fmt.Errorf("stripe is not configured")
	}
	webhookURL = strings.TrimSpace(webhookURL)
	if webhookURL == "" {
		return StripeWebhookEndpoint{}, "", fmt.Errorf("webhook url required")
	}
	if len(events) == 0 {
		return StripeWebhookEndpoint{}, "", fmt.Errorf("at least one enabled_event required")
	}
	form := url.Values{}
	form.Set("url", webhookURL)
	form.Set("api_version", stripeapi.APIVersion)
	form.Set("metadata["+StripeMetadataOpenRailsManaged+"]", "true")
	for _, e := range events {
		if e = strings.TrimSpace(e); e != "" {
			form.Add("enabled_events[]", e)
		}
	}
	body, status, err := s.webhookPost(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/webhook_endpoints", form)
	if err != nil {
		return StripeWebhookEndpoint{}, "", err
	}
	if status >= 300 {
		return StripeWebhookEndpoint{}, "", fmt.Errorf("stripe webhook endpoint create failed: %s", parseStripeError(body))
	}
	var out StripeWebhookEndpoint
	if err := json.Unmarshal(body, &out); err != nil {
		return StripeWebhookEndpoint{}, "", err
	}
	var sec struct {
		Secret string `json:"secret"`
	}
	_ = json.Unmarshal(body, &sec)
	if strings.TrimSpace(out.ID) == "" {
		return StripeWebhookEndpoint{}, "", fmt.Errorf("stripe webhook endpoint create returned no id")
	}
	return out, sec.Secret, nil
}

// ListWebhookEndpoints returns every webhook endpoint in the account (paginated).
func (s *StripeCatalogService) ListWebhookEndpoints(ctx context.Context) ([]StripeWebhookEndpoint, error) {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return nil, fmt.Errorf("stripe is not configured")
	}
	var endpoints []StripeWebhookEndpoint
	cursor := ""
	for {
		endpoint := fmt.Sprintf("%s/v1/webhook_endpoints?limit=%d", s.baseURL(), stripeListPageLimit)
		if cursor != "" {
			endpoint += "&starting_after=" + url.QueryEscape(cursor)
		}
		body, status, err := s.stripeGet(ctx, stripeProc.SecretKey, endpoint)
		if err != nil {
			return nil, err
		}
		if status >= 300 {
			return nil, fmt.Errorf("stripe webhook endpoint list failed: %s", parseStripeError(body))
		}
		var resp struct {
			Data    []StripeWebhookEndpoint `json:"data"`
			HasMore bool                    `json:"has_more"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}
		endpoints = append(endpoints, resp.Data...)
		if !resp.HasMore || len(resp.Data) == 0 {
			break
		}
		cursor = resp.Data[len(resp.Data)-1].ID
	}
	return endpoints, nil
}

// UpdateWebhookEndpointParams are the in-place-updatable fields (api_version is NOT
// updatable by Stripe — a version change needs delete+recreate).
type UpdateWebhookEndpointParams struct {
	URL           *string
	EnabledEvents []string // nil = leave unchanged
	Disabled      *bool
}

// UpdateWebhookEndpoint patches mutable fields in place; the signing secret is
// preserved.
func (s *StripeCatalogService) UpdateWebhookEndpoint(ctx context.Context, id string, params UpdateWebhookEndpointParams) error {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return fmt.Errorf("stripe is not configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("webhook endpoint id required")
	}
	form := url.Values{}
	if params.URL != nil {
		form.Set("url", strings.TrimSpace(*params.URL))
	}
	if params.EnabledEvents != nil {
		for _, e := range params.EnabledEvents {
			if e = strings.TrimSpace(e); e != "" {
				form.Add("enabled_events[]", e)
			}
		}
	}
	if params.Disabled != nil {
		form.Set("disabled", strconv.FormatBool(*params.Disabled))
	}
	if len(form) == 0 {
		return nil
	}
	body, status, err := s.webhookPost(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/webhook_endpoints/"+url.PathEscape(id), form)
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("stripe webhook endpoint update failed: %s", parseStripeError(body))
	}
	return nil
}

// DeleteWebhookEndpoint removes a webhook endpoint. A 404 is treated as success.
func (s *StripeCatalogService) DeleteWebhookEndpoint(ctx context.Context, id string) error {
	stripeProc := s.stripeRail()
	if stripeProc == nil || stripeProc.SecretKey == "" {
		return fmt.Errorf("stripe is not configured")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("webhook endpoint id required")
	}
	body, status, err := s.stripeDelete(ctx, stripeProc.SecretKey, s.baseURL()+"/v1/webhook_endpoints/"+url.PathEscape(id))
	if err != nil {
		return err
	}
	if status == 404 {
		return nil
	}
	if status >= 300 {
		return fmt.Errorf("stripe webhook endpoint delete failed: %s", parseStripeError(body))
	}
	return nil
}

// WebhookReconcileAction names what ReconcileWebhookEndpoint did.
type WebhookReconcileAction string

const (
	WebhookCreated   WebhookReconcileAction = "created"
	WebhookRecreated WebhookReconcileAction = "recreated"
	WebhookUpdated   WebhookReconcileAction = "updated"
	WebhookUnchanged WebhookReconcileAction = "unchanged"
)

// DesiredWebhookEndpoint is the target state for reconciliation.
type DesiredWebhookEndpoint struct {
	URL           string
	EnabledEvents []string
	// HaveSecret reports whether the caller already holds the stored signing
	// secret for the managed endpoint. When false and an endpoint already exists,
	// reconcile recreates it to obtain a fresh secret (the secret can't be
	// re-fetched from Stripe).
	HaveSecret bool
}

// WebhookReconcileResult reports the action taken and, on Created/Recreated, the
// new signing secret the caller must persist (empty otherwise — the existing
// secret is still valid).
type WebhookReconcileResult struct {
	Action     WebhookReconcileAction
	EndpointID string
	Secret     string
}

// ReconcileWebhookEndpoint brings the OpenRails-managed webhook endpoint to the
// desired state, idempotently:
//   - none found            -> create (returns secret)
//   - api_version drifted    -> delete + recreate (returns new secret)
//   - caller lost the secret -> delete + recreate (returns new secret)
//   - url/events/disabled drift -> patch in place (secret preserved)
//   - already correct        -> no-op
//
// Endpoints without the openrails_managed marker are ignored (never touched).
func (s *StripeCatalogService) ReconcileWebhookEndpoint(ctx context.Context, desired DesiredWebhookEndpoint) (WebhookReconcileResult, error) {
	if strings.TrimSpace(desired.URL) == "" {
		return WebhookReconcileResult{}, fmt.Errorf("desired webhook url required")
	}
	if len(desired.EnabledEvents) == 0 {
		return WebhookReconcileResult{}, fmt.Errorf("desired enabled_events required")
	}
	all, err := s.ListWebhookEndpoints(ctx)
	if err != nil {
		return WebhookReconcileResult{}, fmt.Errorf("list webhook endpoints: %w", err)
	}
	var existing *StripeWebhookEndpoint
	for i := range all {
		if all[i].managed() {
			existing = &all[i]
			break
		}
	}

	if existing == nil {
		ep, secret, err := s.CreateWebhookEndpoint(ctx, desired.URL, desired.EnabledEvents)
		if err != nil {
			return WebhookReconcileResult{}, err
		}
		return WebhookReconcileResult{Action: WebhookCreated, EndpointID: ep.ID, Secret: secret}, nil
	}

	// Version drift or a lost secret both require a recreate (api_version isn't
	// updatable; the secret can't be re-fetched).
	if existing.APIVersion != stripeapi.APIVersion || !desired.HaveSecret {
		if err := s.DeleteWebhookEndpoint(ctx, existing.ID); err != nil {
			return WebhookReconcileResult{}, fmt.Errorf("recreate (delete old): %w", err)
		}
		ep, secret, err := s.CreateWebhookEndpoint(ctx, desired.URL, desired.EnabledEvents)
		if err != nil {
			return WebhookReconcileResult{}, err
		}
		return WebhookReconcileResult{Action: WebhookRecreated, EndpointID: ep.ID, Secret: secret}, nil
	}

	// In-place patch for the cheap drifts (secret survives).
	params := UpdateWebhookEndpointParams{}
	changed := false
	if strings.TrimSpace(existing.URL) != strings.TrimSpace(desired.URL) {
		u := desired.URL
		params.URL = &u
		changed = true
	}
	if !sameStringSet(existing.EnabledEvents, desired.EnabledEvents) {
		params.EnabledEvents = desired.EnabledEvents
		changed = true
	}
	if strings.EqualFold(existing.Status, "disabled") {
		dis := false
		params.Disabled = &dis
		changed = true
	}
	if !changed {
		return WebhookReconcileResult{Action: WebhookUnchanged, EndpointID: existing.ID}, nil
	}
	if err := s.UpdateWebhookEndpoint(ctx, existing.ID, params); err != nil {
		return WebhookReconcileResult{}, err
	}
	return WebhookReconcileResult{Action: WebhookUpdated, EndpointID: existing.ID}, nil
}

// sameStringSet compares two string slices as sets (order-insensitive).
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ca := append([]string(nil), a...)
	cb := append([]string(nil), b...)
	sort.Strings(ca)
	sort.Strings(cb)
	for i := range ca {
		if ca[i] != cb[i] {
			return false
		}
	}
	return true
}
