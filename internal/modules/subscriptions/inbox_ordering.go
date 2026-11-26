package subscriptions

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/notificationinbox"
)

// ExportNotificationOrdering snapshots the merchant's notification ordering
// for a billing-archive move. It holds the merchant's ordering alone, so no
// publication or withdrawal of the merchant interleaves and no other merchant
// waits, and refuses while a restore awaits its own snapshot.
func ExportNotificationOrdering(ctx context.Context, d *db.DB, id billing.MerchantID) (billing.NotificationOrderingSnapshot, error) {
	out := billing.NotificationOrderingSnapshot{MerchantID: id}
	ctx = merchant.WithID(ctx, id)
	err := d.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := notificationinbox.LockOrdering(ctx, tx, id.UUID()); err != nil {
			return err
		}
		if err := notificationinbox.RequireOrdering(ctx, tx, id.UUID()); err != nil {
			return err
		}
		var err error
		out.Ordering, err = notificationOrdering(ctx, tx, id)
		return err
	})
	return out, err
}

func notificationOrdering(ctx context.Context, tx pgx.Tx, id billing.MerchantID) ([]billing.NotificationOrdering, error) {
	rows, err := tx.Query(ctx, `SELECT customer_id,issue_key,revision,withdrawn,source_event
FROM billing.notification_issue_revisions WHERE merchant_id=$1 ORDER BY customer_id,issue_key`, id.UUID())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (billing.NotificationOrdering, error) {
		var record billing.NotificationOrdering
		return record, row.Scan(&record.CustomerID, &record.Key, &record.Revision, &record.Withdrawn, &record.SourceEvent)
	})
}

// RestoreNotificationOrdering merges an explicit source snapshot without ever
// decreasing a known revision. It is required after a financial archive restore
// before the same inbox can receive new messages from the relocated producer.
func RestoreNotificationOrdering(ctx context.Context, d *db.DB, id billing.MerchantID, snapshot billing.NotificationOrderingSnapshot) error {
	if snapshot.MerchantID != id {
		return errors.New("notification ordering snapshot belongs to another merchant")
	}
	ctx = merchant.WithID(ctx, id)
	return d.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := notificationinbox.LockOrdering(ctx, tx, id.UUID()); err != nil {
			return err
		}
		for _, item := range snapshot.Ordering {
			if item.CustomerID.IsZero() || item.Key == "" || item.Revision < 1 {
				return errors.New("invalid notification ordering record")
			}
			var revision int64
			err := tx.QueryRow(ctx, `INSERT INTO billing.notification_issue_revisions AS current
(merchant_id,customer_id,issue_key,revision,withdrawn,source_event) VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(merchant_id,customer_id,issue_key) DO UPDATE SET revision=EXCLUDED.revision,
withdrawn=EXCLUDED.withdrawn,source_event=EXCLUDED.source_event WHERE current.revision<EXCLUDED.revision
 OR (current.revision=EXCLUDED.revision AND current.withdrawn=EXCLUDED.withdrawn AND current.source_event IS NOT DISTINCT FROM EXCLUDED.source_event)
RETURNING revision`, id.UUID(), item.CustomerID.UUID(), item.Key, item.Revision, item.Withdrawn, item.SourceEvent).Scan(&revision)
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("notification ordering snapshot conflicts with newer source state for %s", item.Key)
			}
			if err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO billing.notification_channel_state(merchant_id,restore_required) VALUES($1,false)
ON CONFLICT(merchant_id) DO UPDATE SET restore_required=false`, id.UUID())
		return err
	})
}
