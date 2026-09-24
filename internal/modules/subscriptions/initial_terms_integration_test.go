//go:build integration

package subscriptions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/stretchr/testify/require"
)

func TestAcceptedInitialMembershipUsesFrozenTermsAtomically(t *testing.T) {
	for _, mode := range []string{"paid", "free", "pending", "engine"} {
		t.Run(mode, func(t *testing.T) {
			f := newFailopenFixture(t, 720, true)
			ctx := f.ctx()
			now := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(time.Microsecond)
			terms := InitialMembershipTerms{CollectionPolicy: models.CollectionPolicyProvider, SubscriptionID: uuid.New(), PaymentID: uuid.New(), CustomerID: uuid.MustParse(f.userID), PSPID: f.pspID, ProductID: f.productID, PriceID: f.priceID, PaymentMethodID: uuid.New(), ProductName: "accepted product", Amount: 9_990_000, RecurringAmount: 9_990_000, Currency: "USD", AcceptedAt: now, PeriodStart: now, PeriodEnd: now.Add(720 * time.Hour), Entitlements: map[string]*int{f.ent: nil}}
			transaction := "initial-" + uuid.NewString()
			if mode != "paid" && mode != "engine" {
				terms.Amount = 0
				terms.PaymentID = uuid.Nil
				transaction = ""
			}
			if mode == "free" {
				terms.RecurringAmount = 0
			}
			if mode == "pending" {
				terms.Pending = true
				terms.PeriodStart = now.Add(72 * time.Hour)
				terms.PeriodEnd = terms.PeriodStart.Add(720 * time.Hour)
			}
			require.NoError(t, paymentmethods.NewPaymentMethodRepo(f.dbi).Create(ctx, &models.PaymentMethod{ID: terms.PaymentMethodID, CustomerID: terms.CustomerID, Rail: models.RailNMI, PspID: f.pspID, Custodian: "psp", RailCustomerRef: "vault-" + uuid.NewString()}))
			_, err := f.pool.Exec(ctx, `UPDATE billing.products SET entitlements_spec='{"changed":null}', archived=true WHERE id=$1`, f.productID)
			require.NoError(t, err)
			_, err = f.pool.Exec(ctx, `UPDATE billing.prices SET amount=25000000, access_duration_hours=24, archived=true WHERE id=$1`, f.priceID)
			require.NoError(t, err)
			providerSub := "accepted-" + uuid.NewString()
			if mode == "engine" {
				terms.CollectionPolicy = models.CollectionPolicyEngine
				providerSub = ""
			}
			params := &CreateMembershipParams{Prepared: &terms, UserID: f.userID, PriceID: f.priceID, Rail: models.RailNMI, RailSubscriptionID: &providerSub, TransactionID: transaction, PurchasedAt: &now}
			rollback := errors.New("caller terminal decision could not commit")
			apply := func(abort bool) error {
				return f.dbi.MerchantTx(ctx, func(txctx context.Context, tx pgx.Tx) error {
					sub, notices, err := f.lifecycle.CreateMembershipTx(txctx, db.NewWithPgxTx(tx), params)
					if err != nil {
						return err
					}
					require.Equal(t, terms.SubscriptionID, sub.ID)
					require.Equal(t, terms.CollectionPolicy, sub.CollectionPolicy)
					require.Equal(t, terms.Entitlements, sub.EntitlementsSpecSnapshot)
					if mode == "pending" {
						require.Equal(t, models.StatusPending, sub.Status)
						require.Empty(t, notices)
						require.Nil(t, sub.CurrentPeriodStartsAt)
					} else {
						require.Equal(t, models.StatusActive, sub.Status)
						require.Len(t, notices, 1)
						require.WithinDuration(t, terms.PeriodEnd, *sub.CurrentPeriodEndsAt, time.Microsecond)
					}
					if abort {
						return rollback
					}
					return nil
				})
			}
			require.ErrorIs(t, apply(true), rollback)
			var count int
			for _, table := range []string{"subscriptions", "payments", "notifications", "entitlements"} {
				require.NoError(t, f.pool.QueryRow(ctx, "SELECT count(*) FROM billing."+table+" WHERE customer_id=$1", terms.CustomerID).Scan(&count))
				require.Zero(t, count, "caller rollback removes all %s effects", table)
			}
			require.NoError(t, apply(false))
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.payments WHERE subscription_id=$1`, terms.SubscriptionID).Scan(&count))
			if mode == "paid" || mode == "engine" {
				require.Equal(t, 1, count)
				var id uuid.UUID
				var amount int64
				require.NoError(t, f.pool.QueryRow(ctx, `SELECT id,amount FROM billing.payments WHERE subscription_id=$1`, terms.SubscriptionID).Scan(&id, &amount))
				require.Equal(t, terms.PaymentID, id)
				require.Equal(t, terms.Amount, amount)
			} else {
				require.Zero(t, count)
			}
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements WHERE source_id=$1 AND entitlement=$2 AND source_type='subscription'`, terms.SubscriptionID, f.ent).Scan(&count))
			if mode == "pending" {
				require.Zero(t, count)
			} else {
				require.Equal(t, 1, count)
			}
			var allowance int
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements WHERE source_id=$1 AND entitlement=$2 AND source_type='grace'`, terms.SubscriptionID, f.ent).Scan(&allowance))
			if mode == "engine" {
				require.Equal(t, 1, allowance, "an engine period carries its renewal allowance")
			} else {
				require.Zero(t, allowance)
			}
			if mode == "pending" {
				paid := terms
				paid.Pending = false
				paid.Amount = paid.RecurringAmount
				paid.PaymentID = uuid.New()
				f.lifecycle.SetClock(clockwork.NewFakeClockAt(paid.PeriodStart.Add(time.Minute)))
				first := *params
				first.Prepared = &paid
				first.TransactionID = "first-paid-" + uuid.NewString()
				first.PurchasedAt = &paid.PeriodStart
				require.NoError(t, f.dbi.MerchantTx(ctx, func(txctx context.Context, tx pgx.Tx) error {
					sub, notices, err := f.lifecycle.CreateMembershipTx(txctx, db.NewWithPgxTx(tx), &first)
					if err != nil {
						return err
					}
					require.Equal(t, terms.SubscriptionID, sub.ID)
					require.Equal(t, models.StatusActive, sub.Status)
					require.Len(t, notices, 1)
					require.WithinDuration(t, terms.PeriodStart, *sub.CurrentPeriodStartsAt, time.Microsecond)
					require.WithinDuration(t, terms.PeriodEnd, *sub.CurrentPeriodEndsAt, time.Microsecond)
					return nil
				}))
				// Replaying the original no-charge enrollment never reinterprets its
				// already-active membership as a new charge or duplicates its first event.
				require.NoError(t, f.dbi.MerchantTx(ctx, func(txctx context.Context, tx pgx.Tx) error {
					sub, notices, err := f.lifecycle.CreateMembershipTx(txctx, db.NewWithPgxTx(tx), params)
					if err != nil {
						return err
					}
					require.Equal(t, models.StatusActive, sub.Status)
					require.Empty(t, notices)
					return nil
				}))
			} else {
				require.NoError(t, f.lifecycle.CancelMembership(ctx, &CancelMembershipParams{SubscriptionID: &terms.SubscriptionID, CancelType: models.CancelTypeUser, RevokeAccess: true}))
				_, err := f.pool.Exec(ctx, `UPDATE billing.subscriptions SET deleted_at=now() WHERE id=$1`, terms.SubscriptionID)
				require.NoError(t, err)
				require.NoError(t, f.dbi.MerchantTx(ctx, func(txctx context.Context, tx pgx.Tx) error {
					sub, notices, err := f.lifecycle.CreateMembershipTx(txctx, db.NewWithPgxTx(tx), params)
					if err != nil {
						return err
					}
					require.Equal(t, models.StatusCancelled, sub.Status)
					var tombstoned bool
					require.NoError(t, tx.QueryRow(txctx, `SELECT deleted_at IS NOT NULL FROM billing.subscriptions WHERE id=$1`, sub.ID).Scan(&tombstoned))
					require.True(t, tombstoned, "accepted replay must preserve historical tombstone")
					require.Empty(t, notices)
					return nil
				}))
				var active int
				require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements WHERE source_id=$1 AND revoked_at IS NULL AND deleted_at IS NULL`, terms.SubscriptionID).Scan(&active))
				require.Zero(t, active, "accepted replay preserves later revocation")
			}

		})
	}
}
