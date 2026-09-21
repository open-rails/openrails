//go:build integration

package subscriptions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/stretchr/testify/require"
)

func TestAcceptedInitialMembershipUsesFrozenTermsAtomically(t *testing.T) {
	for _, mode := range []string{"paid", "free", "pending"} {
		t.Run(mode, func(t *testing.T) {
			f := newFailopenFixture(t, 720, true)
			ctx := f.ctx()
			now := time.Now().UTC().Truncate(time.Microsecond)
			terms := InitialMembershipTerms{SubscriptionID: uuid.New(), PaymentID: uuid.New(), CustomerID: uuid.MustParse(f.userID), PSPID: f.pspID, ProductID: f.productID, PriceID: f.priceID, PaymentMethodID: uuid.New(), ProductName: "accepted product", Amount: 9_990_000, RecurringAmount: 9_990_000, Currency: "USD", AcceptedAt: now, PeriodStart: now, PeriodEnd: now.Add(720 * time.Hour), Entitlements: map[string]*int{f.ent: nil}}
			transaction := "initial-" + uuid.NewString()
			if mode != "paid" {
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
			require.NoError(t, paymentmethods.NewPaymentMethodRepo(f.dbi).Create(ctx, &models.PaymentMethod{ID: terms.PaymentMethodID, CustomerID: terms.CustomerID, Rail: models.RailNMI, PspID: f.pspID, Custodian: "psp", RailCustomerRef: "vault-" + uuid.NewString(), RebillDriver: "provider"}))
			_, err := f.pool.Exec(ctx, `UPDATE billing.products SET entitlements_spec='{"changed":null}', archived=true WHERE id=$1`, f.productID)
			require.NoError(t, err)
			_, err = f.pool.Exec(ctx, `UPDATE billing.prices SET amount=25000000, access_duration_hours=24, archived=true WHERE id=$1`, f.priceID)
			require.NoError(t, err)
			providerSub := "accepted-" + uuid.NewString()
			params := &CreateMembershipParams{Prepared: &terms, UserID: f.userID, PriceID: f.priceID, Rail: models.RailNMI, RailSubscriptionID: &providerSub, TransactionID: transaction, PurchasedAt: &now}
			rollback := errors.New("caller terminal decision could not commit")
			apply := func(abort bool) error {
				return f.dbi.MerchantTx(ctx, func(txctx context.Context, tx pgx.Tx) error {
					sub, notices, err := f.lifecycle.CreateMembershipTx(txctx, db.NewWithPgxTx(tx), params)
					if err != nil {
						return err
					}
					require.Equal(t, terms.SubscriptionID, sub.ID)
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
			if mode == "paid" {
				require.Equal(t, 1, count)
				var id uuid.UUID
				var amount int64
				require.NoError(t, f.pool.QueryRow(ctx, `SELECT id,amount FROM billing.payments WHERE subscription_id=$1`, terms.SubscriptionID).Scan(&id, &amount))
				require.Equal(t, terms.PaymentID, id)
				require.Equal(t, terms.Amount, amount)
			} else {
				require.Zero(t, count)
			}
			require.NoError(t, f.pool.QueryRow(ctx, `SELECT count(*) FROM billing.entitlements WHERE source_id=$1 AND entitlement=$2`, terms.SubscriptionID, f.ent).Scan(&count))
			if mode == "pending" {
				require.Zero(t, count)
			} else {
				require.Equal(t, 1, count)
			}
		})
	}
}
