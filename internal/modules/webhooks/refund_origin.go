package webhooks

import "github.com/open-rails/openrails/internal/db/models"

// refundDecidedByOpenRails reports a refund OpenRails itself issued. Its
// access and membership effects were applied with the merchant's explicit
// revoke_access choice when it completed; the provider's notification of the
// same refund must not apply a second, different decision.
func refundDecidedByOpenRails(refund *models.Payment) bool {
	if refund == nil {
		return false
	}
	_, ok := refund.Metadata["admin_refund_idempotency_key"]
	return ok
}

// refundInitiatedByOpenRails reports whether OpenRails asked for the refund:
// a staff refund or one of its automatic refunds, each marked when reserved.
func refundInitiatedByOpenRails(refund *models.Payment) bool {
	if refund == nil {
		return false
	}
	for _, key := range []string{"admin_refund_idempotency_key", "charge_after_cancel", "fraud_warning", "order_late_payment"} {
		if _, ok := refund.Metadata[key]; ok {
			return true
		}
	}
	return false
}
