package authkit

import (
	"context"
	"fmt"
	"slices"

	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
)

// Notifications connects an AuthKit account-inbox publisher to billing's
// optional channel for one explicit merchant. Other merchants are skipped
// before reaching a publisher bound to this merchant's source scope.
// Native customers and explicitly allowed issuers use the
// same account UUID; other issuers are skipped. The destination publisher
// verifies that the account is live in its bound tenant. There is no email or
// display-name identity matching, or second directory read inside billing's tx.
func Notifications(sink notify.Publisher, merchant billing.MerchantID, issuers ...string) *openrails.NotificationChannel {
	allowed := slices.Clone(issuers)
	return &openrails.NotificationChannel{Sink: sink, Recipient: func(_ context.Context, source openrails.NotificationRecipient) (string, bool, error) {
		if merchant.IsZero() || source.MerchantID.IsZero() {
			return "", false, fmt.Errorf("notification channel requires an explicit merchant binding")
		}
		if source.MerchantID != merchant {
			return "", false, nil
		}
		if source.Issuer != "" && !slices.Contains(allowed, source.Issuer) {
			return "", false, nil
		}
		if source.CustomerID.IsZero() {
			return "", false, fmt.Errorf("notification recipient requires merchant and customer identity")
		}
		return source.CustomerID.UUID().String(), true, nil
	}}
}
