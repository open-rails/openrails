//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
)

// change is the customer's own subscription change (POST
// /v1/me/subscriptions/{id}/change): a refusal is a *billing.StatusError.
func (c *customer) change(sub billing.SubscriptionID, params billing.ChangeSubscriptionParams) (*billing.SubscriptionChange, error) {
	c.w.t.Helper()
	return c.changeAt(c.w.server.URL, sub, params)
}

// changeAt is change through the server at base. A processing answer whose
// charge River's executor is still running is replayed once it finishes.
func (c *customer) changeAt(base string, sub billing.SubscriptionID, params billing.ChangeSubscriptionParams) (*billing.SubscriptionChange, error) {
	var out billing.SubscriptionChange
	err := c.changeCall(base, "/subscriptions/"+sub.String()+"/change", params.IdempotencyKey, params, &out)
	if err == nil && out.Status == "processing" && params.IdempotencyKey != "" && !out.OperationID.IsZero() && c.w.awaitExecutor(`i.id = $1`, out.OperationID.UUID()) {
		out = billing.SubscriptionChange{}
		err = c.changeCall(base, "/subscriptions/"+sub.String()+"/change", params.IdempotencyKey, params, &out)
	}
	return &out, err
}

// previewChange is the customer's preview of a change.
func (c *customer) previewChange(sub billing.SubscriptionID, params billing.ChangeSubscriptionParams) (*billing.SubscriptionChangePreview, error) {
	c.w.t.Helper()
	var out billing.SubscriptionChangePreview
	return &out, c.changeCall(c.w.server.URL, "/subscriptions/"+sub.String()+"/change/preview", "", params, &out)
}

func (c *customer) changeCall(base, path, key string, params billing.ChangeSubscriptionParams, out any) error {
	raw, err := json.Marshal(params)
	require.NoError(c.w.t, err)
	req, err := http.NewRequestWithContext(c.w.t.Context(), http.MethodPost, base+mountPrefix+"/v1/me"+path, bytes.NewReader(raw))
	require.NoError(c.w.t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, err := http.DefaultClient.Do(req)
	require.NoError(c.w.t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(c.w.t, err)
	if res.StatusCode >= 300 {
		var envelope struct {
			Error billing.ErrorDetails `json:"error"`
		}
		require.NoError(c.w.t, json.Unmarshal(body, &envelope), "%s: %s", path, body)
		return &billing.StatusError{Status: res.StatusCode, ErrorDetails: envelope.Error}
	}
	require.NoError(c.w.t, json.Unmarshal(body, out), "%s: %s", path, body)
	return nil
}

// seats is a quantity for a change request.
func seats(n int) *int { return &n }

// priceRef is a price for a change request.
func priceRef(id billing.PriceID) *billing.PriceID { return &id }
