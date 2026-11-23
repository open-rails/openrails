package webhooks

import (
	"context"
)

func (s *CCBillWebhookService) logBillingError(ctx context.Context, err *BillingError, additionalFields []any) {
	fields := []any{"error_type", err.Type, "error_message", err.Message, "error_context", err.Context}

	for k, v := range additionalFields {
		fields = append(fields, k, v)
	}

	s.Log.With(fields...).ErrorContext(ctx, "Billing operation failed")
}

func newBillingError(errorType, message string, context map[string]interface{}, err error) *BillingError {
	return &BillingError{
		Type:    errorType,
		Message: message,
		Context: context,
		Err:     err,
	}
}
