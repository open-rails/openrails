//go:build e2e && integration

package subscriptions_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/stretchr/testify/require"
)

// A refund removes only the access bought by that payment, never renewal.
func TestRefundPreservesNewerPaidPeriod(t *testing.T) {
	for _, rail := range []string{"nmi", "stripe"} {
		for _, mode := range []string{"admin_keep", "admin_revoke", "provider_full"} {
			t.Run(rail+"/"+mode, func(t *testing.T) {
				w := newWorld(t)
				e := enroll(t, w, rail, embedded)
				old := completed(w.payments(embedded, e.c.id))[0]
				e.toFreshPeriodEnd()
				w.runRenewals()
				require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
				require.True(t, e.c.entitled(e.ent))
				if mode == "provider_full" {
					first := e.providerLedger()[0]
					if rail == "nmi" {
						w.nmi.Refund(first.ID, first.Amount)
					} else {
						w.stripe.dashboardRefund(first.Charge, first.Amount)
					}
					require.Equal(t, http.StatusOK, w.deliver(rail, w.refundNotice(rail)))
				} else {
					_, err := w.client[embedded].RefundPayment(t.Context(), old.ID, billing.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: mode == "admin_revoke", IdempotencyKey: "refund-old-period"})
					require.NoError(t, err)
				}
				w.settle()
				sub := w.subscription(embedded, e.sub)
				hasAccess := e.c.entitled(e.ent)
				require.Equal(t, billing.SubscriptionActive, sub.Status)
				require.True(t, hasAccess, "newer paid access survives the older refund")
				w.checkMoneyInvariants()
				e.toFreshPeriodEnd()
				w.runRenewals()
				require.Len(t, e.providerLedger(), 3, "refund does not cancel renewal")
				require.True(t, e.c.entitled(e.ent))

			})
		}
	}
}

func TestRefundRevokesCurrentPeriodWithoutCanceling(t *testing.T) {
	for _, rail := range []string{"nmi", "stripe"} {
		for _, mode := range []string{"admin", "provider"} {
			t.Run(rail+"/"+mode, func(t *testing.T) {
				w := newWorld(t)
				e := enroll(t, w, rail, embedded)
				original := completed(w.payments(embedded, e.c.id))[0]
				if mode == "admin" {
					params := billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "requested_by_customer", IdempotencyKey: "current-period"}
					_, err := w.client[embedded].RefundPayment(t.Context(), original.ID, params)
					require.NoError(t, err)
					_, err = w.client[embedded].RefundPayment(t.Context(), original.ID, params)
					require.NoError(t, err)
				} else {
					paid := e.providerLedger()[0]
					if rail == "nmi" {
						w.nmi.Refund(paid.ID, paid.Amount)
					} else {
						w.stripe.dashboardRefund(paid.Charge, paid.Amount)
					}
				}
				require.Equal(t, http.StatusOK, w.deliver(rail, w.refundNotice(rail)))
				require.Equal(t, http.StatusOK, w.deliver(rail, w.refundNotice(rail)))
				w.settle()
				require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)

				require.False(t, e.c.entitled(e.ent))
				w.checkMoneyInvariants()
				e.toFreshPeriodEnd()
				w.converge()
				require.False(t, e.c.entitled(e.ent), "grace cannot restore refunded access")
				w.runRenewals()
				require.Len(t, e.providerLedger(), 2)
				require.True(t, e.c.entitled(e.ent), "a new payment buys its own access")
			})
		}
	}
}

func TestRefundSeatChangesRequireAccessReview(t *testing.T) {
	for _, refundBase := range []bool{true, false} {
		t.Run(fmt.Sprintf("original_payment_%t", refundBase), func(t *testing.T) {
			w := newWorld(t)
			team := w.seatPrice("refund-seats", 1, 1000, catalog.Quantity{Min: 1, Max: 10})
			c, sub := w.engineMember("nmi", embedded, team)
			base := completed(w.payments(embedded, c.id))[0]
			end := *w.subscription(embedded, sub).CurrentPeriodEndsAt
			w.advance(end.Sub(w.clock.Now()) - 360*time.Hour)
			changed, err := c.change(sub, billing.ChangeSubscriptionParams{Quantity: seats(3), IdempotencyKey: "refund-seats-change"})
			require.NoError(t, err)
			require.Equal(t, "succeeded", changed.Status)
			payment := base
			if !refundBase {
				for _, p := range completed(w.payments(embedded, c.id)) {
					if p.ID != base.ID {
						payment = p
					}
				}
				require.NotEqual(t, base.ID, payment.ID)
			}
			params := billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "requested_by_customer", IdempotencyKey: "refund-combined-access"}
			for range 2 {
				_, err = w.client[embedded].RefundPayment(t.Context(), payment.ID, params)
				require.NoError(t, err)
			}
			w.settle()
			entitlementSeats, productSeats := w.seatsHeld(c, team.ent, team.ProductID)
			require.Equal(t, seats(3), entitlementSeats)
			require.Equal(t, seats(3), productSeats)
			require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, sub).Status)
			paid, err := w.client[embedded].GetPayment(t.Context(), payment.ID)
			require.NoError(t, err)
			require.Equal(t, paid.Amount, paid.AmountRefunded)
			var findings int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.reconciliation_findings WHERE finding_type='consistency.refund_access' AND status='requires_review' AND subject_key=$1`), payment.ID.String()).Scan(&findings))
			require.Equal(t, 1, findings, "convergence preserves one actionable review finding")
			w.checkMoneyInvariants()
		})
	}
}

func TestRefundAccessDecisionSurvivesArchive(t *testing.T) {
	source := newWorld(t)
	e := enroll(t, source, "nmi", embedded)
	original := completed(source.payments(embedded, e.c.id))[0]
	_, err := source.client[embedded].RefundPayment(t.Context(), original.ID, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "requested_by_customer", IdempotencyKey: "archive-refund"})
	require.NoError(t, err)
	source.settle()
	require.False(t, e.c.entitled(e.ent))

	source.stop()
	source.checkMoneyInvariants()
	mid := source.client[embedded].MerchantID()
	sourceDB, err := db.NewWithPGXPool(source.pool, source.schema)
	require.NoError(t, err)
	var archive bytes.Buffer
	require.NoError(t, merchantarchive.Export(t.Context(), sourceDB, mid, &archive))
	target := handoffTarget(t, source)
	targetDB, err := db.NewWithPGXPool(target.pool, target.schema)
	require.NoError(t, err)
	directory, err := merchants.NewDirectoryService(targetDB.DataPool())
	require.NoError(t, err)
	_, _, err = directory.RegisterForRestore(t.Context(), mid, target.slug)
	require.NoError(t, err)
	_, err = merchantarchive.Restore(t.Context(), targetDB, mid, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	source.waive("recorded", "the restored destination owns subsequent renewal charges")
	armMerchant(t, target, mid)
	target.start()
	e.w, e.c.w = target, target
	var revoked bool
	require.NoError(t, target.pool.QueryRow(t.Context(), target.q(`SELECT metadata->>'refund_access_revoked'='true' FROM billing.payments WHERE id=$1`), original.ID.UUID()).Scan(&revoked))
	require.True(t, revoked)
	target.converge()
	require.False(t, e.c.entitled(e.ent))
	e.toFreshPeriodEnd()
	target.converge()
	require.False(t, e.c.entitled(e.ent), "restore cannot recreate grace for the refunded period")
	target.runRenewals()
	require.True(t, e.c.entitled(e.ent))
	require.Len(t, e.providerLedger(), 2)
}

func TestCCBillRefundNeverGuessesOrCancels(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("known_original_%t", known), func(t *testing.T) {
			dl := newDataLinkFake(t)
			w := newDataLinkWorld(t, dl)
			m := importCCBill(t, w)
			w.armDestructive()
			original := m.saleTxn
			if !known {
				original = ccbillNumericID()
			}
			fields := m.reversal(original, "9.99")
			w.deliverCCBill("Refund", fields)
			w.deliverCCBill("Refund", fields)
			w.settle()
			require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, m.sub).Status)
			require.Equal(t, !known, m.c.entitled(m.ent))
			require.Zero(t, dl.canceled(m.railSub))
			require.Zero(t, w.providerCancels("ccbill_cancel_subscription", m.sub.UUID()))
			var refunds int
			require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*) FROM billing.payments WHERE refunded_payment_id IS NOT NULL`)).Scan(&refunds))
			if known {
				require.Equal(t, 1, refunds)
			} else {
				require.Zero(t, refunds, "an unknown original cannot be replaced with the newest payment")
				require.Positive(t, w.countRows(`SELECT count(*) FROM billing.reconciliation_findings WHERE finding_type='consistency.ledger.unbooked' AND status='requires_review'`))
			}
		})
	}
}

func TestRefundConcurrentAccessRepair(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	original := completed(w.payments(embedded, e.c.id))[0]
	// Simulate the missing projection that ordinary convergence repairs.
	_, err := w.pool.Exec(t.Context(), w.q(`DELETE FROM billing.product_access WHERE customer_id=$1`), e.c.cid().UUID())
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := w.client[embedded].RefundPayment(t.Context(), original.ID, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "requested_by_customer", IdempotencyKey: "refund-during-repair"})
		done <- err
	}()
	w.converge()
	require.NoError(t, <-done)
	w.settle()
	require.False(t, e.c.entitled(e.ent), "repair cannot restore access after a refund")
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
	w.checkMoneyInvariants()
}

func TestCCBillRefundUsesAcceptedInitialBoundary(t *testing.T) {
	s := openCCBillShop(t)
	c, _ := s.buyer()
	created := c.order(http.MethodPost, "/orders", "", map[string]any{"lines": []any{map[string]any{"price_id": s.ccbillPrice}}})
	require.Equal(t, http.StatusCreated, created.status, "%v", created.body)
	paid := s.payCCBill(c, created.body["id"].(string))
	require.Equal(t, http.StatusOK, paid.status, "%v", paid.body)
	subID, transaction := ccbillNumericID(), ccbillNumericID()
	sale := s.sale(flexForm(t, paid.body), subID, transaction)
	sale["nextRenewalDate"] = ccbillDate(s.w.clock.Now().Add(29 * day))
	s.w.deliverCCBill("NewSaleSuccess", sale)
	members := s.memberships(c)
	require.Len(t, members, 1)
	sub := members[0]
	require.False(t, sub.CurrentPeriodEndsAt.Equal(sub.StartedAt.Add(30*day)), "the verified provider boundary differs from nominal cadence")
	require.True(t, c.entitled(s.ent))
	s.w.deliverCCBill("Refund", map[string]string{"subscriptionId": subID, "transactionId": transaction, "amount": "23.00", "currencyCode": "840", "reason": "customer request"})
	s.w.settle()
	require.False(t, c.entitled(s.ent))
	require.Equal(t, billing.SubscriptionActive, s.w.subscription(embedded, sub.ID).Status)
	require.Zero(t, s.w.countRows(`SELECT count(*) FROM billing.reconciliation_findings WHERE finding_type='consistency.refund_access'`))
}

func TestRefundCartMemberships(t *testing.T) {
	w := orderWorld(t)
	one, two := w.membership("refund:one", 10_000_000), w.membership("refund:two", 20_000_000)
	c := w.newCustomer()
	body := map[string]any{"lines": []any{line(one, 0), line(two, 0)}, "expected_total": micros(30_000_000),
		"payment": map[string]any{"token": w.newCardToken("nmi", visa), "psp_id": w.psp["nmi"], "billing_details": map[string]any{"name": "Cart Refund"}}}
	bought := c.order(http.MethodPost, "/orders", "refund-cart", body)
	require.Equal(t, http.StatusCreated, bought.status, "%v", bought.body)
	require.True(t, c.entitled("refund:one"))
	require.True(t, c.entitled("refund:two"))
	payment, err := billing.ParsePaymentID(paymentOf(bought)["payment_id"].(string))
	require.NoError(t, err)
	_, err = w.client[embedded].RefundPayment(t.Context(), payment, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "requested_by_customer", IdempotencyKey: "refund-cart-memberships"})
	require.NoError(t, err)
	w.settle()
	require.False(t, c.entitled("refund:one"))
	require.False(t, c.entitled("refund:two"))
	members := w.memberships(c)
	require.Len(t, members, 2)
	for _, sub := range members {
		require.Equal(t, billing.SubscriptionActive, sub.Status)
	}
	w.checkMoneyInvariants()
}

func TestRefundConcurrentRenewal(t *testing.T) {
	for _, rail := range []string{"nmi", "stripe"} {
		t.Run(rail, func(t *testing.T) {
			w := newWorld(t)
			e := enroll(t, w, rail, embedded)
			original := completed(w.payments(embedded, e.c.id))[0]
			e.toFreshPeriodEnd()
			done := make(chan error, 1)
			go func() {
				_, err := w.client[embedded].RefundPayment(t.Context(), original.ID, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "requested_by_customer", IdempotencyKey: "refund-during-renewal"})
				done <- err
			}()
			w.runRenewals()
			require.NoError(t, <-done)
			w.settle()
			require.Len(t, e.providerLedger(), 2)
			require.True(t, e.c.entitled(e.ent), "the concurrently paid period keeps its access")
			require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)
			w.checkMoneyInvariants()
		})
	}
}

// Pruning a source must not turn an already-refunded window into new access.
func TestRefundAccessDecisionSurvivesTombstones(t *testing.T) {
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	original := completed(w.payments(embedded, e.c.id))[0]
	var window gen.RefundRevokedAccessWindowParams
	window.MerchantID = w.client[embedded].MerchantID().UUID()
	window.SourceID = e.sub.UUID().String()
	window.SourceType = "subscription"
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT customer_id, product_id, starts_at, ends_at FROM billing.grants WHERE merchant_id=$1 AND source_id=$2 AND source_type='subscription' AND event='grant' ORDER BY created_at LIMIT 1`), window.MerchantID, window.SourceID).Scan(&window.CustomerID, &window.ProductID, &window.StartsAt, &window.EndsAt))
	require.NotNil(t, window.EndsAt)
	_, err := w.client[embedded].RefundPayment(t.Context(), original.ID, billing.RefundPaymentParams{Full: true, RevokeAccess: true, Reason: "requested_by_customer", IdempotencyKey: "tombstone-refund"})
	require.NoError(t, err)
	w.settle()
	require.False(t, e.c.entitled(e.ent))
	database, err := db.NewWithPGXPool(w.pool, w.schema)
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), w.client[embedded].MerchantID())
	rollback := errors.New("restore fixture after tombstone check")
	require.ErrorIs(t, database.RunInTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE billing.payments SET deleted_at=now() WHERE merchant_id=$1 AND id=$2`, window.MerchantID, original.ID.UUID())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE billing.subscriptions SET deleted_at=now() WHERE merchant_id=$1 AND id=$2`, window.MerchantID, e.sub.UUID())
		if err != nil {
			return err
		}
		q := gen.New(tx)
		for _, source := range []string{"payment", "subscription", "grace"} {
			candidate := window
			candidate.SourceType = source
			if source == "payment" {
				candidate.PaymentID = new(original.ID.UUID())
			}
			if source == "grace" {
				candidate.StartsAt = *window.EndsAt
			}
			revoked, err := q.RefundRevokedAccessWindow(ctx, candidate)
			if err != nil {
				return err
			}
			require.True(t, revoked, "%s must retain the refund decision", source)
		}
		later := window
		later.StartsAt = *window.EndsAt
		later.EndsAt = new(window.EndsAt.Add(30 * 24 * time.Hour))
		revoked, err := q.RefundRevokedAccessWindow(ctx, later)
		if err != nil {
			return err
		}
		require.False(t, revoked, "a later paid period is not revoked")
		_, err = q.LockPaymentForRefund(ctx, gen.LockPaymentForRefundParams{MerchantID: window.MerchantID, PaymentID: original.ID.UUID()})
		require.ErrorIs(t, err, pgx.ErrNoRows, "new refund access commands refuse pruned payments")
		return rollback
	}), rollback)
	w.checkMoneyInvariants()
}
