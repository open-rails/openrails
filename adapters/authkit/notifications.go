package authkit

import (
	"context"
	"fmt"
	"slices"

	notifypg "github.com/open-rails/helpers/notify/postgres"
	"github.com/open-rails/openrails"
)

// Notifications connects an AuthKit account-inbox publisher to billing's
// optional channel. Native customers and explicitly allowed issuers use the
// same account UUID; other issuers are skipped. The destination publisher
// verifies that the account is live in its bound tenant. There is no email or
// display-name identity matching, or second directory read inside billing's tx.
func Notifications(sink notifypg.TxSink, issuers ...string) *openrails.NotificationChannel {
	allowed := slices.Clone(issuers)
	return &openrails.NotificationChannel{Sink: sink, Recipient: func(_ context.Context, source openrails.NotificationRecipient) (string, bool, error) {
		if source.Issuer != "" && !slices.Contains(allowed, source.Issuer) {
			return "", false, nil
		}
		if source.MerchantID.IsZero() || source.CustomerID.IsZero() {
			return "", false, fmt.Errorf("notification recipient requires merchant and customer identity")
		}
		return source.CustomerID.UUID().String(), true, nil
	}}
}
