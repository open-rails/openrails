//go:build integration

package tests

import (
	"github.com/open-rails/openrails"
	"github.com/stretchr/testify/require"
	"testing"
)

func sdkProductID(t testing.TB, value string) openrails.ProductID {
	t.Helper()
	id, err := openrails.ParseProductID(value)
	require.NoError(t, err)
	return id
}

func sdkPriceID(t testing.TB, value string) openrails.PriceID {
	t.Helper()
	id, err := openrails.ParsePriceID(value)
	require.NoError(t, err)
	return id
}

func sdkCatalogID(t testing.TB, value string) openrails.CatalogID {
	t.Helper()
	id, err := openrails.ParseCatalogID(value)
	require.NoError(t, err)
	return id
}
