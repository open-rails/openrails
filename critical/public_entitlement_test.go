//go:build integration

package critical_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/failpoint"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/grants"
	"github.com/open-rails/openrails/internal/reconcile/converge"
	"github.com/stretchr/testify/require"
)

func publicGrant(t *testing.T, a *authorityWorld, key string, hours int) billing.ProductAccessGrant {
	t.Helper()
	product, err := a.alphaRemote.CreateProduct(t.Context(), billing.CreateProductParams{Key: key, DisplayName: key, Entitlements: []string{"library:read"}})
	require.NoError(t, err)
	grants, err := a.alphaRemote.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: key, Items: []billing.CreateProductAccessParams{{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), ProductID: product.ID, Hours: ptr(hours), Reason: billing.GrantReasonComp}}})
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, product.ID, grants[0].ProductID)
	require.Equal(t, billing.ProductAccessSourceGrant, grants[0].SourceType)
	return grants[0]
}

func publicPastClock(w *world, ago time.Duration) {
	// PostgreSQL's public timestamps have microsecond precision. Pin the
	// controlled input to that precision before creating any domain facts.
	w.clock.Advance(w.clock.Now().Truncate(time.Microsecond).Add(-ago).Sub(w.clock.Now()))
}

func publicAccess(t *testing.T, a *authorityWorld, at time.Time, expected bool) {
	t.Helper()
	check, err := a.alphaRemote.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: billing.CustomerID(uuid.MustParse(a.alice.ID)), Entitlements: []string{"library:read", "never:granted"}, At: at})
	require.NoError(t, err)
	require.Equal(t, expected, check.Entitlements["library:read"], at.String())
	require.False(t, check.Entitlements["never:granted"])
}

func TestPublicEntitlements(t *testing.T) {
	t.Run("OR-J27-S01/purchase-and-grant-overlap-counts-once", func(t *testing.T) {
		a, p := newPaymentWorld(t, func(w *world) { publicPastClock(w, 72*time.Hour) })
		_, price := publicSale(t, a, "purchased-day", 9_990_000, ptr(24))
		order := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "overlap-purchase")
		start := a.clock.Now()
		a.clock.Advance(12 * time.Hour)
		grant := publicGrant(t, a, "overlapping-comp", 24)
		require.Equal(t, start.Add(12*time.Hour), grant.StartsAt)
		require.Equal(t, start.Add(36*time.Hour), *grant.EndsAt)
		for _, hours := range []int{0, 12, 24, 35} {
			publicAccess(t, a, start.Add(time.Duration(hours)*time.Hour), true)
		}
		publicAccess(t, a, start.Add(36*time.Hour), false)
		a.clock.Advance(24 * time.Hour)
		tenure, err := a.alphaRemote.GetAccessTenure(t.Context(), billing.AccessTenureParams{CustomerID: order.CustomerID, Entitlements: []string{"library:read"}})
		require.NoError(t, err)
		require.EqualValues(t, 129600, tenure.Seconds["library:read"], "36 hours, not the sum of two overlapping 24-hour sources")
	})
	t.Run("OR-J27-S02/revoking-one-source-preserves-the-other", func(t *testing.T) {
		a, _ := newPaymentWorld(t, func(w *world) { publicPastClock(w, 72*time.Hour) })
		first := publicGrant(t, a, "short-source", 24)
		second := publicGrant(t, a, "surviving-source", 48)
		a.clock.Advance(time.Hour)
		require.NoError(t, a.alphaRemote.RevokeProductAccess(t.Context(), first.ID, billing.RevokeProductAccessParams{Reason: "One complimentary source ends"}))
		publicAccess(t, a, a.clock.Now(), true)
		publicAccess(t, a, first.StartsAt.Add(24*time.Hour), true)
		publicAccess(t, a, *second.EndsAt, false)
		rows, err := a.alphaRemote.ListProductAccess(t.Context(), billing.ProductAccessListParams{IDs: []billing.ProductAccessID{first.ID, second.ID}})
		require.NoError(t, err)
		require.Len(t, rows.Items, 2)
		for _, row := range rows.Items {
			if row.ID == first.ID {
				require.NotNil(t, row.RevokedAt)
			} else {
				require.Equal(t, second.ID, row.ID)
				require.Nil(t, row.RevokedAt)
			}
		}
	})
	t.Run("OR-J27-S03/exact-window-boundaries-and-historical-tenure", func(t *testing.T) {
		a, _ := newPaymentWorld(t, func(w *world) { publicPastClock(w, 48*time.Hour) })
		grant := publicGrant(t, a, "six-hour-window", 6)
		for _, sample := range []struct {
			at      time.Time
			allowed bool
		}{{grant.StartsAt.Add(-time.Microsecond), false}, {grant.StartsAt, true}, {grant.EndsAt.Add(-time.Microsecond), true}, {*grant.EndsAt, false}} {
			publicAccess(t, a, sample.at, sample.allowed)
		}
		a.clock.Advance(6 * time.Hour)
		for _, sample := range []struct {
			at      time.Time
			seconds int64
		}{{grant.StartsAt, 0}, {grant.StartsAt.Add(3 * time.Hour), 10800}, {*grant.EndsAt, 21600}} {
			tenure, err := a.alphaRemote.GetAccessTenure(t.Context(), billing.AccessTenureParams{CustomerID: grant.CustomerID, Entitlements: []string{"library:read", "never:granted"}, At: sample.at})
			require.NoError(t, err)
			require.Equal(t, sample.seconds, tenure.Seconds["library:read"])
			require.Zero(t, tenure.Seconds["never:granted"])
		}
		_, err := a.alphaRemote.GetAccessTenure(t.Context(), billing.AccessTenureParams{CustomerID: grant.CustomerID, Entitlements: []string{"library:read"}, At: a.clock.Now().Add(time.Hour)})
		require.ErrorIs(t, err, billing.ErrInvalid)
	})
	t.Run("OR-J27-S04/archive-and-refund-obey-explicit-access-policy", func(t *testing.T) {
		for _, revoke := range []bool{false, true} {
			t.Run(map[bool]string{false: "keep-access", true: "revoke-purchased-access"}[revoke], func(t *testing.T) {
				a, p := newPaymentWorld(t)
				product, price := publicSale(t, a, "refund-policy", 9_990_000, nil)
				order := publicPurchase(t, a, p, price.ID, 1, 9_990_000, "refund-policy-purchase")
				_, err := a.alphaRemote.UpdateProduct(t.Context(), product.ID, billing.UpdateProductParams{Archived: catalog.Value(true)})
				require.NoError(t, err)
				publicAccess(t, a, time.Time{}, true)
				refund, err := a.alphaRemote.RefundPayment(t.Context(), *order.Payment.PaymentID, billing.RefundPaymentParams{Full: true, RevokeAccess: revoke, Reason: "Explicit customer refund policy", IdempotencyKey: "policy-refund"})
				require.NoError(t, err)
				require.Equal(t, billing.PaymentSucceeded, refund.Status)
				publicAccess(t, a, time.Time{}, !revoke)
				retained, err := a.alphaRemote.GetOrder(t.Context(), order.ID)
				require.NoError(t, err)
				require.Equal(t, billing.OrderComplete, retained.Status)
				require.Equal(t, order.Payment.PaymentID, retained.Payment.PaymentID)
				payment, err := a.alphaRemote.GetPayment(t.Context(), *order.Payment.PaymentID)
				require.NoError(t, err)
				require.Equal(t, billing.PaymentRefunded, payment.Status)
				require.EqualValues(t, 9_990_000, payment.AmountRefunded)
				require.Len(t, p.effects(t), 2)
			})
		}
	})
}

func TestSubscriptionAccessRepair(t *testing.T) {
	t.Run("OR-J27-S05/future-subscription-repair-stays-closed", func(t *testing.T) {
		for _, removed := range []bool{false, true} {
			name := "close-future-window"
			if removed {
				name = "already-removed-window"
			}
			t.Run(name, func(t *testing.T) {
				a, _, sub := purchasedMembership(t)
				enableLifecycleMaintenance(t, a)
				database, err := db.NewWithPGXPool(a.pool, a.schema)
				require.NoError(t, err)
				ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
				a.clock.Advance(time.Hour)
				closedAt := a.clock.Now().Truncate(time.Microsecond)
				a.clock.Advance(closedAt.Sub(a.clock.Now()))
				start, end := closedAt.Add(2*time.Hour), closedAt.Add(4*time.Hour)
				service := entitlements.NewEntitlementService(database, a.clock)
				window := subscriptionAccessWindow(t, a, service, sub, start, end)
				// Retained source drift is deliberate fixture state, not the normal chargeback writer.
				_, err = a.pool.Exec(ctx, "UPDATE "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" SET status='canceled', cancel_type='chargeback', canceled_at=$1, ended_at=$1, lifecycle_rev=lifecycle_rev+1 WHERE merchant_id=$2 AND id=$3", closedAt, a.alpha.MerchantID().UUID(), sub.ID.UUID())
				require.NoError(t, err)
				if removed {
					// The previous boundary writer changed projections without terminating grants.
					_, err = a.pool.Exec(ctx, "UPDATE "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" SET deleted_at=CASE WHEN starts_at >= $1 THEN $1 ELSE deleted_at END, ends_at=CASE WHEN starts_at < $1 AND (ends_at IS NULL OR ends_at > $1) THEN $1 ELSE ends_at END WHERE merchant_id=$2 AND source_type='subscription' AND source_id=$3 AND deleted_at IS NULL AND revoked_at IS NULL", closedAt, a.alpha.MerchantID().UUID(), sub.ID.UUID().String())
					require.NoError(t, err)
				} else {
					require.NoError(t, service.BoundSubscriptionAccess(ctx, sub.ID.UUID(), closedAt))
				}
				a.clock.Advance(3 * time.Hour)
				check := func(at time.Time, want bool) {
					t.Helper()
					answer, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}, At: at})
					require.NoError(t, err)
					require.Equal(t, want, answer.Entitlements["membership:paid"])
				}
				check(a.clock.Now(), false)
				customer := sub.CustomerID.UUID()
				repair := converge.NewConvergeEngine(database, a.clock)
				if removed {
					armDestructive(t, a, false)
					_, err = repair.Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID(), Customer: &customer})
					require.NoError(t, err)
					check(a.clock.Now(), false)
					assertGrantTermination(t, a, window.GrantID, nil)
					armDestructive(t, a, true)
				}
				results := make(chan error, 4)
				for range 4 {
					go func() {
						_, err := repair.Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID(), Customer: &customer})
						results <- err
					}()
				}
				for range 4 {
					require.NoError(t, <-results)
				}
				for range 4 {
					_, err := repair.Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID(), Customer: &customer})
					require.NoError(t, err)
					check(a.clock.Now(), false)
					check(closedAt.Add(-time.Microsecond), true)
					assertGrantTermination(t, a, window.GrantID, &closedAt)
				}
			})
		}
	})
}

// Original grants stay immutable; a boundary is one additional event at business time.
func assertGrantTermination(t *testing.T, a *authorityWorld, grant uuid.UUID, at *time.Time) {
	t.Helper()
	var count int
	var startsAt *time.Time
	err := a.pool.QueryRow(t.Context(), "SELECT count(*), min(starts_at) FROM "+pgx.Identifier{a.schema, "grants"}.Sanitize()+" WHERE merchant_id=$1 AND supersedes_id=$2", a.alpha.MerchantID().UUID(), grant).Scan(&count, &startsAt)
	require.NoError(t, err)
	if at == nil {
		require.Zero(t, count)
	} else {
		require.Equal(t, 1, count)
		require.Equal(t, at.UTC(), startsAt.UTC())
	}
}

func TestSubscriptionAccessBoundary(t *testing.T) {
	t.Run("OR-J27-S06/closure-is-atomic-and-source-scoped", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		database, err := db.NewWithPGXPool(a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		a.clock.Advance(time.Hour)
		bound := a.clock.Now().Truncate(time.Microsecond)
		a.clock.Advance(bound.Sub(a.clock.Now()))
		service := entitlements.NewEntitlementService(database, a.clock)
		end := bound.Add(4 * time.Hour)
		var windows []*models.ProductAccess
		for _, start := range []time.Time{bound.Add(-time.Microsecond), bound, bound.Add(time.Microsecond)} {
			window := subscriptionAccessWindow(t, a, service, sub, start, end)
			windows = append(windows, window)
		}
		// A manual grant overlaps the same product; it must survive the subscription closure.
		grants, err := a.alpha.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: "unrelated-source", Items: []billing.CreateProductAccessParams{{CustomerID: sub.CustomerID, ProductID: sub.ProductID, EndsAt: &end, Reason: billing.GrantReasonComp}}})
		require.NoError(t, err)
		require.Len(t, grants, 1)
		var before string
		require.NoError(t, a.pool.QueryRow(ctx, "SELECT json_agg(g ORDER BY g.id)::text FROM "+pgx.Identifier{a.schema, "grants"}.Sanitize()+" g WHERE merchant_id=$1 AND event='grant'", a.alpha.MerchantID().UUID()).Scan(&before))
		rollback := errors.New("abort caller transaction")
		err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			require.NoError(t, entitlements.NewEntitlementService(database.NewWithPgxTx(tx), a.clock).BoundSubscriptionAccess(ctx, sub.ID.UUID(), bound))
			return rollback
		})
		require.ErrorIs(t, err, rollback)
		for _, window := range windows {
			retained, err := service.GetAccessByID(ctx, window.ID)
			require.NoError(t, err)
			require.Nil(t, retained.DeletedAt)
			require.Equal(t, end.UTC(), retained.EndsAt.UTC())
			assertGrantTermination(t, a, window.GrantID, nil)
		}
		// A merchant cannot close a different merchant's source by knowing its ID.
		require.NoError(t, service.BoundSubscriptionAccess(merchant.WithID(t.Context(), a.beta.MerchantID()), sub.ID.UUID(), bound))
		for _, window := range windows {
			assertGrantTermination(t, a, window.GrantID, nil)
		}
		results := make(chan error, 4)
		for range 4 {
			go func() { results <- service.BoundSubscriptionAccess(ctx, sub.ID.UUID(), bound) }()
		}
		for range 4 {
			require.NoError(t, <-results)
		}
		for i, window := range windows {
			if i == 0 {
				retained, err := service.GetAccessByID(ctx, window.ID)
				require.NoError(t, err)
				require.Equal(t, bound.UTC(), retained.EndsAt.UTC())
				assertGrantTermination(t, a, window.GrantID, nil)
			} else {
				assertGrantTermination(t, a, window.GrantID, &bound)
			}
		}
		var after string
		require.NoError(t, a.pool.QueryRow(ctx, "SELECT json_agg(g ORDER BY g.id)::text FROM "+pgx.Identifier{a.schema, "grants"}.Sanitize()+" g WHERE merchant_id=$1 AND event='grant'", a.alpha.MerchantID().UUID()).Scan(&after))
		require.Equal(t, before, after, "original grant facts must not change")
		a.clock.Advance(2 * time.Hour)
		answer, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
		require.NoError(t, err)
		require.True(t, answer.Entitlements["membership:paid"], "the independent grant still covers access")
	})
}

func TestSubscriptionAccessRepairRecheck(t *testing.T) {
	t.Run("OR-J27-S07/repair-rechecks-chargeback-after-detection", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		database, err := db.NewWithPGXPool(a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		a.clock.Advance(time.Hour)
		bound := a.clock.Now().Truncate(time.Microsecond)
		a.clock.Advance(bound.Sub(a.clock.Now()))
		start, end := bound.Add(time.Hour), bound.Add(3*time.Hour)
		window := subscriptionAccessWindow(t, a, entitlements.NewEntitlementService(database, a.clock), sub, start, end)
		_, err = a.pool.Exec(ctx, "UPDATE "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" SET deleted_at=$1 WHERE merchant_id=$2 AND id=$3", bound, a.alpha.MerchantID().UUID(), window.ID)
		require.NoError(t, err)
		// The source changes after detection, before the repair acquires its customer lock.
		fired := false
		remove := failpoint.Set(func(ctx context.Context, site failpoint.Site) error {
			if fired || site.Point != failpoint.BeforeRepair || site.Kind != "derive.grant_effect.missing" {
				return nil
			}
			fired = true
			_, err := a.pool.Exec(ctx, "UPDATE "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" SET status='canceled', cancel_type='chargeback', canceled_at=$1, ended_at=$1, lifecycle_rev=lifecycle_rev+1 WHERE merchant_id=$2 AND id=$3", bound, a.alpha.MerchantID().UUID(), sub.ID.UUID())
			return err
		})
		t.Cleanup(remove)
		customer := sub.CustomerID.UUID()
		repair := converge.NewConvergeEngine(database, a.clock)
		_, err = repair.Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID(), Customer: &customer})
		require.NoError(t, err)
		require.True(t, fired)
		remove()
		var deletedAt *time.Time
		require.NoError(t, a.pool.QueryRow(ctx, "SELECT deleted_at FROM "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), window.ID).Scan(&deletedAt))
		require.NotNil(t, deletedAt, "stale missing-effect repair must not restore the window")
		assertGrantTermination(t, a, window.GrantID, nil)
		armDestructive(t, a, true)
		_, err = repair.Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID(), Customer: &customer})
		require.NoError(t, err)
		assertGrantTermination(t, a, window.GrantID, &bound)
	})
}

func TestSubscriptionAccessRepairPreservesSources(t *testing.T) {
	t.Run("OR-J27-S08/ordinary-and-uncertain-sources-keep-their-grants", func(t *testing.T) {
		for _, status := range []string{"canceled", "unverified"} {
			t.Run(status, func(t *testing.T) {
				a, _, sub := purchasedMembership(t)
				database, err := db.NewWithPGXPool(a.pool, a.schema)
				require.NoError(t, err)
				ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
				a.clock.Advance(time.Hour)
				bound := a.clock.Now().Truncate(time.Microsecond)
				a.clock.Advance(bound.Sub(a.clock.Now()))
				start, end := bound.Add(time.Hour), bound.Add(3*time.Hour)
				window := subscriptionAccessWindow(t, a, entitlements.NewEntitlementService(database, a.clock), sub, start, end)
				_, err = a.pool.Exec(ctx, "UPDATE "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" SET status=$1, cancel_type='user', canceled_at=$2, ended_at=$2, lifecycle_rev=lifecycle_rev+1 WHERE merchant_id=$3 AND id=$4", status, bound, a.alpha.MerchantID().UUID(), sub.ID.UUID())
				require.NoError(t, err)
				_, err = a.pool.Exec(ctx, "UPDATE "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" SET deleted_at=$1 WHERE merchant_id=$2 AND id=$3", bound, a.alpha.MerchantID().UUID(), window.ID)
				require.NoError(t, err)
				enableLifecycleMaintenance(t, a)
				customer := sub.CustomerID.UUID()
				_, err = converge.NewConvergeEngine(database, a.clock).Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID(), Customer: &customer})
				require.NoError(t, err)
				assertGrantTermination(t, a, window.GrantID, nil)
				var restored bool
				require.NoError(t, a.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" WHERE merchant_id=$1 AND grant_id=$2 AND deleted_at IS NULL)", a.alpha.MerchantID().UUID(), window.GrantID).Scan(&restored))
				require.True(t, restored, "ordinary and uncertain sources still repair missing windows")
				a.clock.Advance(2 * time.Hour)
				answer, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"membership:paid"}})
				require.NoError(t, err)
				require.True(t, answer.Entitlements["membership:paid"])
			})
		}
	})
}

func subscriptionAccessWindow(t *testing.T, a *authorityWorld, service *entitlements.EntitlementService, sub *billing.Subscription, start, end time.Time) *models.ProductAccess {
	t.Helper()
	ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
	_, err := service.PushAccess(ctx, entitlements.PushAccessParams{
		UserID: sub.CustomerID.UUID().String(), CustomerID: sub.CustomerID.UUID(), ProductID: sub.ProductID.UUID(),
		SourceType: models.AccessSourceSubscription, SourceID: sub.ID.UUID().String(), NotBefore: &start, EndsAt: &end,
	})
	require.NoError(t, err)
	// PushAccess returns the longest window of the source, which can be an older period.
	var id uuid.UUID
	require.NoError(t, a.pool.QueryRow(ctx, "SELECT id FROM "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" WHERE merchant_id=$1 AND source_type='subscription' AND source_id=$2 AND starts_at=$3", a.alpha.MerchantID().UUID(), sub.ID.UUID().String(), start).Scan(&id))
	window, err := service.GetAccessByID(ctx, id)
	require.NoError(t, err)
	return window
}

func TestSubscriptionAccessRepairTerminated(t *testing.T) {
	t.Run("OR-J27-S09/an-ended-window-still-applies-its-recorded-revocation", func(t *testing.T) {
		a, _, sub := purchasedMembership(t)
		armDestructive(t, a, true)
		database, err := db.NewWithPGXPool(a.pool, a.schema)
		require.NoError(t, err)
		ctx := merchant.WithID(t.Context(), a.alpha.MerchantID())
		a.clock.Advance(time.Hour)
		bound := a.clock.Now().Truncate(time.Microsecond)
		a.clock.Advance(bound.Sub(a.clock.Now()))
		start, end := bound.Add(time.Hour), bound.Add(2*time.Hour)
		window := subscriptionAccessWindow(t, a, entitlements.NewEntitlementService(database, a.clock), sub, start, end)
		_, err = a.pool.Exec(ctx, "UPDATE "+pgx.Identifier{a.schema, "subscriptions"}.Sanitize()+" SET status='canceled', cancel_type='chargeback', canceled_at=$1, ended_at=$1, lifecycle_rev=lifecycle_rev+1 WHERE merchant_id=$2 AND id=$3", bound, a.alpha.MerchantID().UUID(), sub.ID.UUID())
		require.NoError(t, err)
		_, err = a.pool.Exec(ctx, "UPDATE "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" SET ends_at=$1 WHERE merchant_id=$2 AND source_type='subscription' AND source_id=$3 AND starts_at < $1", bound, a.alpha.MerchantID().UUID(), sub.ID.UUID().String())
		require.NoError(t, err)
		// Model a committed revocation whose projection still needs repair.
		err = database.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			ledger := grants.New(gen.New(tx), a.alpha.MerchantID().UUID())
			if err := ledger.LockCustomer(ctx, sub.CustomerID.UUID()); err != nil {
				return err
			}
			_, err := ledger.RevokeAsOf(ctx, window.GrantID, "chargeback", bound)
			return err
		})
		require.NoError(t, err)
		a.clock.Advance(3 * time.Hour)
		customer := sub.CustomerID.UUID()
		repair := converge.NewConvergeEngine(database, a.clock)
		for pass := range 2 {
			result, err := repair.Converge(ctx, converge.Scope{Merchant: a.alpha.MerchantID(), Customer: &customer})
			require.NoError(t, err)
			var retracted bool
			require.NoError(t, a.pool.QueryRow(ctx, "SELECT revoked_at IS NOT NULL OR deleted_at IS NOT NULL FROM "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" WHERE merchant_id=$1 AND id=$2", a.alpha.MerchantID().UUID(), window.ID).Scan(&retracted))
			require.True(t, retracted, "an ended window must honor its already recorded grant revocation")
			assertGrantTermination(t, a, window.GrantID, &bound)
			if pass == 1 {
				require.Zero(t, result.AutoFixed, "a completed retraction must not be reported as fixed again")
			}
		}
	})
}
