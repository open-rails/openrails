package webhooks

import (
	"context"

	log "github.com/sirupsen/logrus"
)

func (s *CCBillWebhookService) logBillingError(ctx context.Context, err *BillingError, additionalFields log.Fields) {
	fields := log.Fields{
		"error_type":    err.Type,
		"error_message": err.Message,
		"error_context": err.Context,
	}

	for k, v := range additionalFields {
		fields[k] = v
	}

	log.WithContext(ctx).WithFields(fields).Error("Billing operation failed")
}

func newBillingError(errorType, message string, context map[string]interface{}, err error) *BillingError {
	return &BillingError{
		Type:    errorType,
		Message: message,
		Context: context,
		Err:     err,
	}
}
