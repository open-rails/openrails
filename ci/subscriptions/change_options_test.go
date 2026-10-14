//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// The customer's plan menu lists each other live tier of the subscription's
// group, ranked, with the change's own preview; never an archived tier or
// another group's.
func TestSubscriptionChangeOptions(t *testing.T) {
	t.Parallel()
	forEachRail(t, func(t *testing.T, rail string) {
		w := newWorld(t)
		group := "g" + uuid.NewString()[:8]
		low := w.tierPrice(group, 1, 1000, 720, false)
		mid := w.tierPrice(group, 2, 2000, 720, false)
		high := w.tierPrice(group, 3, 3000, 720, false)
		archived := w.tierPrice(group, 4, 4000, 720, false)
		_, err := w.client[embedded].UpdateProduct(t.Context(), archived.ProductID, billing.UpdateProductParams{Archived: catalog.Value(true)})
		require.NoError(t, err)
		w.tierPrice("g"+uuid.NewString()[:8], 5, 5000, 720, false)

		c, sub := w.engineMember(rail, embedded, mid)
		w.advance(time.Hour)
		status, options := c.changeOptions(sub)
		require.Equal(t, http.StatusOK, status)
		require.Len(t, options, 2)
		require.Equal(t, low.ID, options[0].Price.ID)
		require.Equal(t, high.ID, options[1].Price.ID)
		require.Equal(t, "period_end", options[0].Preview.Effective)
		require.Equal(t, "now", options[1].Preview.Effective)
		for _, option := range options {
			preview, err := c.previewChange(sub, billing.ChangeSubscriptionParams{PriceID: priceRef(option.Price.ID)})
			require.NoError(t, err)
			require.Equal(t, *preview, option.Preview, "an option carries the change's own preview")
		}

		status, _ = w.newCustomer().changeOptions(sub)
		require.Equal(t, http.StatusNotFound, status, "another customer's subscription")
	})
}

func (c *customer) changeOptions(sub billing.SubscriptionID) (int, []billing.SubscriptionChangeOption) {
	c.w.t.Helper()
	req, err := http.NewRequestWithContext(c.w.t.Context(), http.MethodGet, c.w.server.URL+mountPrefix+"/v1/me/subscriptions/"+sub.String()+"/change/options", nil)
	require.NoError(c.w.t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := http.DefaultClient.Do(req)
	require.NoError(c.w.t, err)
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	require.NoError(c.w.t, err)
	var page struct {
		Data []billing.SubscriptionChangeOption `json:"data"`
	}
	if res.StatusCode == http.StatusOK {
		require.NoError(c.w.t, json.Unmarshal(raw, &page), "%s", raw)
	}
	return res.StatusCode, page.Data
}
