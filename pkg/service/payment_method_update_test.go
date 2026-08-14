package service

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/modules/paymentmethods"
)

func TestPaymentMethodUpdateFacadeError(t *testing.T) {
	t.Run("processing remains inspectable", func(t *testing.T) {
		err := paymentMethodUpdateFacadeError(fmt.Errorf("update: %w", paymentmethods.ErrPaymentMethodUpdateProcessing))
		require.ErrorIs(t, err, ErrPaymentMethodUpdateProcessing)
	})

	t.Run("retokenize remains inspectable", func(t *testing.T) {
		err := paymentMethodUpdateFacadeError(fmt.Errorf("update: %w", paymentmethods.ErrPaymentMethodRetokenize))
		require.ErrorIs(t, err, ErrPaymentMethodRetokenize)
	})

	t.Run("terminal reason crosses the facade", func(t *testing.T) {
		err := paymentMethodUpdateFacadeError(&paymentmethods.PaymentMethodUpdateFailedError{Reason: "provider state conflict"})
		var terminal *PaymentMethodUpdateFailedError
		require.ErrorAs(t, err, &terminal)
		require.Equal(t, "provider state conflict", terminal.Reason)
	})

	t.Run("validation detail crosses the facade", func(t *testing.T) {
		err := paymentMethodUpdateFacadeError(&paymentmethods.PaymentMethodUpdateValidationError{Message: "last_four is required"})
		var validation *PaymentMethodUpdateValidationError
		require.ErrorAs(t, err, &validation)
		require.Equal(t, "last_four is required", validation.Message)
	})

	t.Run("unrelated errors pass through", func(t *testing.T) {
		original := errors.New("database unavailable")
		require.Same(t, original, paymentMethodUpdateFacadeError(original))
	})
}
