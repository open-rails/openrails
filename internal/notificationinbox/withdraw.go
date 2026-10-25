// Package notificationinbox shares exact account-inbox withdrawals between
// subscription, order and credit settlement paths. It never reads the recipient inbox.
package notificationinbox

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit/notify"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// PaymentIssueKinds returns the deliberately selected actionable billing kinds.
func PaymentIssueKinds() []string {
	return []string{"payment_method_failed", "payment_method_update_required", "payment_authentication_required", "order_collection_stopped"}
}

func IsPaymentIssue(kind string) bool { return slices.Contains(PaymentIssueKinds(), kind) }

func Recipient(ctx context.Context, d *db.DB, customer uuid.UUID) (*config.NotificationChannel, notify.Recipient, error) {
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

func RequireOrdering(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID) error {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.notification_channel_state WHERE merchant_id=$1 AND restore_required)`, merchantID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("account notifications require explicit ordering-state restoration after the billing archive restore")
	}
	return nil
}

func Withdraw(ctx context.Context, d *db.DB, customerID uuid.UUID, keys []string) error {
	if d.Notifications() == nil || len(keys) == 0 {
		return nil
	}
	keys = slices.Clone(keys)
	slices.Sort(keys) // All writers take exact issue locks in the same order.
	keys = slices.Compact(keys)
	return d.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		txdb := d.NewWithPgxTx(tx)
		channel, recipient, err := Recipient(ctx, txdb, customerID)
		if err != nil || channel == nil {
			return err
		}
		mid, err := merchant.Require(ctx)
		if err != nil {
			return err
		}
		if err := RequireOrdering(ctx, tx, mid.UUID()); err != nil {
			return err
		}
		for batch := range slices.Chunk(keys, notify.MaxMutations) {
			withdrawals := make([]notify.Withdrawal, 0, len(batch))
			for _, key := range batch {
				// A delivered or queued publication always created this source
				// row in the same transaction. Ordinary successful payments must
				// not enqueue withdrawals for notices that never existed.
				var revision int64
				err := tx.QueryRow(ctx, `UPDATE billing.notification_issue_revisions
SET revision=revision+1,withdrawn=true,source_event=NULL
WHERE merchant_id=$1 AND customer_id=$2 AND issue_key=$3 AND NOT withdrawn RETURNING revision`,
					mid.UUID(), customerID, key).Scan(&revision)
				if errors.Is(err, pgx.ErrNoRows) {
					continue
				}
				if err != nil {
					return fmt.Errorf("withdraw notification %s: %w", key, err)
				}
				withdrawals = append(withdrawals, notify.Withdrawal{RecipientID: recipient.ID, ID: key, Revision: revision})
			}
			if len(withdrawals) == 0 {
				continue
			}
			if err := channel.Sink.Withdraw(ctx, withdrawals, tx); err != nil {
				return err
			}
		}
		return nil
	})
}

// WithdrawOrder follows a settled, canceled, expired or void exact order.
// No mutation is queued when that order has never emitted an inbox issue.
func WithdrawOrder(ctx context.Context, d *db.DB, customerID, orderID uuid.UUID) error {
	keys := make([]string, 0, 4)
	for _, kind := range PaymentIssueKinds() {
		keys = append(keys, "order:"+orderID.String()+":"+kind)
	}
	return Withdraw(ctx, d, customerID, keys)
}
