package payment

import (
	"errors"
	"fmt"
)

// ErrPaymentMethodStale is a purchase whose saved payment method can no longer
// be charged for this customer and processor: it is gone, not theirs, vaulted
// on another provider account, or its collection token expired. Nothing was
// charged; the card must be collected again.
var ErrPaymentMethodStale = errors.New("saved payment method can no longer be charged")

// ErrPaymentMethodRequired is a card purchase that names neither a saved
// method nor a new card token. The customer's default card is never implied.
var ErrPaymentMethodRequired = fmt.Errorf("%w: payment_method_id or payment_token is required", ErrInvoiceAttemptValidation)
