package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequireCurrency(t *testing.T) {
	got, err := requireCurrency(" EUR ")
	require.NoError(t, err)
	require.Equal(t, "EUR", got)

	got, err = requireCurrency(" usd ")
	require.NoError(t, err)
	require.Equal(t, "USD", got)

	got, err = requireCurrency("merchant/gold")
	require.NoError(t, err)
	require.Equal(t, "merchant/gold", got)

	_, err = requireCurrency(" ")
	require.EqualError(t, err, "currency required")
}

func TestGetCreditsRequiresCurrency(t *testing.T) {
	svc := &Service{}
	got, err := svc.GetCredits(t.Context(), "user_123")
	require.Nil(t, got)
	require.EqualError(t, err, "currency required; use GetCreditsByType")
}
