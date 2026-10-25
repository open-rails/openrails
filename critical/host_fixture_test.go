//go:build integration

package critical_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/stretchr/testify/require"
)

func hostProduct(t *testing.T, c *openrails.Client, key string) *billing.Price {
	t.Helper()
	product, err := c.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Ownership: catalog.OwnershipConsumable, Entitlements: []string{key + ":owned"}})
	require.NoError(t, err)
	price, err := c.CreatePrice(t.Context(), billing.CreatePriceParams{ProductID: product.ID, Key: key, UnitAmount: 10_000_000, Currency: "USD"})
	require.NoError(t, err)
	return price
}

func (w *standaloneWorld) hostResult(t *testing.T, in any, out any) string {
	t.Helper()
	data, err := json.Marshal(in)
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), "POST", w.ControlURL, bytes.NewReader(data))
	require.NoError(t, err)
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var envelope struct {
		Result json.RawMessage
		Error  string
	}
	require.NoError(t, json.Unmarshal(raw, &envelope))
	if envelope.Error == "" && out != nil {
		require.NoError(t, json.Unmarshal(envelope.Result, out))
	}
	return envelope.Error
}
