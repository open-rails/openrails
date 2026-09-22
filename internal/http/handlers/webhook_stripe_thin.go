package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/modules/webhooks"
)

// stripeAPIBase is the ONE host thin-event hydration may reach. Not derived
// from the payload, not configurable per request.
const stripeAPIBase = "https://api.stripe.com"

// Only v2 event retrieval uses the preview that exposes snapshot_event. Resource
// reads retain our stable parser version. This does not register a destination
// or enable the account's API-v1 thin private preview.
const stripeThinEventAPIVersion = "2025-11-17.preview"

type stripeThinEnvelope struct {
	ID            string          `json:"id"`
	Object        string          `json:"object"`
	Type          string          `json:"type"`
	Account       string          `json:"account"`
	Context       string          `json:"context"`
	Created       json.RawMessage `json:"created"`
	SnapshotEvent string          `json:"snapshot_event"`
	RelatedObject *struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"related_object"`
	Data struct {
		Object             json.RawMessage `json:"object"`
		PreviousAttributes json.RawMessage `json:"previous_attributes"`
	} `json:"data"`
}

// hydrateThinStripeEvent runs only AFTER the original bytes pass HMAC
// verification. Unknown/malformed thin events fail retryably; acknowledging an
// unrecognized v1.* event would silently lose financial notifications.
func hydrateThinStripeEvent(ctx context.Context, secretKey, accountID string, body []byte) ([]byte, error) {
	var notice stripeThinEnvelope
	if err := json.Unmarshal(body, &notice); err != nil {
		return nil, err
	}
	thin := notice.Object == "v2.core.event" || strings.HasPrefix(notice.Type, "v1.") || strings.HasPrefix(notice.Type, "v2.") || notice.RelatedObject != nil
	if !thin {
		// A signed snapshot can still name a foreign Connect account/context.
		// The routed credential scope applies before either delivery format.
		return nil, validateStripeEventScope(notice, accountID)
	}
	eventType := strings.TrimPrefix(notice.Type, "v1.")
	if !strings.HasPrefix(notice.Type, "v1.") || !slices.Contains(webhooks.HandledStripeEventTypes, eventType) {
		return nil, fmt.Errorf("unsupported stripe thin event type %q", notice.Type)
	}
	if !stripeIdentifier(notice.ID, "evt_") || !stripeIdentifier(accountID, "acct_") || strings.TrimSpace(secretKey) == "" {
		return nil, fmt.Errorf("thin stripe event requires event id and exact account credentials")
	}
	if err := validateStripeEventScope(notice, accountID); err != nil {
		return nil, err
	}
	if _, _, err := thinStripeResource(notice, eventType); err != nil {
		return nil, err
	}

	// A direct account key must actually belong to the routed account. Never use
	// a payload-supplied context to switch the authority of an organization key.
	client := stripeapi.ReadOnlyClient(15 * time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	account, err := fetchThinStripeJSON(ctx, client, secretKey, "/v1/account", "")
	if err != nil {
		return nil, err
	}
	var identity struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(account, &identity); err != nil || identity.ID != accountID {
		return nil, fmt.Errorf("stripe credential account does not match webhook account")
	}

	raw, err := fetchThinStripeJSON(ctx, client, secretKey, "/v2/core/events/"+notice.ID, stripeThinEventAPIVersion)
	if err != nil {
		return nil, err
	}
	var event stripeThinEnvelope
	if err := json.Unmarshal(raw, &event); err != nil {
		return nil, err
	}
	if event.ID != notice.ID || event.Type != notice.Type || event.Context != notice.Context || event.Account != notice.Account {
		return nil, fmt.Errorf("fetched stripe event does not match signed notification")
	}
	if err := validateStripeEventScope(event, accountID); err != nil {
		return nil, err
	}
	resourcePath, resourceType, err := thinStripeResource(event, eventType)
	if err != nil {
		return nil, err
	}
	if event.RelatedObject.ID != notice.RelatedObject.ID || event.RelatedObject.Type != notice.RelatedObject.Type {
		return nil, fmt.Errorf("fetched stripe event resource does not match signed notification")
	}
	if notice.SnapshotEvent != "" && notice.SnapshotEvent != event.SnapshotEvent {
		return nil, fmt.Errorf("fetched stripe snapshot correlation mismatch")
	}
	eventID := event.ID
	previous := event.Data.PreviousAttributes
	if event.SnapshotEvent != "" {
		if !stripeIdentifier(event.SnapshotEvent, "evt_") {
			return nil, fmt.Errorf("invalid stripe snapshot event id")
		}
		snapshotRaw, err := fetchThinStripeJSON(ctx, client, secretKey, "/v1/events/"+event.SnapshotEvent, "")
		if err != nil {
			return nil, err
		}
		var snapshot stripeThinEnvelope
		if err := json.Unmarshal(snapshotRaw, &snapshot); err != nil {
			return nil, err
		}
		if snapshot.ID != event.SnapshotEvent || snapshot.Type != eventType {
			return nil, fmt.Errorf("stripe snapshot correlation identity mismatch")
		}
		if err := validateStripeEventScope(snapshot, accountID); err != nil {
			return nil, err
		}
		if err := validateThinStripeResource(snapshot.Data.Object, event.RelatedObject.ID, resourceType); err != nil {
			return nil, err
		}
		previous = snapshot.Data.PreviousAttributes
		eventID = event.SnapshotEvent // Same dedup key as the snapshot destination.
	}
	object, err := fetchThinStripeJSON(ctx, client, secretKey, resourcePath, "")
	if err != nil {
		return nil, err
	}
	if err := validateThinStripeResource(object, event.RelatedObject.ID, resourceType); err != nil {
		return nil, err
	}
	created, err := thinStripeCreated(event.Created)
	if err != nil {
		return nil, err
	}
	// Keep event metadata (context, changes, reason, snapshot_event) intact. Only
	// adapt the fields consumed by the existing snapshot handlers and deduper.
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	result["id"], _ = json.Marshal(eventID)
	result["type"], _ = json.Marshal(eventType)
	result["created"], _ = json.Marshal(created)
	result["data"], _ = json.Marshal(struct {
		Object             json.RawMessage `json:"object"`
		PreviousAttributes json.RawMessage `json:"previous_attributes,omitempty"`
	}{object, previous})
	return json.Marshal(result)
}

func validateStripeEventScope(event stripeThinEnvelope, accountID string) error {
	if (event.Account != "" && event.Account != accountID) || (event.Context != "" && event.Context != accountID) {
		return fmt.Errorf("stripe event account/context does not match routed account")
	}
	return nil
}

func stripeIdentifier(id, prefix string) bool {
	if !strings.HasPrefix(id, prefix) || len(id) <= len(prefix) || len(id) > 255 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// Never fetch an arbitrary signed URL, even on api.stripe.com. Derive the one
// permitted read from a supported event type and validate the advertised path.
func thinStripeResource(event stripeThinEnvelope, eventType string) (string, string, error) {
	var kind, collection, prefix string
	switch {
	case strings.HasPrefix(eventType, "payment_intent."):
		kind, collection, prefix = "payment_intent", "payment_intents", "pi_"
	case strings.HasPrefix(eventType, "invoice."):
		kind, collection, prefix = "invoice", "invoices", "in_"
	case eventType == "invoice_payment.paid":
		kind, collection, prefix = "invoice_payment", "invoice_payments", "inpay_"
	case strings.HasPrefix(eventType, "checkout.session."):
		kind, collection, prefix = "checkout.session", "checkout/sessions", "cs_"
	case strings.HasPrefix(eventType, "customer.subscription."):
		kind, collection, prefix = "subscription", "subscriptions", "sub_"
	case strings.HasPrefix(eventType, "refund."):
		kind, collection, prefix = "refund", "refunds", "re_"
	case strings.HasPrefix(eventType, "charge.dispute."):
		kind, collection, prefix = "dispute", "disputes", "dp_"
	case strings.HasPrefix(eventType, "charge."):
		kind, collection, prefix = "charge", "charges", "ch_"
	case strings.HasPrefix(eventType, "payment_method."):
		kind, collection, prefix = "payment_method", "payment_methods", "pm_"
	case eventType == "customer.updated":
		kind, collection, prefix = "customer", "customers", "cus_"
	default:
		return "", "", fmt.Errorf("unsupported stripe thin resource")
	}
	related := event.RelatedObject
	if related == nil || related.Type != kind || !stripeIdentifier(related.ID, prefix) {
		return "", "", fmt.Errorf("invalid stripe thin related object")
	}
	path := "/v1/" + collection + "/" + related.ID
	if related.URL != path {
		return "", "", fmt.Errorf("stripe thin related object path mismatch")
	}
	return path, kind, nil
}

func validateThinStripeResource(raw []byte, id, kind string) error {
	var resource struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	if err := json.Unmarshal(raw, &resource); err != nil {
		return err
	}
	if resource.ID != id || resource.Object != kind {
		return fmt.Errorf("fetched stripe resource identity mismatch")
	}
	return nil
}

func thinStripeCreated(raw json.RawMessage) (int64, error) {
	var timestamp string
	if json.Unmarshal(raw, &timestamp) == nil {
		value, err := time.Parse(time.RFC3339Nano, timestamp)
		if err == nil && value.Unix() > 0 {
			return value.Unix(), nil
		}
	}
	var seconds int64
	if json.Unmarshal(raw, &seconds) == nil && seconds > 0 {
		return seconds, nil
	}
	return 0, fmt.Errorf("stripe thin event has invalid creation time")
}

func fetchThinStripeJSON(ctx context.Context, client *http.Client, secretKey, path, version string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, stripeAPIBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	if version != "" {
		req.Header.Set(stripeapi.VersionHeader, version)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("stripe thin hydration read failed (%d)", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxStripeWebhookBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxStripeWebhookBytes {
		return nil, fmt.Errorf("stripe thin hydration response too large")
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("stripe thin hydration response is not JSON")
	}
	return raw, nil
}
