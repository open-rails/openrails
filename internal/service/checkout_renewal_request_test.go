package service

import (
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

func TestCheckoutRequestPreservesOrderRenewalChoice(t *testing.T) {
	for _, choice := range []*bool{nil, new(true), new(false)} {
		request, err := checkoutCreateRequest(billing.CreateCheckoutAttemptParams{AutoRenew: choice}, nil)
		require.NoError(t, err)
		require.Equal(t, choice, request.AutoRenew)
	}
}
