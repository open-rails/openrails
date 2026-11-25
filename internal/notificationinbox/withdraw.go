// Package notificationinbox shares exact account-inbox withdrawals between
// subscription, invoice and credit settlement paths. It never reads the recipient inbox.
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
	return []string{"payment_method_failed", "payment_method_update_required", "payment_authentication_required", "invoice_collection_stopped"}
}

func IsPaymentIssue(kind string) bool { return slices.Contains(PaymentIssueKinds(), kind) }

// IssueKey is the exact inbox issue of one subject and kind. Recipients hold
// issues keyed before invoices were named, so an invoice keeps the "order"
// subject and its stopped collection the "order_collection_stopped" kind.
func IssueKey(subject string, id uuid.UUID, kind string) string {
	if subject == "invoice" {
		subject = "order"
	}
	if kind == "invoice_collection_stopped" {
		kind = "order_collection_stopped"
	}
	return subject + ":" + id.String() + ":" + kind
}

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

// RequireOrdering admits one producer change to the merchant's notification
// ordering: it shares the merchant's ordering lock until the transaction ends
// and refuses while a billing archive restore awaits the ordering snapshot.
func RequireOrdering(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID) error {
	if err := gen.New(tx).ShareNotificationOrdering(ctx, merchantID); err != nil {
		return err
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM billing.notification_channel_state WHERE merchant_id=$1 AND restore_required)`, merchantID).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return fmt.Errorf("account notifications require explicit ordering-state restoration after the billing archive restore")
	}
	return nil
}

// LockOrdering holds the merchant's notification ordering alone until the
// transaction ends: none of its producers commits meanwhile, and no other
// merchant waits.
func LockOrdering(ctx context.Context, tx pgx.Tx, merchantID uuid.UUID) error {
	return gen.New(tx).LockNotificationOrdering(ctx, merchantID)
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

// WithdrawInvoice follows a settled, canceled, expired or void exact invoice.
// No mutation is queued when that invoice has never emitted an inbox issue.
func WithdrawInvoice(ctx context.Context, d *db.DB, customerID, invoiceID uuid.UUID) error {
	keys := make([]string, 0, 4)
	for _, kind := range PaymentIssueKinds() {
		keys = append(keys, IssueKey("invoice", invoiceID, kind))
	}
	return Withdraw(ctx, d, customerID, keys)
}
