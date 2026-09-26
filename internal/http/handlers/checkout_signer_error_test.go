package handlers

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/integrations/vault"
	"github.com/open-rails/openrails/internal/modules/checkout"
)

// A Solana checkout refused because its signer is unavailable or awaits
// approval answers 503, even when the service reports it as a validation
// failure of the session.
func TestCheckoutSignerUnavailableIs503(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: %w", checkout.ErrCheckoutSessionValidation, vault.ErrSignerUnapproved),
		fmt.Errorf("%w: %w", checkout.ErrCheckoutSessionValidation, vault.ErrNotAuthenticated),
	} {
		r, rec := newTestRequest(http.MethodPost, "/v1/checkout", nil, nil)
		writeCheckoutSessionError(r, err, checkoutSessionErrorContext{})
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, err.Error())
	}
}
