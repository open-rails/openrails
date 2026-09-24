//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

var rails = []string{"stripe", "nmi"}

// engineCase is one engine-owned membership on one rail and topology.
type engineCase struct {
	w       *world
	rail    string
	tp      topology
	c       *customer
	price   string
	amount  int64
	ent     string
	sub     openrails.SubscriptionID
	method  string
	started time.Time
}

func (e *engineCase) providerLedger() []ledgerEntry {
	if e.rail == "stripe" {
		return e.w.stripe.ledger("")
	}
	return e.w.nmi.ledger("")
}

func (e *engineCase) providerAttempts() int {
	if e.rail == "stripe" {
		return e.w.stripe.attempts("")
	}
	return e.w.nmi.saleAttempts()
}

func (e *engineCase) setDecline(last4, stripeCode, nmiCode string) {
	if e.rail == "stripe" {
		e.w.stripe.setDecline(last4, stripeCode)
	} else {
		e.w.nmi.setDecline(last4, nmiCode)
	}
}

// enroll starts a paid engine membership: one initial charge, standing on
// the returned subscription.
func enroll(t *testing.T, w *world, rail string, tp topology) *engineCase {
	t.Helper()
	price := w.membership("content:members", 9_990_000)
	e := &engineCase{w: w, rail: rail, tp: tp, price: price.ID, amount: 999, ent: "content:members", started: w.clock.Now()}
	e.c = w.newCustomer()
	e.method = e.c.saveCard(rail, visa)
	e.sub = e.c.subscribe(tp, rail, price.ID, e.ent, e.method)
	sub := w.subscription(tp, e.sub)
	require.Equal(t, "engine", sub.CollectionPolicy)
	require.Empty(t, sub.RailSubscriptionID)
	require.Equal(t, "active", sub.Status)
	require.Len(t, e.providerLedger(), 1, "one initial charge")
	require.True(t, e.c.entitled(e.ent))
	return e
}

func (e *engineCase) periodEnd() time.Time {
	sub := e.w.subscription(e.tp, e.sub)
	require.NotNil(e.w.t, sub.CurrentPeriodEndsAt)
	return *sub.CurrentPeriodEndsAt
}

// toPeriodEnd moves engine time just past the current paid period.
func (e *engineCase) toPeriodEnd() {
	e.w.advance(e.periodEnd().Sub(e.w.clock.Now()) + time.Second)
}

func forEach(t *testing.T, run func(t *testing.T, rail string, tp topology)) {
	for _, rail := range rails {
		for _, tp := range []topology{embedded, remote} {
			t.Run(fmt.Sprintf("%s/%s", rail, tp), func(t *testing.T) { run(t, rail, tp) })
		}
	}
}

// Scenarios 0, 1 and 10: with the demo's embedded posture a due renewal
// executes (never parks); each period charges exactly once, extends the
// period and keeps access continuous; local payments equal provider charges.
func TestEngineHappyRenewals(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		e := enroll(t, w, rail, tp)
		for period := 2; period <= 3; period++ {
			end := e.periodEnd()
			e.toPeriodEnd()
			require.True(t, e.c.entitled(e.ent), "access is continuous across the due boundary, before the renewal pass runs")
			w.runRenewals()
			w.runRenewals() // a second pass in the same period admits nothing
			sub := w.subscription(tp, e.sub)
			require.Equal(t, "active", sub.Status)
			require.True(t, sub.CurrentPeriodEndsAt.After(end), "period extends")
			require.True(t, end.Add(monthHours*time.Hour).Equal(*sub.CurrentPeriodEndsAt), "one period added: %s -> %s", end, sub.CurrentPeriodEndsAt)
			require.True(t, e.c.entitled(e.ent))
			require.Len(t, e.providerLedger(), period, "one provider charge per period")
			require.Equal(t, period, e.providerAttempts())
			paid := completed(w.payments(tp, e.c.id))
			require.Len(t, paid, period, "local payments match provider charges")
		}
		require.Empty(t, w.stripe.unexpected())
		require.Empty(t, w.nmi.unexpected())
	})
}

// Scenario 0: nothing but the runtime's own schedule renews a due membership.
// A process that starts after the boundary renews at once (the due pass runs
// on start); it never waits for a multi-hour dunning tick.
func TestEngineRenewalRunsOnItsOwnSchedule(t *testing.T) {
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			w := newWorld(t)
			e := enroll(t, w, rail, embedded)
			end := e.periodEnd()
			e.toPeriodEnd()
			w.restart()
			require.Eventually(t, func() bool {
				w.settle()
				return w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end)
			}, 30*time.Second, 100*time.Millisecond, "the runtime renews without a manual pass")
			require.Len(t, e.providerLedger(), 2)
			require.True(t, e.c.entitled(e.ent))
		})
	}
}

// Scenario 2: the documented decline policy (docs/operations.md, Dunning):
// a soft decline on a monthly cycle retries at +2d, +5d, +9d and +13d from the
// first failure and terminates (cancelled, access revoked) on the fifth
// failure; a card-fixable decline stops retrying and waits for a new method;
// a non-recoverable decline terminates at once. Engine access is bounded by
// the paid period, so a declined renewal has no grace window.
func TestEngineDeclinePolicy(t *testing.T) {
	cases := []struct {
		name, stripe, nmi string
		armed             bool
		retries           []time.Duration
		final             string
	}{
		{"soft/armed", "insufficient_funds", "202", true, []time.Duration{2 * day, 5 * day, 9 * day, 13 * day}, "cancelled"},
		{"soft/default_switch_off", "insufficient_funds", "202", false, []time.Duration{2 * day, 5 * day, 9 * day, 13 * day}, "past_due"},
		{"fix_payment_method", "expired_card", "223", true, nil, "past_due"},
		{"non_recoverable/armed", "stolen_card", "252", true, nil, "cancelled"},
		{"non_recoverable/default_switch_off", "stolen_card", "252", false, nil, "past_due"},
	}
	for _, rail := range rails {
		for _, tc := range cases {
			t.Run(rail+"/"+tc.name, func(t *testing.T) {
				w := newWorld(t)
				if tc.armed {
					w.armDestructive()
				}
				e := enroll(t, w, rail, remote)
				e.setDecline(visa.Last4, tc.stripe, tc.nmi)
				end := e.periodEnd()
				e.toPeriodEnd()
				first := w.clock.Now()
				w.runRenewals()
				var offsets []time.Duration
				for range 10 {
					sub := w.subscription(e.tp, e.sub)
					require.True(t, sub.CurrentPeriodEndsAt.Equal(end), "a decline never extends the period")
					require.False(t, e.c.entitled(e.ent), "no grace after a decline: engine access ends with the paid period")
					if sub.NextRetryAt == nil {
						break
					}
					require.Equal(t, "past_due", sub.Status)
					offsets = append(offsets, sub.NextRetryAt.Sub(first).Round(time.Hour))
					w.advance(sub.NextRetryAt.Sub(w.clock.Now()) + time.Second)
					w.runRenewals()
				}
				require.Equal(t, tc.retries, offsets, "retry schedule")
				sub := w.subscription(e.tp, e.sub)
				require.Equal(t, tc.final, sub.Status)
				attempts := 1 + len(tc.retries) + 1
				require.Equal(t, attempts, e.providerAttempts(), "one provider attempt per scheduled try")
				require.Len(t, e.providerLedger(), 1, "no charge besides the initial one")
				require.Len(t, completed(w.payments(e.tp, e.c.id)), 1)
				w.advance(40 * day)
				w.runRenewals()
				require.Equal(t, attempts, e.providerAttempts(), "nothing retries after the policy's last attempt")

				// The customer always has a way back with a working card: a
				// cancelled membership re-enrolls, a stopped one takes a new card
				// and recovers at the next due pass.
				if tc.final == "cancelled" {
					fresh := e.c.saveCard(rail, mastercard)
					again := e.c.subscribeAgain(e.tp, rail, e.price, e.ent, fresh)
					require.NotEqual(t, e.sub, again)
				} else {
					e.replaceCard(mastercard)
					w.runRenewals()
					sub := w.subscription(e.tp, e.sub)
					require.Equal(t, "active", sub.Status)
					require.True(t, sub.CurrentPeriodEndsAt.After(w.clock.Now()))
					require.Len(t, e.providerLedger(), 2, "the recovery charge lands on the new card")
					require.Equal(t, mastercard.Last4, e.lastChargedCard())
				}
				require.True(t, e.c.entitled(e.ent))
			})
		}
	}
}

// replaceCard is the customer giving the membership a new card: on Stripe a
// new off-session card selected for the membership, on NMI the membership's
// vault updated in place.
func (e *engineCase) replaceCard(c card) {
	t := e.w.t
	t.Helper()
	if e.rail == "stripe" {
		method := e.c.saveCard("stripe", c)
		e.c.must(http.MethodPut, "/subscriptions/"+e.sub.String()+"/payment-method", "", map[string]any{"payment_method_id": method})
		e.method = method
		return
	}
	e.c.must(http.MethodPut, "/payment-methods/"+e.method, "", map[string]any{"provider": "nmi", "payment_token": e.w.nmi.tokenize(c), "last_four": c.Last4, "card_type": c.Brand, "expiry_date": "12/35"})
	e.w.settle()
}

func (e *engineCase) lastChargedCard() string {
	if e.rail == "stripe" {
		ledger := e.w.stripe.ledger("")
		return e.w.stripe.cardOf(ledger[len(ledger)-1].Method)
	}
	return e.w.nmi.lastSale().Card.Last4
}

const day = 24 * time.Hour

// Scenarios 3 and 4: a new card given during dunning is retried at once and
// recovers the same period; a card changed mid-cycle is the one the next
// renewal charges. The old card is never charged again.
func TestEngineCardReplacement(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		t.Run("during_dunning", func(t *testing.T) {
			w := newWorld(t)
			e := enroll(t, w, rail, tp)
			e.setDecline(visa.Last4, "insufficient_funds", "202")
			end := e.periodEnd()
			e.toPeriodEnd()
			w.runRenewals()
			require.Equal(t, "past_due", w.subscription(tp, e.sub).Status)
			w.advance(day)
			e.replaceCard(mastercard)
			w.runRenewals()
			sub := w.subscription(tp, e.sub)
			require.Equal(t, "active", sub.Status)
			require.True(t, end.Add(monthHours*time.Hour).Equal(*sub.CurrentPeriodEndsAt), "recovery pays the period that was due, not a fresh one")
			require.Equal(t, mastercard.Last4, e.lastChargedCard())
			require.Len(t, e.providerLedger(), 2)
			require.True(t, e.c.entitled(e.ent))
		})
		t.Run("mid_cycle", func(t *testing.T) {
			w := newWorld(t)
			e := enroll(t, w, rail, tp)
			w.advance(10 * day)
			e.replaceCard(mastercard)
			e.toPeriodEnd()
			w.runRenewals()
			require.Equal(t, "active", w.subscription(tp, e.sub).Status)
			require.Equal(t, mastercard.Last4, e.lastChargedCard())
			require.Len(t, e.providerLedger(), 2)
		})
	})
}

// Scenario 5: cancelling keeps access to the end of the paid period and
// never renews; resuming before the end renews as if never cancelled.
func TestEngineCancelAndResume(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		t.Run("cancel_at_period_end", func(t *testing.T) {
			w := newWorld(t)
			e := enroll(t, w, rail, tp)
			w.advance(5 * day)
			e.c.must(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"feedback": "done"})
			w.settle()
			sub := w.subscription(tp, e.sub)
			require.NotNil(t, sub.CancelledAt)
			require.True(t, e.c.entitled(e.ent), "access continues to the paid period end")
			e.toPeriodEnd()
			w.runRenewals()
			require.False(t, e.c.entitled(e.ent), "access ends at the paid period end, with no renewal allowance")
			require.Len(t, e.providerLedger(), 1)
			require.Equal(t, 1, e.providerAttempts(), "a cancelled membership is never charged")
		})
		t.Run("resume_before_end", func(t *testing.T) {
			w := newWorld(t)
			e := enroll(t, w, rail, tp)
			w.advance(5 * day)
			e.c.must(http.MethodPost, "/subscriptions/"+e.sub.String()+"/cancel", "", map[string]any{"feedback": "maybe"})
			w.settle()
			require.True(t, w.subscription(tp, e.sub).Resumable)
			require.NoError(t, w.client[tp].ResumeSubscription(t.Context(), e.sub))
			w.settle()
			sub := w.subscription(tp, e.sub)
			require.Nil(t, sub.CancelledAt)
			require.Equal(t, "active", sub.Status)
			end := e.periodEnd()
			e.toPeriodEnd()
			require.True(t, e.c.entitled(e.ent), "a resumed membership is continuous across the boundary")
			w.runRenewals()
			require.True(t, w.subscription(tp, e.sub).CurrentPeriodEndsAt.After(end))
			require.Len(t, e.providerLedger(), 2)
		})
	})
}

// Scenario 9: the host's account-deletion callback cancels through the
// portable Client (the demo passes RevokeAccess=false). Whatever state the
// membership is in, the cancel succeeds, is idempotent, and nothing renews.
func TestEngineAccountDeletionCancels(t *testing.T) {
	for _, state := range []string{"active", "past_due"} {
		forEach(t, func(t *testing.T, rail string, tp topology) {
			t.Run(state, func(t *testing.T) {
				w := newWorld(t)
				e := enroll(t, w, rail, tp)
				if state == "past_due" {
					e.setDecline(visa.Last4, "insufficient_funds", "202")
					e.toPeriodEnd()
					w.runRenewals()
					require.Equal(t, "past_due", w.subscription(tp, e.sub).Status)
				}
				attempts := e.providerAttempts()
				request := openrails.CancelSubscriptionRequest{Reason: "Account deletion evt_1"}
				require.NoError(t, w.client[tp].CancelSubscription(t.Context(), e.sub, request))
				sub := w.subscription(tp, e.sub)
				require.Equal(t, "cancelled", sub.Status)
				require.NotNil(t, sub.CancelledAt)
				w.advance(90 * day)
				w.runRenewals()
				require.Equal(t, attempts, e.providerAttempts(), "a deleted account is never charged again")
				require.False(t, e.c.entitled(e.ent))
			})
		})
	}
}

// Scenario 7: a price version bump grandfathers existing members at their
// accepted terms; new members buy, and renew at, the new price.
func TestEngineRepricing(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		w := newWorld(t)
		old := enroll(t, w, rail, tp)
		price, err := w.client[tp].Prices.Retrieve(t.Context(), old.price)
		require.NoError(t, err)
		hours := monthHours
		bumped, err := w.client[tp].Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: price.ProductID, Key: price.Key, UnitAmount: 14_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours})
		require.NoError(t, err)
		require.NotEqual(t, price.ID, bumped.ID)
		archived, err := w.client[tp].Prices.Retrieve(t.Context(), price.ID)
		require.NoError(t, err)
		require.True(t, archived.Archived, "the previous version is grandfathered")

		fresh := w.newCustomer()
		method := fresh.saveCard(rail, mastercard)
		newSub := fresh.subscribeAgain(tp, rail, bumped.ID, old.ent, method)
		require.Equal(t, bumped.ID, w.subscription(tp, newSub).PriceID)

		for range 2 {
			old.toPeriodEnd()
			w.runRenewals()
		}
		byCustomer := func(c *customer) (out []int64) {
			for _, p := range completed(w.payments(tp, c.id)) {
				out = append(out, p.Amount)
			}
			return out
		}
		require.Equal(t, []int64{9_990_000, 9_990_000, 9_990_000}, byCustomer(old.c), "the existing member keeps the accepted 9.99")
		require.Equal(t, []int64{14_990_000, 14_990_000, 14_990_000}, byCustomer(fresh), "the new member buys and renews at 14.99")
		require.Len(t, old.providerLedger(), 6, "provider charges match local payments")
		require.Equal(t, price.ID, w.subscription(tp, old.sub).PriceID)
		require.Equal(t, bumped.ID, w.subscription(tp, newSub).PriceID)
	})
}

// Scenario 8: refunding a renewal payment returns the money at the provider
// once, with or without ending that period's access; the membership itself
// continues (a refund is not a cancellation). Archiving the product with
// refunds touches one-time purchases only (documented on ArchiveProduct):
// subscription payments are neither listed nor refunded.
func TestEngineRenewalRefunds(t *testing.T) {
	forEach(t, func(t *testing.T, rail string, tp topology) {
		for _, revoke := range []bool{false, true} {
			t.Run(fmt.Sprintf("revoke_access=%t", revoke), func(t *testing.T) {
				w := newWorld(t)
				e := enroll(t, w, rail, tp)
				e.toPeriodEnd()
				w.runRenewals()
				paid := completed(w.payments(tp, e.c.id))
				require.Len(t, paid, 2)
				renewal := paid[0]
				if paid[1].CreatedAt.After(renewal.CreatedAt) {
					renewal = paid[1]
				}
				refund, err := w.client[tp].RefundPayment(t.Context(), renewal.ID, openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: revoke, IdempotencyKey: "refund-" + renewal.ID.String()})
				require.NoError(t, err)
				w.settle()
				again, err := w.client[tp].RefundPayment(t.Context(), renewal.ID, openrails.RefundPaymentParams{Full: true, Reason: "requested_by_customer", RevokeAccess: revoke, IdempotencyKey: "refund-" + renewal.ID.String()})
				require.NoError(t, err)
				require.Equal(t, refund.ID, again.ID, "a replayed refund is the same refund")
				w.settle()
				got, err := w.client[tp].GetPayment(t.Context(), renewal.ID)
				require.NoError(t, err)
				require.True(t, got.Refunded)
				require.Equal(t, got.Amount, got.AmountRefunded)
				var refunded int64
				for _, entry := range e.providerLedger() {
					refunded += entry.Refunded
				}
				require.EqualValues(t, e.amount, refunded, "exactly one provider refund of the renewal")
				// The provider's own notice of this refund must not re-decide it.
				require.Equal(t, http.StatusOK, w.deliver(rail, w.refundNotice(rail)))
				require.Equal(t, !revoke, e.c.entitled(e.ent), "access follows the merchant's revoke_access choice")
				sub := w.subscription(tp, e.sub)
				if !revoke {
					require.Equal(t, "active", sub.Status, "refunding money alone does not cancel the membership")
					e.toPeriodEnd()
					w.runRenewals()
					require.Len(t, e.providerLedger(), 3, "the membership renews after a refunded period")
					require.True(t, e.c.entitled(e.ent))
					return
				}
				require.Equal(t, "cancelled", sub.Status, "revoking a membership payment's access ends the membership")
				e.toPeriodEnd()
				w.runRenewals()
				require.Len(t, e.providerLedger(), 2, "a revoked membership never renews")
				require.False(t, e.c.entitled(e.ent))
			})
		}
		t.Run("archive_with_refund", func(t *testing.T) {
			w := newWorld(t)
			e := enroll(t, w, rail, tp)
			price, err := w.client[tp].Prices.Retrieve(t.Context(), e.price)
			require.NoError(t, err)
			archive, err := w.client[tp].ArchiveProduct(t.Context(), openrails.ArchiveProductParams{ProductID: price.ProductID, Action: openrails.PurchaseActionRefund, Window: 365 * day, Reason: "retired", IdempotencyKey: "archive-" + price.ProductID})
			require.NoError(t, err)
			w.settle()
			require.True(t, archive.Complete)
			require.Empty(t, archive.Purchases, "subscription payments are not archive purchases")
			for _, entry := range e.providerLedger() {
				require.Zero(t, entry.Refunded)
			}
			e.toPeriodEnd()
			w.runRenewals()
			require.Len(t, e.providerLedger(), 2, "the grandfathered member still renews")
			require.True(t, e.c.entitled(e.ent))
		})
	})
}
