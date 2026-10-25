package subscriptions

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/notify"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/notificationinbox"
)

// inboxKey selects deliberate notices, independently of the retained event log.
func inboxKey(n *models.NotificationEvent) (string, bool) {
	kind := string(n.EventType)
	switch {
	case notificationinbox.IsPaymentIssue(kind):
		if n.Data.Source == "customer_checkout" || n.Data.Source == "customer_subscription_change" {
			return "", false
		}
	case n.EventType == models.NotificationPremiumEnded:
		if !n.Data.AccessEnded {
			return "", false
		}
	case n.EventType == models.NotificationPremiumRenewed:
		if !n.Data.DowngradeApplied {
			return "", false
		}
		return "event:" + n.ID.String(), true
	default:
		return "", false
	}
	if !n.Data.SubscriptionID.IsZero() {
		return subscriptionIssue(n.Data.SubscriptionID.UUID(), kind), true
	}
	if !n.Data.OrderID.IsZero() {
		return orderIssue(n.Data.OrderID.UUID(), kind), true
	}
	return "event:" + n.ID.String(), true
}

func subscriptionIssue(id uuid.UUID, kind string) string {
	return "subscription:" + id.String() + ":" + kind
}
func orderIssue(id uuid.UUID, kind string) string { return "order:" + id.String() + ":" + kind }

func publishInbox(ctx context.Context, d *db.DB, tx pgx.Tx, n *models.NotificationEvent) error {
	if d.Notifications() == nil {
		return nil
	}
	if !n.Data.SubscriptionID.IsZero() {
		switch n.EventType {
		case models.NotificationPremiumStarted, models.NotificationPremiumRenewed:
			if err := withdrawRestoredSubscription(ctx, d, n.CustomerID, n.Data.SubscriptionID.UUID(), n.CreatedAt); err != nil {
				return err
			}

		}
	}
	key, selected := inboxKey(n)
	if !selected {
		return nil
	}
	channel, recipient, err := notificationinbox.Recipient(ctx, d, n.CustomerID)
	if err != nil || channel == nil {
		return err
	}
	data, err := json.Marshal(n.Data)
	if err != nil {
		return err
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	revision, err := issueRevision(ctx, tx, mid.UUID(), n.CustomerID, key, false, n.ID)
	if err != nil {
		return err
	}
	in := notify.Input{Recipient: recipient, Origin: notify.Origin{System: "openrails", Scope: mid.String(), ID: key},
		Kind: string(n.EventType), Data: data, CreatedAt: n.CreatedAt, Visibility: notify.VisibilityTenant,
		ActionRequested: billing.CustomerNotificationActionRequested(string(n.EventType))}
	return channel.Sink.ApplyTx(ctx, tx, []notify.Mutation{{Input: in, Revision: revision}})
}

// A new subscription may restore the same product that an older one lost.
// Select those identities from authoritative billing rows, never the inbox.
func withdrawRestoredSubscription(ctx context.Context, d *db.DB, customerID, subscriptionID uuid.UUID, at time.Time) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	var active bool
	err = d.Qx(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.subscriptions
WHERE merchant_id=$1 AND customer_id=$2 AND id=$3 AND status='active')`, mid.UUID(), customerID, subscriptionID).Scan(&active)
	if err != nil || !active {
		return err
	}
	if err := WithdrawSubscriptionPaymentNotices(ctx, d, customerID, subscriptionID); err != nil {
		return err
	}
	rows, err := d.Qx(ctx).Query(ctx, `SELECT prior.id FROM billing.subscriptions prior
JOIN billing.subscriptions current ON current.merchant_id=prior.merchant_id
 AND current.customer_id=prior.customer_id AND current.product_id=prior.product_id
WHERE current.merchant_id=$1 AND current.customer_id=$2 AND current.id=$3
AND EXISTS(SELECT 1 FROM billing.product_access access WHERE access.merchant_id=prior.merchant_id
 AND access.customer_id=prior.customer_id AND access.product_id=prior.product_id
 AND access.deleted_at IS NULL AND access.starts_at<=$4
 AND LEAST(COALESCE(access.ends_at,'infinity'),COALESCE(access.revoked_at,'infinity'))>$4)
ORDER BY prior.id`, mid.UUID(), customerID, subscriptionID, at)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, subscriptionIssue(id, "premium_ended"))
	}
	return notificationinbox.Withdraw(ctx, d, customerID, keys)
}

// issueRevision serializes all producers of one exact issue, even if their
// domain transitions lock different source rows. Repeated withdrawals reuse the
// same tombstone; new source facts advance the revision and may reopen it.
func issueRevision(ctx context.Context, tx pgx.Tx, merchantID, customerID uuid.UUID, key string, withdrawn bool, sourceEvent uuid.UUID) (int64, error) {
	if err := notificationinbox.RequireOrdering(ctx, tx, merchantID); err != nil {
		return 0, err
	}
	var event *uuid.UUID
	if sourceEvent != uuid.Nil {
		event = &sourceEvent
	}
	var revision int64
	err := tx.QueryRow(ctx, `INSERT INTO billing.notification_issue_revisions AS current
(merchant_id,customer_id,issue_key,revision,withdrawn,source_event) VALUES($1,$2,$3,1,$4,$5)
ON CONFLICT(merchant_id,customer_id,issue_key) DO UPDATE SET
revision=CASE WHEN current.withdrawn=EXCLUDED.withdrawn AND current.source_event IS NOT DISTINCT FROM EXCLUDED.source_event THEN current.revision ELSE current.revision+1 END,
withdrawn=EXCLUDED.withdrawn,source_event=EXCLUDED.source_event RETURNING revision`, merchantID, customerID, key, withdrawn, event).Scan(&revision)
	return revision, err
}

// WithdrawSubscriptionPaymentNotices follows confirmed payment or reinstatement.
// The caller has already updated the subscription in the same transaction.
func WithdrawSubscriptionPaymentNotices(ctx context.Context, d *db.DB, customerID, subscriptionID uuid.UUID) error {
	keys := make([]string, 0, len(notificationinbox.PaymentIssueKinds()))
	for _, kind := range notificationinbox.PaymentIssueKinds() {
		keys = append(keys, subscriptionIssue(subscriptionID, kind))
	}
	return notificationinbox.Withdraw(ctx, d, customerID, keys)
}

// WithdrawMethodNotice follows an accepted replacement, without asserting that
// any remaining outstanding balance has been paid.
func WithdrawMethodNotice(ctx context.Context, d *db.DB, customerID, subscriptionID uuid.UUID) error {
	return notificationinbox.Withdraw(ctx, d, customerID, []string{subscriptionIssue(subscriptionID, "payment_method_update_required")})
}

// WithdrawOrderPaymentNotices follows settled payment for this exact order.
func WithdrawOrderPaymentNotices(ctx context.Context, d *db.DB, customerID, orderID uuid.UUID) error {
	keys := make([]string, 0, len(notificationinbox.PaymentIssueKinds()))
	for _, kind := range notificationinbox.PaymentIssueKinds() {
		keys = append(keys, orderIssue(orderID, kind))
	}
	return notificationinbox.Withdraw(ctx, d, customerID, keys)
}
