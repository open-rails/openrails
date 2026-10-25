package subscriptions

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/notify"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

var paymentIssueKinds = []string{
	"payment_method_failed", "payment_method_update_required", "payment_authentication_required", "order_collection_stopped",
}

// inboxKey selects deliberate notices, independently of the retained event log.
func inboxKey(n *models.NotificationEvent) (string, bool) {
	kind := string(n.EventType)
	switch {
	case slices.Contains(paymentIssueKinds, kind):
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

func inboxRecipient(ctx context.Context, d *db.DB, customer uuid.UUID) (*config.NotificationChannel, notify.Recipient, error) {
	channel := d.Notifications()
	if channel == nil {
		return nil, notify.Recipient{}, nil
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, notify.Recipient{}, err
	}
	row, err := d.Gen(ctx).GetCustomer(ctx, gen.GetCustomerParams{MerchantID: mid.UUID(), ID: customer})
	if err != nil {
		return nil, notify.Recipient{}, err
	}
	issuer := ""
	if row.Issuer != nil {
		issuer = *row.Issuer
	}
	id, deliver, err := channel.Recipient(ctx, config.NotificationRecipient{MerchantID: mid, CustomerID: billing.CustomerID(customer), Issuer: issuer})
	if err != nil || !deliver {
		return nil, notify.Recipient{}, err
	}
	r := notify.Recipient{Audience: notify.AudienceUser, ID: id}
	if err := notify.ValidateRecipient(r); err != nil {
		return nil, notify.Recipient{}, err
	}
	return channel, r, nil
}

func publishInbox(ctx context.Context, d *db.DB, tx pgx.Tx, n *models.NotificationEvent) error {
	if d.Notifications() == nil {
		return nil
	}
	if (n.EventType == models.NotificationPremiumStarted || n.EventType == models.NotificationPremiumRenewed) && !n.Data.SubscriptionID.IsZero() {
		if err := withdrawRestoredSubscription(ctx, d, n.CustomerID, n.Data.SubscriptionID.UUID(), n.CreatedAt); err != nil {
			return err
		}
	}
	key, selected := inboxKey(n)
	if !selected {
		return nil
	}
	channel, recipient, err := inboxRecipient(ctx, d, n.CustomerID)
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
	return withdrawIssues(ctx, d, customerID, keys)
}

// issueRevision serializes all producers of one exact issue, even if their
// domain transitions lock different source rows. Repeated withdrawals reuse the
// same tombstone; new source facts advance the revision and may reopen it.
func issueRevision(ctx context.Context, tx pgx.Tx, merchantID, customerID uuid.UUID, key string, withdrawn bool, sourceEvent uuid.UUID) (int64, error) {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.notification_channel_state WHERE merchant_id=$1 AND restore_required)`, merchantID).Scan(&pending); err != nil {
		return 0, err
	}
	if pending {
		return 0, fmt.Errorf("account notifications require explicit ordering-state restoration after the billing archive restore")
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

func withdrawIssues(ctx context.Context, d *db.DB, customerID uuid.UUID, keys []string) error {
	if d.Notifications() == nil || len(keys) == 0 {
		return nil
	}
	keys = slices.Clone(keys)
	slices.Sort(keys) // All writers take exact issue locks in the same order.
	keys = slices.Compact(keys)
	return d.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := d.NewWithPgxTx(tx)
		channel, recipient, err := inboxRecipient(ctx, txdb, customerID)
		if err != nil || channel == nil {
			return err
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		for batch := range slices.Chunk(keys, notify.MaxMutations) {
			mutations := make([]notify.Mutation, 0, len(batch))
			for _, key := range batch {
				revision, err := issueRevision(ctx, tx, mid.UUID(), customerID, key, true, uuid.Nil)
				if err != nil {
					return fmt.Errorf("withdraw notification %s: %w", key, err)
				}
				mutations = append(mutations, notify.Mutation{Input: notify.Input{Recipient: recipient,
					Origin: notify.Origin{System: "openrails", Scope: mid.String(), ID: key}, Visibility: notify.VisibilityTenant}, Revision: revision, Withdrawn: true})
			}
			if err := channel.Sink.ApplyTx(ctx, tx, mutations); err != nil {
				return err
			}
		}
		return nil
	})
}

// WithdrawSubscriptionPaymentNotices follows confirmed payment or reinstatement.
// The caller has already updated the subscription in the same transaction.
func WithdrawSubscriptionPaymentNotices(ctx context.Context, d *db.DB, customerID, subscriptionID uuid.UUID) error {
	keys := make([]string, 0, len(paymentIssueKinds))
	for _, kind := range paymentIssueKinds {
		keys = append(keys, subscriptionIssue(subscriptionID, kind))
	}
	return withdrawIssues(ctx, d, customerID, keys)
}

// WithdrawMethodNotice follows an accepted replacement, without asserting that
// any remaining outstanding balance has been paid.
func WithdrawMethodNotice(ctx context.Context, d *db.DB, customerID, subscriptionID uuid.UUID) error {
	return withdrawIssues(ctx, d, customerID, []string{subscriptionIssue(subscriptionID, "payment_method_update_required")})
}

// WithdrawOrderPaymentNotices follows settled payment for this exact order.
func WithdrawOrderPaymentNotices(ctx context.Context, d *db.DB, customerID, orderID uuid.UUID) error {
	keys := make([]string, 0, len(paymentIssueKinds))
	for _, kind := range paymentIssueKinds {
		keys = append(keys, orderIssue(orderID, kind))
	}
	return withdrawIssues(ctx, d, customerID, keys)
}
