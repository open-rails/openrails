//go:build integration

package integrationharness

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// checkNMITierChangeReplay: an NMI upgrade answers the same
// durable tier change contract as a Stripe tier change
// (checkStripeTierChangeReplay) in every deployment shape. A
// committed upgrade replays its stored result under the same key without a
// second enrollment or charge; a lost proration response answers 202 with the
// operation, the same key replays it, another key is refused with it, and the
// restarted deployment's own verifier converges the upgrade exactly once from
// the exact provider receipt; a definitive decline is coded and never resent.
// The provider is a loopback NMI fake; no live gateway behavior is proven.
func checkNMITierChangeReplay(t *testing.T, h *Harness, gateway *FakeNMIGateway, d moneyDeployment) {
	t.Helper()
	ctx := context.Background()
	gateway.SetMode(NMISaleApprove)
	gateway.SetVisible(true)
	client := d.client()

	// Success, then the stored result.
	fixture := h.SeedNMITierSubscription(d.runtime(), d.merchant)
	gateway.DeclarePlan(nmi.V5Plan{ID: fixture.ProPlan, PlanAmount: dollars(fixture.ProAmount), DayFrequency: "30", PlanPayments: "0"})
	var recurringBefore string
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT pm.stored_credential_recurring_ref FROM billing.payment_methods pm JOIN billing.subscriptions s ON s.payment_method_id=pm.id WHERE s.id=$1`, fixture.SubscriptionID).Scan(&recurringBefore))
	require.NotEmpty(t, recurringBefore)
	request := openrails.ChangeTierRequest{PriceID: (fixture.ProPrice).String()}
	key := "tier-" + uuid.NewString()[:8]
	sales, enrollments := gateway.SaleCount(), len(gateway.Enrollments())
	first, err := client.ChangeTier(ctx, fixture.Subscription, key, request)
	require.NoError(t, err)
	require.Equal(t, "succeeded", first.Status)
	require.Equal(t, "upgrade", first.Action)
	require.Equal(t, h.LatestTierChangeOperation(fixture.SubscriptionID).ID.String(), first.OperationID)
	require.Equal(t, fixture.ProAmount, first.NextChargeAmount)
	require.Positive(t, first.AmountDueNow)
	require.Len(t, gateway.Enrollments(), enrollments+1)
	enrollment := gateway.Enrollments()[enrollments]
	require.Equal(t, fixture.ProPlan, enrollment.Plan)
	require.Equal(t, fixture.Vault, enrollment.Vault)
	require.Empty(t, enrollment.SaleType)
	require.Empty(t, enrollment.SaleAmount)
	require.Empty(t, enrollment.StoredCredentialIndicator)
	require.Equal(t, sales+1, gateway.SaleCount())
	require.Equal(t, dollars(first.AmountDueNow), gateway.Sales()[sales].Amount, "the proration charged is the frozen amount")
	successor := first.SubscriptionID.UUID()
	require.NotEqual(t, fixture.SubscriptionID, successor, "a committed upgrade names its successor")
	require.Equal(t, "active", h.LocalSubscriptionStatus(successor))
	require.Equal(t, fixture.ProPrice, h.LocalSubscriptionPrice(successor))
	require.Equal(t, "cancelled", h.LocalSubscriptionStatus(fixture.SubscriptionID))
	var recurringAfter string
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT pm.stored_credential_recurring_ref FROM billing.payment_methods pm JOIN billing.subscriptions s ON s.payment_method_id=pm.id WHERE s.id=$1`, successor).Scan(&recurringAfter))
	require.Equal(t, recurringBefore, recurringAfter, "schedule-only successor preserves the established recurring agreement")
	again, err := client.ChangeTier(ctx, fixture.Subscription, key, request)
	require.NoError(t, err)
	require.Equal(t, first, again, "the same key answers the stored result")
	require.Equal(t, sales+1, gateway.SaleCount(), "a replay never charges again")
	require.Len(t, gateway.Enrollments(), enrollments+1, "a replay never enrolls again")
	require.Equal(t, 1, h.TierChangeOperations(fixture.SubscriptionID))

	// A tier change without the client's key never mutates.
	unkeyed := h.SeedNMITierSubscription(d.runtime(), d.merchant)
	gateway.DeclarePlan(nmi.V5Plan{ID: unkeyed.ProPlan, PlanAmount: dollars(unkeyed.ProAmount), DayFrequency: "30", PlanPayments: "0"})
	_, err = client.ChangeTier(ctx, unkeyed.Subscription, "", openrails.ChangeTierRequest{PriceID: (unkeyed.ProPrice).String()})
	requireRefusal(t, err, openrails.ErrInvalid, openrails.CodeTierChangeIdempotencyKeyRequired)
	require.Equal(t, 0, h.TierChangeOperations(unkeyed.SubscriptionID))
	require.Equal(t, unkeyed.BasicPrice, h.LocalSubscriptionPrice(unkeyed.SubscriptionID))

	// Lost proration response: accepted, owned, converged once after a restart.
	lost := h.SeedNMITierSubscription(d.runtime(), d.merchant)
	gateway.DeclarePlan(nmi.V5Plan{ID: lost.ProPlan, PlanAmount: dollars(lost.ProAmount), DayFrequency: "30", PlanPayments: "0"})
	_, err = h.Pool().Exec(ctx, `UPDATE billing.payment_methods SET stored_credential_recurring_ref='',stored_credential_unscheduled_ref='' WHERE id=(SELECT payment_method_id FROM billing.subscriptions WHERE id=$1)`, lost.SubscriptionID)
	require.NoError(t, err)
	lostRequest := openrails.ChangeTierRequest{PriceID: (lost.ProPrice).String()}
	lostKey := "lost-" + uuid.NewString()[:8]
	sales, enrollments = gateway.SaleCount(), len(gateway.Enrollments())
	gateway.SetMode(NMISaleUncertain)
	gateway.SetVisible(false)
	pending, err := client.ChangeTier(ctx, lost.Subscription, lostKey, lostRequest)
	require.NoError(t, err)
	require.Equal(t, "processing", pending.Status)
	op := h.LatestTierChangeOperation(lost.SubscriptionID)
	require.Equal(t, "unknown_needs_verify", op.Status)
	require.Equal(t, op.ID.String(), pending.OperationID)
	require.Equal(t, lost.Subscription, *pending.SubscriptionID, "an unresolved upgrade names the predecessor it owns")
	require.Equal(t, "active", h.LocalSubscriptionStatus(lost.SubscriptionID), "nothing commits without a receipt")
	require.Equal(t, sales+1, gateway.SaleCount(), "the proration landed at the provider")
	replayed, err := client.ChangeTier(ctx, lost.Subscription, lostKey, lostRequest)
	require.NoError(t, err)
	require.Equal(t, pending, replayed)
	_, err = client.ChangeTier(ctx, lost.Subscription, "other-"+uuid.NewString()[:8], lostRequest)
	requireRefusal(t, err, openrails.ErrConflict, openrails.CodeTierChangeInFlight)
	var refused *openrails.StatusError
	require.True(t, errors.As(err, &refused))
	require.Equal(t, op.ID.String(), refused.Metadata["operation_id"])
	require.Equal(t, sales+1, gateway.SaleCount(), "nothing is resent while the outcome is unknown")
	require.Len(t, gateway.Enrollments(), enrollments+1)

	d.stop()
	gateway.SetVisible(true)
	d.start()
	client = d.client()
	h.MakeOperationDue(op.ID)
	h.FireProviderIntentVerify(h.Pool(), op.ID)
	require.Eventually(t, func() bool {
		return h.LatestTierChangeOperation(lost.SubscriptionID).Status == "succeeded"
	}, 90*time.Second, 500*time.Millisecond, "the restarted deployment's verifier settles from the exact receipt")
	done, err := client.ChangeTier(ctx, lost.Subscription, lostKey, lostRequest)
	require.NoError(t, err)
	require.Equal(t, "succeeded", done.Status)
	require.Equal(t, pending.OperationID, done.OperationID)
	require.Equal(t, pending.AmountDueNow, done.AmountDueNow)
	require.Equal(t, gateway.Sales()[sales].TransactionID, done.Payment.TransactionID)
	var recurring, unscheduled string
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT pm.stored_credential_recurring_ref,pm.stored_credential_unscheduled_ref FROM billing.payment_methods pm JOIN billing.subscriptions s ON s.payment_method_id=pm.id WHERE s.id=$1`, done.SubscriptionID.UUID()).Scan(&recurring, &unscheduled))
	require.Empty(t, recurring, "neither schedule creation nor unscheduled proration establishes recurring consent")
	require.Equal(t, done.Payment.TransactionID, unscheduled, "only qualified customer proration establishes its unscheduled agreement")
	require.Equal(t, "cancelled", h.LocalSubscriptionStatus(lost.SubscriptionID))
	require.Equal(t, lost.ProPrice, h.LocalSubscriptionPrice(done.SubscriptionID.UUID()))
	require.Equal(t, sales+1, gateway.SaleCount())
	require.Len(t, gateway.Enrollments(), enrollments+1)
	require.Equal(t, 1, h.TierChangeOperations(lost.SubscriptionID))

	// A definitive decline is coded, replays as itself and is never resent.
	declined := h.SeedNMITierSubscription(d.runtime(), d.merchant)
	gateway.DeclarePlan(nmi.V5Plan{ID: declined.ProPlan, PlanAmount: dollars(declined.ProAmount), DayFrequency: "30", PlanPayments: "0"})
	declinedRequest := openrails.ChangeTierRequest{PriceID: (declined.ProPrice).String()}
	declinedKey := "decl-" + uuid.NewString()[:8]
	sales, attempts := gateway.SaleCount(), gateway.SaleAttempts()
	gateway.SetMode(NMISaleDecline)
	_, err = client.ChangeTier(ctx, declined.Subscription, declinedKey, declinedRequest)
	requireRefusal(t, err, openrails.ErrPaymentRefused, "transaction_was_declined_by_processor")
	require.Equal(t, "failed_terminal", h.LatestTierChangeOperation(declined.SubscriptionID).Status)
	_, err = client.ChangeTier(ctx, declined.Subscription, declinedKey, declinedRequest)
	requireRefusal(t, err, openrails.ErrPaymentRefused, "transaction_was_declined_by_processor")
	require.Equal(t, attempts+1, gateway.SaleAttempts(), "a declined proration is never resent")
	require.Equal(t, sales, gateway.SaleCount())
	require.Equal(t, "active", h.LocalSubscriptionStatus(declined.SubscriptionID))
	gateway.SetMode(NMISaleApprove)
}

// dollars renders USD micros the way the NMI wire carries them.
func dollars(micros int64) string {
	return fmt.Sprintf("%d.%02d", micros/1_000_000, micros%1_000_000/10_000)
}
