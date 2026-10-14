package service

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestProviderCadenceKeepsCalendarMonthsDistinctFromFixedDays(t *testing.T) {
	hours, months, err := providerCadence("month", 1)
	require.NoError(t, err)
	require.Nil(t, hours)
	require.Equal(t, new(1), months)
	hours, months, err = providerCadence("day", 30)
	require.NoError(t, err)
	require.Equal(t, new(720), hours)
	require.Nil(t, months)
	hours, months, err = providerCadence("year", 1)
	require.NoError(t, err)
	require.Nil(t, hours)
	require.Equal(t, new(12), months)
}

func TestProviderObjectReferenceIsExact(t *testing.T) {
	linked := map[string]string{"rail": "ccbill", "psp_id": "psp", "flex_id": "form", "recurring_billing_option_id": "option-a"}
	require.False(t, sameProviderRefs(linked, map[string]string{"flex_id": "form"}), "dropping the price discriminator changes the object")
	require.False(t, sameProviderRefs(linked, map[string]string{"flex_id": "form", "recurring_billing_option_id": "option-b"}))
	require.True(t, sameProviderRefs(linked, map[string]string{"flex_id": "form", "recurring_billing_option_id": "option-a"}))
}
