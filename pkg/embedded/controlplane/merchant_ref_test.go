package controlplane

import (
	"encoding/json"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestMerchantRefCarriesStableUUIDOnWire(t *testing.T) {
	id, err := merchant.ParseID("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	require.NoError(t, err)
	data, err := json.Marshal(MerchantRef{ID: id, Slug: "shop"})
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","slug":"shop"}`, string(data))
	var decoded MerchantRef
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, id, decoded.ID)
}
