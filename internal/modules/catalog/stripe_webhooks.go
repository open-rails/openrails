package catalog

// Stripe webhook-endpoint management. OpenRails registers and reconciles its own
// endpoint, pinned to stripeapi.APIVersion, so the operator supplies only the
// API key. Find-or-create keys on metadata[openrails_managed], never the URL
// (it drifts on redeploys); url, events and disabled are patched in place.
//
// Rollover, never delete-then-create: api_version is create-only and the
// signing secret is returned only at creation, so a version bump needs a new
// endpoint. The successor is created first; predecessors are stamped
// metadata[openrails_superseded_at] and stay enabled. Only
// RetireSupersededWebhookEndpoints deletes, after the overlap window and behind
// the operator kill switch (https://docs.stripe.com/webhooks/versioning).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
)

// StripeMetadataSupersededAt (RFC 3339) marks a managed endpoint a newer one
// replaced. It is the only retirement clock and lives on the Stripe object.
const StripeMetadataSupersededAt = "openrails_superseded_at"

// WebhookRolloverOverlap is how long a superseded endpoint keeps delivering
// before it may be retired. Stripe retries a failed delivery for up to 3 days;
// a week leaves room for the retry tail plus an operator's working week.
const WebhookRolloverOverlap = 7 * 24 * time.Hour

// maxManagedWebhookEndpoints caps managed endpoints per Stripe account, well
// short of Stripe's 16, so a looping rollover cannot exhaust it. Hitting the cap
// asks the operator to retire superseded endpoints.
const maxManagedWebhookEndpoints = 4

// ErrWebhookEndpointBudgetExhausted means maxManagedWebhookEndpoints managed
// endpoints already exist, so a rollover would risk Stripe's per-account limit.
// Retire the superseded ones first.
var ErrWebhookEndpointBudgetExhausted = errors.New("managed stripe webhook endpoint budget exhausted: retire superseded endpoints before rolling over again")

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
	Created       int64             `json:"created"`
	EnabledEvents []string          `json:"enabled_events"`
	Metadata      map[string]string `json:"metadata"`
}

func (e StripeWebhookEndpoint) managed() bool {
	return strings.EqualFold(strings.TrimSpace(e.Metadata[StripeMetadataOpenRailsManaged]), "true")
}

// supersededAt reports when a newer managed endpoint replaced this one. An
// unparseable stamp reads as NOT superseded — a garbled timestamp must never be
// the reason an endpoint is deleted.
func (e StripeWebhookEndpoint) supersededAt() (time.Time, bool) {
	raw := strings.TrimSpace(e.Metadata[StripeMetadataSupersededAt])
	if raw == "" {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, false
	}
	return ts.UTC(), true
}

// SupersededEndpoint is a managed endpoint that a newer one has replaced. It
// keeps delivering until RetireAfter, so nothing in flight is lost.
type SupersededEndpoint struct {
	ID          string
	APIVersion  string
	Since       time.Time
	RetireAfter time.Time
}

// CreateWebhookEndpoint registers a new OpenRails-managed webhook endpoint at
// webhookURL for the given event types, pinned to stripeapi.APIVersion. It returns
// the endpoint AND its signing secret — the secret is only ever returned here (on
// create), so the caller MUST persist it for inbound signature verification.
func (s *StripeCatalogService) CreateWebhookEndpoint(ctx context.Context, webhookURL string, events []string) (StripeWebhookEndpoint, string, error) {
	stripeProc := s.stripeRail(ctx)
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
	stripeProc := s.stripeRail(ctx)
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

// UpdateWebhookEndpointParams are the in-place-updatable fields. api_version is
// not one: a version change needs a new endpoint (rollover).
type UpdateWebhookEndpointParams struct {
	URL           *string
	EnabledEvents []string // nil = leave unchanged
	Disabled      *bool
	Metadata      map[string]string // merged key-by-key; "" deletes a key
}

// UpdateWebhookEndpoint patches mutable fields in place; the signing secret is
// preserved.
func (s *StripeCatalogService) UpdateWebhookEndpoint(ctx context.Context, id string, params UpdateWebhookEndpointParams) error {
	stripeProc := s.stripeRail(ctx)
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
	for k, v := range params.Metadata {
		if k = strings.TrimSpace(k); k != "" {
			form.Set("metadata["+k+"]", v)
		}
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
	stripeProc := s.stripeRail(ctx)
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
	WebhookCreated WebhookReconcileAction = "created"
	// WebhookRolledOver: a successor endpoint at the pinned api_version was
	// created and the predecessors were stamped superseded. They are STILL
	// ENABLED and still delivering — this action never removes anything.
	WebhookRolledOver WebhookReconcileAction = "rolled_over"
	WebhookUpdated    WebhookReconcileAction = "updated"
	WebhookUnchanged  WebhookReconcileAction = "unchanged"
)

// DesiredWebhookEndpoint is the target state for reconciliation.
type DesiredWebhookEndpoint struct {
	// PublishSecret must durably publish custody before predecessors are superseded.
	PublishSecret            func(context.Context, string, string) error
	PublishedEndpointID      string
	RequirePublishedIdentity bool

	URL           string
	EnabledEvents []string
	// HaveSecret reports whether the caller holds the signing secret of the
	// current-version managed endpoint. False is never a reason to remove that
	// endpoint; reconcile stands a successor up alongside it.
	HaveSecret bool
	// ForbidCreate refuses any create with ErrWebhookCreateForbidden before
	// mutating Stripe, for a caller that cannot persist a minted signing secret.
	ForbidCreate bool
	// Now overrides the rollover clock (tests). Zero = time.Now().
	Now time.Time
	// RetireOverlap overrides WebhookRolloverOverlap when reporting when a
	// superseded endpoint becomes retireable (tests).
	RetireOverlap time.Duration
}

// ErrWebhookCreateForbidden is returned when reconcile would have to create a
// managed endpoint (minting a signing secret) but ForbidCreate is set.
var ErrWebhookCreateForbidden = errors.New("managed stripe webhook endpoint create refused: the minted signing secret cannot be persisted by this caller")

// WebhookReconcileResult reports the action taken and, on Created/RolledOver,
// the new signing secret the caller must persist (empty otherwise — the existing
// secret is still valid).
type WebhookReconcileResult struct {
	UnqualifiedPredecessors bool
	Action                  WebhookReconcileAction
	EndpointID              string
	Secret                  string
	// Superseded are the endpoints this pass stamped as replaced. They are still
	// enabled; only RetireSupersededWebhookEndpoints removes them.
	Superseded []string
	// Legacy is every managed endpoint currently carrying a superseded stamp,
	// with the instant it becomes eligible for retirement. Non-empty is the
	// operator's cue that a rollover is in flight.
	Legacy []SupersededEndpoint
}

// ReconcileWebhookEndpoint brings the OpenRails-managed webhook endpoints to the
// desired state, idempotently and additively:
//
//   - none found, api_version drift, or no held secret -> create a successor
//     (returns its secret); predecessors are stamped superseded, stay enabled
//   - url/events/disabled drift -> patch in place (secret preserved)
//   - already correct -> no-op
//
// No branch deletes. Endpoints without the openrails_managed marker are never
// touched.
func (s *StripeCatalogService) ReconcileWebhookEndpoint(ctx context.Context, desired DesiredWebhookEndpoint) (WebhookReconcileResult, error) {
	if strings.TrimSpace(desired.URL) == "" {
		return WebhookReconcileResult{}, fmt.Errorf("desired webhook url required")
	}
	if len(desired.EnabledEvents) == 0 {
		return WebhookReconcileResult{}, fmt.Errorf("desired enabled_events required")
	}
	now := desired.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	all, err := s.ListWebhookEndpoints(ctx)
	if err != nil {
		return WebhookReconcileResult{}, fmt.Errorf("list webhook endpoints: %w", err)
	}
	managed, current := partitionManagedEndpoints(all)

	if current != nil && desired.HaveSecret && (!desired.RequirePublishedIdentity || current.ID == desired.PublishedEndpointID) {
		res, err := s.patchInPlace(ctx, current, desired)
		if err != nil {
			return WebhookReconcileResult{}, err
		}
		stamp := map[string]string{StripeMetadataSupersededAt: now.Format(time.RFC3339)}
		for _, m := range managed {
			if m.ID == current.ID {
				continue
			}
			if _, ok := m.supersededAt(); ok {
				continue
			}
			if err := s.UpdateWebhookEndpoint(ctx, m.ID, UpdateWebhookEndpointParams{Metadata: stamp}); err != nil {
				res.UnqualifiedPredecessors = true
				continue
			}
			m.Metadata[StripeMetadataSupersededAt] = stamp[StripeMetadataSupersededAt]
		}
		res.Legacy = supersededEndpoints(managed, current.ID, desired.RetireOverlap)
		return res, nil
	}

	// A new endpoint at the pinned api_version is required — either none exists
	// there, or we hold no secret for the one that does. Both are additive.
	if desired.ForbidCreate {
		if current == nil && len(managed) == 0 {
			return WebhookReconcileResult{}, ErrWebhookCreateForbidden
		}
		return WebhookReconcileResult{}, fmt.Errorf("rollover needed (api_version drift or unknown signing secret): %w", ErrWebhookCreateForbidden)
	}
	if len(managed) >= maxManagedWebhookEndpoints {
		return WebhookReconcileResult{}, fmt.Errorf("%w (%d managed endpoints on this account)", ErrWebhookEndpointBudgetExhausted, len(managed))
	}

	// CREATE FIRST. Until this succeeds every existing endpoint is untouched, so
	// a failure here costs nothing but a retry.
	ep, secret, err := s.CreateWebhookEndpoint(ctx, desired.URL, desired.EnabledEvents)
	if err != nil {
		return WebhookReconcileResult{}, err
	}

	if desired.PublishSecret != nil {
		if err := desired.PublishSecret(ctx, ep.ID, secret); err != nil {
			return WebhookReconcileResult{}, fmt.Errorf("publish webhook credential for %s (existing endpoints remain enabled): %w", ep.ID, err)
		}
	}
	action := WebhookCreated
	if len(managed) > 0 {
		action = WebhookRolledOver
	}
	res := WebhookReconcileResult{Action: action, EndpointID: ep.ID, Secret: secret}
	stamp := map[string]string{StripeMetadataSupersededAt: now.Format(time.RFC3339)}
	for _, m := range managed {
		if m.ID == ep.ID {
			continue
		}
		if _, already := m.supersededAt(); already {
			continue
		}
		// A stamp failure is not fatal: the successor is already live and the
		// predecessor keeps delivering. The next pass re-stamps it.
		if err := s.UpdateWebhookEndpoint(ctx, m.ID, UpdateWebhookEndpointParams{Metadata: stamp}); err != nil {
			res.UnqualifiedPredecessors = true
			continue
		}
		m.Metadata[StripeMetadataSupersededAt] = stamp[StripeMetadataSupersededAt]
		res.Superseded = append(res.Superseded, m.ID)
	}
	res.Legacy = supersededEndpoints(managed, ep.ID, desired.RetireOverlap)
	return res, nil
}

// patchInPlace applies the cheap drifts to the current-version endpoint; the
// signing secret survives every one of them.
func (s *StripeCatalogService) patchInPlace(ctx context.Context, existing *StripeWebhookEndpoint, desired DesiredWebhookEndpoint) (WebhookReconcileResult, error) {
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

// partitionManagedEndpoints returns every OpenRails-managed endpoint plus the
// one that is CURRENT: pinned to stripeapi.APIVersion and not superseded. When
// several qualify the newest wins (Stripe's `created`, id as the tiebreak), so
// the successor a rollover just made always outranks the endpoint it replaced —
// even if stamping that predecessor failed.
func partitionManagedEndpoints(all []StripeWebhookEndpoint) ([]*StripeWebhookEndpoint, *StripeWebhookEndpoint) {
	var managed []*StripeWebhookEndpoint
	var current *StripeWebhookEndpoint
	for i := range all {
		ep := &all[i]
		if !ep.managed() {
			continue
		}
		if ep.Metadata == nil {
			ep.Metadata = map[string]string{}
		}
		managed = append(managed, ep)
		if ep.APIVersion != stripeapi.APIVersion {
			continue
		}
		if _, superseded := ep.supersededAt(); superseded {
			continue
		}
		if current == nil || ep.Created > current.Created ||
			(ep.Created == current.Created && ep.ID > current.ID) {
			current = ep
		}
	}
	return managed, current
}

// supersededEndpoints lists the stamped predecessors, excluding the live one.
// overlap <= 0 means WebhookRolloverOverlap.
func supersededEndpoints(managed []*StripeWebhookEndpoint, currentID string, overlap time.Duration) []SupersededEndpoint {
	if overlap <= 0 {
		overlap = WebhookRolloverOverlap
	}
	var out []SupersededEndpoint
	for _, m := range managed {
		if m.ID == currentID {
			continue
		}
		since, ok := m.supersededAt()
		if !ok {
			continue
		}
		out = append(out, SupersededEndpoint{
			ID: m.ID, APIVersion: m.APIVersion,
			Since: since, RetireAfter: since.Add(overlap),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// RetireSupersededParams controls the ONE delete path in this file.
type RetireSupersededParams struct {
	Now time.Time
	// Overlap is how long a superseded endpoint must have been replaced before
	// it may be deleted. Zero = WebhookRolloverOverlap.
	Overlap time.Duration
}

// RetireSupersededResult reports what was removed and what is still in its
// overlap window.
type RetireSupersededResult struct {
	Retired []string
	Pending []SupersededEndpoint
}

// RetireSupersededWebhookEndpoints deletes managed endpoints that a newer one
// replaced longer ago than the overlap window. It is the only place OpenRails
// deletes a Stripe webhook endpoint, and it refuses unless a healthy
// current-version managed endpoint exists — the account is never left with no
// way to reach us.
func (s *StripeCatalogService) RetireSupersededWebhookEndpoints(ctx context.Context, p RetireSupersededParams) (RetireSupersededResult, error) {
	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	overlap := p.Overlap
	if overlap <= 0 {
		overlap = WebhookRolloverOverlap
	}

	all, err := s.ListWebhookEndpoints(ctx)
	if err != nil {
		return RetireSupersededResult{}, fmt.Errorf("list webhook endpoints: %w", err)
	}
	managed, current := partitionManagedEndpoints(all)
	if current == nil {
		// No successor is live: retiring now would leave no endpoint.
		return RetireSupersededResult{Pending: supersededEndpoints(managed, "", overlap)},
			fmt.Errorf("refusing to retire superseded stripe webhook endpoints: no live endpoint at api_version %s", stripeapi.APIVersion)
	}
	if !strings.EqualFold(strings.TrimSpace(current.Status), "enabled") {
		return RetireSupersededResult{Pending: supersededEndpoints(managed, current.ID, overlap)},
			fmt.Errorf("refusing to retire superseded stripe webhook endpoints: successor %s is %s, not enabled", current.ID, current.Status)
	}

	var out RetireSupersededResult
	for _, cand := range supersededEndpoints(managed, current.ID, overlap) {
		if now.Before(cand.RetireAfter) {
			out.Pending = append(out.Pending, cand)
			continue
		}
		if err := s.DeleteWebhookEndpoint(ctx, cand.ID); err != nil {
			return out, fmt.Errorf("retire superseded endpoint %s: %w", cand.ID, err)
		}
		out.Retired = append(out.Retired, cand.ID)
	}
	return out, nil
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
