package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestCardCatalogProvidersRejectTokenTermsBeforeIO(t *testing.T) {
	for _, currency := range []string{"SOL", "USDC"} {
		terms := autoCreateContext{Currency: currency, UnitAmount: 1_000_000}
		for _, adapter := range []providerAdapter{&stripeAdapter{}, &nmiAdapter{}} {
			_, err := adapter.AutoCreate(t.Context(), terms)
			require.ErrorContains(t, err, "not supported by card payment rails")
			_, err = adapter.Attach(t.Context(), map[string]string{}, terms)
			require.ErrorContains(t, err, "not supported by card payment rails")
			_, _, err = adapter.Verify(t.Context(), map[string]string{}, &priceVerifyContext{Currency: currency, UnitAmount: 1_000_000})
			require.ErrorContains(t, err, "not supported by card payment rails")
		}
	}
}
