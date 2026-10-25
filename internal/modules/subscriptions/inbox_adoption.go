package subscriptions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/helpers/notify"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
)

// ExportNotificationAdoption selects exact legacy origins from the producer's
// journal. Stop the old importer and business writers for the whole cutover;
// the table locks below protect this snapshot, not later destination calls.
func ExportNotificationAdoption(ctx context.Context, d *db.DB, id billing.MerchantID, now time.Time) (billing.NotificationAdoption, error) {
	out := billing.NotificationAdoption{MerchantID: id}
	if d.Notifications() == nil {
		return out, errors.New("notification adoption requires the destination recipient mapping")
	}
	ctx = merchant.WithID(ctx, id)
	err := d.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := requireNotificationOrdering(ctx, tx, id.UUID()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `LOCK TABLE billing.notification_events,billing.notification_issue_revisions,
billing.subscriptions,billing.orders,billing.product_access IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		txdb := d.NewWithPgxTx(tx)
		rows, err := tx.Query(ctx, `SELECT id,customer_id,event_type,data,created_at FROM billing.notification_events
WHERE merchant_id=$1 AND event_type=ANY($2) ORDER BY created_at DESC,id DESC`, id.UUID(),
			[]string{"payment_method_failed", "payment_method_update_required", "payment_authentication_required", "order_collection_stopped", "premium_ended", "premium_renewed"})
		if err != nil {
			return err
		}
		facts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (*models.NotificationEvent, error) {
			n := &models.NotificationEvent{}
			var data []byte
			if err := row.Scan(&n.ID, &n.CustomerID, &n.EventType, &data, &n.CreatedAt); err != nil {
				return nil, err
			}
			return n, json.Unmarshal(data, &n.Data)
		})
		if err != nil {
			return err
		}
		type group struct {
			latest   *models.NotificationEvent
			previous []notify.Origin
			key      string
		}
		groups := map[string]*group{}
		for _, n := range facts {
			key, selected := inboxKey(n)
			if !selected || strings.HasPrefix(key, "event:") {
				continue // Historical receipts retain their existing identity and state.
			}
			identity := n.CustomerID.String() + "/" + key
			g := groups[identity]
			if g == nil {
				g = &group{latest: n, key: key}
				groups[identity] = g
			}
			g.previous = append(g.previous, notify.Origin{System: "openrails", Scope: id.String(), ID: "ntf_" + n.ID.String()})
		}
		identities := make([]string, 0, len(groups))
		for identity := range groups {
			identities = append(identities, identity)
		}
		slices.Sort(identities)
		for _, identity := range identities {
			g := groups[identity]
			channel, recipient, err := inboxRecipient(ctx, txdb, g.latest.CustomerID)
			if err != nil {
				return err
			}
			if channel == nil {
				continue
			}
			withdrawn, err := notificationResolved(ctx, txdb, g.latest, now)
			if err != nil {
				return err
			}
			event := g.latest.ID
			if withdrawn {
				event = uuid.Nil
			}
			revision, err := issueRevision(ctx, tx, id.UUID(), g.latest.CustomerID, g.key, withdrawn, event)
			if err != nil {
				return err
			}
			data, err := json.Marshal(g.latest.Data)
			if err != nil {
				return err
			}
			current := notify.Mutation{Input: notify.Input{Recipient: recipient,
				Origin: notify.Origin{System: "openrails", Scope: id.String(), ID: g.key},
				Kind:   string(g.latest.EventType), Data: data, CreatedAt: g.latest.CreatedAt,
				Visibility: notify.VisibilityTenant, ActionRequested: billing.CustomerNotificationActionRequested(string(g.latest.EventType))},
				Revision: revision, Withdrawn: withdrawn}
			out.Adoptions = append(out.Adoptions, notify.Adoption{Previous: g.previous, Current: current})
		}
		out.Ordering, err = notificationOrdering(ctx, tx, id)
		return err
	})
	return out, err
}

func notificationResolved(ctx context.Context, d *db.DB, n *models.NotificationEvent, now time.Time) (bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	if !n.Data.SubscriptionID.IsZero() {
		if n.EventType == models.NotificationPremiumEnded {
			access, err := d.Gen(ctx).SubscriptionNotificationAccess(ctx, gen.SubscriptionNotificationAccessParams{
				MerchantID: mid.UUID(), CustomerID: n.CustomerID, SubscriptionID: n.Data.SubscriptionID.UUID().String(), AtTime: now})
			return access.Retained, err
		}
		sub, err := NewSubscriptionRepo(d).GetByID(ctx, n.Data.SubscriptionID.UUID())
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if n.EventType == models.NotificationPaymentMethodUpdateRequired {
			return sub.Status != models.StatusAwaitingMethod && sub.PaymentMethod != nil && sub.PaymentMethod.Chargeable(), nil
		}
		return sub.Status != models.StatusPastDue && sub.Status != models.StatusAwaitingMethod, nil
	}
	if !n.Data.OrderID.IsZero() {
		var status string
		err := d.Qx(ctx).QueryRow(ctx, `SELECT status FROM billing.orders WHERE merchant_id=$1 AND customer_id=$2 AND id=$3`, mid.UUID(), n.CustomerID, n.Data.OrderID.UUID()).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return status == "complete" || status == "canceled" || status == "expired" || status == "void", err
	}
	return false, nil
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
func RestoreNotificationOrdering(ctx context.Context, d *db.DB, id billing.MerchantID, snapshot billing.NotificationAdoption) error {
	if snapshot.MerchantID != id {
		return errors.New("notification ordering snapshot belongs to another merchant")
	}
	ctx = merchant.WithID(ctx, id)
	return d.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
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
