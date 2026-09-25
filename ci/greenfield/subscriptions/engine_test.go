//go:build greenfield && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/riverqueue/river"
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
	return enrollEvery(t, w, rail, tp, monthHours)
}

// enrollEvery is enroll on a membership renewing every hours.
func enrollEvery(t *testing.T, w *world, rail string, tp topology, hours int) *engineCase {
	t.Helper()
	price := w.membershipEvery("content:members", 9_990_000, hours)
	e := &engineCase{w: w, rail: rail, tp: tp, price: price.ID, amount: 999, ent: "content:members", started: w.clock.Now()}
	e.c = w.newCustomer()
	before := len(e.providerLedger())
	e.method = e.c.saveCard(rail, visa)
	e.sub = e.c.subscribe(tp, rail, price.ID, e.ent, e.method)
	sub := w.subscription(tp, e.sub)
	require.Equal(t, "engine", sub.CollectionPolicy)
	require.Empty(t, sub.RailSubscriptionID)
	require.Equal(t, "active", sub.Status)
	require.Len(t, e.providerLedger(), before+1, "one initial charge")
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
			t.Run(fmt.Sprintf("%s/%s", rail, tp), func(t *testing.T) {
				t.Parallel()
				run(t, rail, tp)
			})
		}
	}
}

// Scenarios 0, 1 and 10: with the demo's embedded posture a due renewal
// executes (never parks); each period charges exactly once, extends the
// period and keeps access continuous; local payments equal provider charges.
func TestEngineHappyRenewals(t *testing.T) {
	t.Parallel()
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
			e.requireLedgerAgreement(paid)
		}
		require.Empty(t, w.stripe.unexpected())
		require.Empty(t, w.nmi.unexpected())
	})
}

// requireLedgerAgreement: every local payment is a provider charge of the
// same amount, and every provider charge is a local payment (no orphan, no
// duplicate).
func (e *engineCase) requireLedgerAgreement(local []openrails.Payment) {
	t := e.w.t
	t.Helper()
	provider := map[string]int64{}
	for _, entry := range e.providerLedger() {
		provider[entry.ID] = entry.Amount
		if entry.Charge != "" {
			provider[entry.Charge] = entry.Amount
		}
	}
	seen := map[string]bool{}
	for _, p := range local {
		cents, ok := provider[p.TransactionID]
		require.True(t, ok, "local payment %s has a provider charge", p.TransactionID)
		require.Equal(t, cents*10_000, p.Amount, "amounts agree")
		require.False(t, seen[p.TransactionID], "no duplicate local payment")
		seen[p.TransactionID] = true
	}
	require.Len(t, seen, len(e.providerLedger()), "no provider charge without a local payment")
}

// Scenario 0: what each provider-write posture does with a due renewal. Only
// full executes it; the others hold it without loss. Unset is refused at
// construction, since it would silently never charge.
func TestEngineRenewalPostures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		apply func(*config.Config)
		renew bool
	}{
		{"full", func(*config.Config) {}, true},
		{"readonly", func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeReadOnly }, false},
		{"limited", func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeLimited }, false},
		{"engine_admission_hold", func(c *config.Config) { c.EngineAdmissionHold = true }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "stripe", embedded)
			end := e.periodEnd()
			w.cfg = tc.apply
			w.restart()
			e.toPeriodEnd()
			w.runRenewals()
			require.Equal(t, tc.renew, w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end))
			if !tc.renew {
				require.Equal(t, 1, e.providerAttempts(), "no charge outside mode=full")
				// Switching to full picks the held renewal up; nothing was lost.
				w.cfg = nil
				w.restart()
				w.advance(10 * time.Minute) // past the park re-check interval
				w.runRenewals()
				w.wake()
				require.True(t, w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end))
				require.Len(t, e.providerLedger(), 2, "exactly one renewal charge once allowed")
			}
		})
	}
}

func TestEmbeddedRequiresWriteMode(t *testing.T) {
	t.Parallel()
	pool, err := pgxpool.New(t.Context(), dsn(t))
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	_, err = embed.New(t.Context(), embed.Options{Config: &config.Config{TestMode: config.CredentialPostureSandbox, DB: &config.DBConfig{URL: dsn(t)}}, PGXPool: pool, River: embed.RiverFromHost()})
	require.ErrorContains(t, err, "ProviderWriteMode is required")
}

// Scenario 0: nothing but the runtime's own schedule renews a due membership.
// A process that starts after the boundary renews at once (the due pass runs
// on start); it never waits for a multi-hour dunning tick.
func TestEngineRenewalRunsOnItsOwnSchedule(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, rail, embedded)
			end := e.periodEnd()
			e.toPeriodEnd()
			w.restart()
			// The due pass runs on start and every minute, once per minute: a
			// restart inside the minute its predecessor ran waits for the next.
			deadline := time.Now().Add(90 * time.Second)
			for !w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end) {
				require.True(t, time.Now().Before(deadline), "the runtime renews without a manual pass")
				w.settle()
				time.Sleep(100 * time.Millisecond)
			}
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
	t.Parallel()
	cases := []struct {
		name, stripe, nmi string
		armed             bool
		retries           []time.Duration
		final             string
	}{
		{"soft/armed", "insufficient_funds", "202", true, []time.Duration{2 * day, 5 * day, 9 * day, 13 * day}, "cancelled"},
		{"soft/default_switch_off", "insufficient_funds", "202", false, []time.Duration{2 * day, 5 * day, 9 * day, 13 * day}, "past_due"},
		{"fix_payment_method", "expired_card", "223", true, nil, "awaiting_method"},
		{"non_recoverable/armed", "stolen_card", "252", true, nil, "cancelled"},
		{"non_recoverable/default_switch_off", "stolen_card", "252", false, nil, "past_due"},
	}
	for _, rail := range rails {
		for _, tc := range cases {
			t.Run(rail+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
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
				if !tc.armed && tc.name != "fix_payment_method" {
					status, body := w.staff(http.MethodGet, "/v1/merchant/findings")
					require.Equal(t, http.StatusOK, status)
					require.Contains(t, body, "life.terminal_outcome.held", "a held terminal outcome is visible to the operator")
				}
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

const (
	day                = 24 * time.Hour
	subscriptionsGrace = 24*time.Hour + time.Minute
)

// Scenarios 3 and 4: a new card given during dunning is retried at once and
// recovers the same period; a card changed mid-cycle is the one the next
// renewal charges. The old card is never charged again.
func TestEngineCardReplacement(t *testing.T) {
	t.Parallel()
	forEach(t, func(t *testing.T, rail string, tp topology) {
		t.Run("during_dunning", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
	forEach(t, func(t *testing.T, rail string, tp topology) {
		t.Run("cancel_at_period_end", func(t *testing.T) {
			t.Parallel()
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
			t.Parallel()
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
	t.Parallel()
	for _, state := range []string{"active", "past_due"} {
		forEach(t, func(t *testing.T, rail string, tp topology) {
			t.Run(state, func(t *testing.T) {
				t.Parallel()
				w := newWorld(t)
				e := enroll(t, w, rail, tp)
				if state == "past_due" {
					e.setDecline(visa.Last4, "insufficient_funds", "202")
					e.toPeriodEnd()
					w.runRenewals()
					require.Equal(t, "past_due", w.subscription(tp, e.sub).Status)
				}
				attempts := e.providerAttempts()
				request := openrails.CancelSubscriptionRequest{Reason: "Account deletion evt_1", AccountDeletion: true}
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
	t.Parallel()
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
	t.Parallel()
	forEach(t, func(t *testing.T, rail string, tp topology) {
		for _, revoke := range []bool{false, true} {
			t.Run(fmt.Sprintf("revoke_access=%t", revoke), func(t *testing.T) {
				t.Parallel()
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
			t.Parallel()
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

// A declined first payment resolves promptly as a refusal: no membership, no
// charge, and the buyer can enroll again with another card.
func TestEngineInitialDeclineResolves(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			price := w.membership("content:members", 9_990_000)
			c := w.newCustomer()
			declined := c.saveCard(rail, card{Brand: "visa", Last4: "0002", Decline: map[string]string{"stripe": "insufficient_funds", "nmi": "202"}[rail]})
			session, err := w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
				OfferKind: openrails.OfferRecurring, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: "content:members", PriceID: price.ID,
				IdempotencyKey: "enroll-declined", PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp[rail], Rail: rail, PaymentMethodID: declined},
				SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
			})
			require.NoError(t, err)
			_, confirm := c.call(http.MethodPost, "/checkout/"+session.ID+"/confirm", "", map[string]any{"payment": map[string]string{"rail": rail}})
			t.Logf("confirm: %v", confirm)
			w.settle()
			w.until(func() bool {
				return unwrap(c.must(http.MethodGet, "/checkout/"+session.ID, "", nil))["status"] == "failed"
			}, "the declined enrollment resolves as failed")
			subs, err := w.client[embedded].ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: c.id})
			require.NoError(t, err)
			require.Empty(t, subs.Data)
			require.False(t, c.entitled("content:members"))
			fresh := c.saveCard(rail, visa)
			c.subscribeAgain(embedded, rail, price.ID, "content:members", fresh)
			require.True(t, c.entitled("content:members"))
		})
	}
}

// An initial payment awaiting issuer authentication that the buyer abandons
// must not hold the product hostage: once the checkout lapses the engine
// closes the payment, and the buyer can enroll again.
func TestEngineAbandonedAuthenticationReleases(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	price := w.membership("content:members", 9_990_000)
	c := w.newCustomer()
	challenged := c.saveCard("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
	session, err := w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
		OfferKind: openrails.OfferRecurring, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: "content:members", PriceID: price.ID,
		IdempotencyKey: "enroll-3ds", PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp["stripe"], Rail: "stripe", PaymentMethodID: challenged},
		SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
	})
	require.NoError(t, err)
	_, confirm := c.call(http.MethodPost, "/checkout/"+session.ID+"/confirm", "", map[string]any{"payment": map[string]string{"rail": "stripe"}})
	t.Logf("confirm: %v", confirm)
	w.settle()
	w.advance(2 * day)
	w.wake()
	w.until(func() bool {
		return unwrap(c.must(http.MethodGet, "/checkout/"+session.ID, "", nil))["status"] == "failed"
	}, "the abandoned challenge fails the enrollment")
	require.Len(t, w.stripe.mutations("/v1/payment_intents/"), 1, "the challenged payment itself is closed, once")
	fresh := c.saveCard("stripe", visa)
	c.subscribeAgain(embedded, "stripe", price.ID, "content:members", fresh)
	require.True(t, c.entitled("content:members"))
	require.Len(t, w.stripe.ledger(""), 1, "only the second card was charged")
}

// A renewal the issuer challenges keeps access through the renewal grace
// while the member may authenticate; if they never do, the engine closes the
// payment, the membership waits for a new card, and a new card recovers.
func TestEngineRenewalAuthenticationAbandoned(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "stripe", embedded)
	e.setDecline(visa.Last4, "auth", "")
	e.toPeriodEnd()
	w.runRenewals()
	require.Equal(t, "active", w.subscription(embedded, e.sub).Status, "an open challenge is not a decline")
	require.True(t, e.c.entitled(e.ent), "access holds while the member can still authenticate")
	w.advance(25 * time.Hour)
	w.until(func() bool { return w.subscription(embedded, e.sub).Status == "awaiting_method" }, "the abandoned renewal waits for a new card")
	require.False(t, e.c.entitled(e.ent))
	require.Empty(t, e.providerLedger()[1:], "the challenged renewal never charged")
	e.replaceCard(mastercard)
	w.runRenewals()
	require.Equal(t, "active", w.subscription(embedded, e.sub).Status)
	require.True(t, e.c.entitled(e.ent))
}

// NMI's duplicate check refuses a request whose card and amount match a recent
// charge. A scheduled renewal refused this way is unknown, not released, and
// is never re-sent: with no matching charge under its own order it is an
// operator finding. A customer-present enrollment is simply refused.
func TestEngineNMIDuplicateRefusal(t *testing.T) {
	t.Parallel()
	t.Run("renewal", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		e := enroll(t, w, "nmi", embedded)
		end := e.periodEnd()
		e.toPeriodEnd()
		before := len(w.nmi.saleOrders())
		w.nmi.refuseDuplicates(1)
		w.runRenewals()
		w.until(func() bool { return len(w.openFindings("life.submission.unresolved")) == 1 }, "the unexplained duplicate is an operator finding")
		for range 6 {
			w.advance(time.Hour)
			w.wake()
		}
		require.Len(t, w.nmi.saleOrders()[before:], 1, "a duplicate refusal is never re-sent")
		require.Len(t, e.providerLedger(), 1, "the refused renewal moved no money")
		require.True(t, w.subscription(embedded, e.sub).CurrentPeriodEndsAt.Equal(end))
	})
	t.Run("enrollment", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		price := w.membership("content:members", 9_990_000)
		c := w.newCustomer()
		method := c.saveCard("nmi", visa)
		w.nmi.refuseDuplicates(1)
		session, err := w.client[embedded].CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
			OfferKind: openrails.OfferRecurring, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: "content:members", PriceID: price.ID,
			IdempotencyKey: "enroll-dup", PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp["nmi"], Rail: "nmi", PaymentMethodID: method},
			SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
		})
		require.NoError(t, err)
		c.call(http.MethodPost, "/checkout/"+session.ID+"/confirm", "", map[string]any{"payment": map[string]string{"rail": "nmi"}})
		w.until(func() bool {
			return unwrap(c.must(http.MethodGet, "/checkout/"+session.ID, "", nil))["status"] == "failed"
		}, "the refused enrollment resolves")
		c.subscribeAgain(embedded, "nmi", price.ID, "content:members", method)
		require.Len(t, w.nmi.ledger(""), 1)
		require.True(t, c.entitled("content:members"))
	})
}

// Scenario 6: a process dies mid-renewal and a new one takes over the same
// database. Before submission the renewal simply runs after restart. After a
// submission whose response was lost, recovery adopts the provider's charge.
// A request that never reached the provider is re-sent under the same
// reference once the provider's read settles on "no transaction" (the soak's
// SIGKILL case). Every path ends with exactly one renewal charge.
func TestEngineCrashDurability(t *testing.T) {
	t.Parallel()
	submit := map[string]func(*http.Request) bool{
		"stripe": func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents" },
		"nmi":    func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/transact.php") },
	}
	beforeSubmit := map[string]func(*http.Request) bool{
		// Stripe's first provider call is the submission itself; the crash
		// lands after admission, before the claim reaches the provider.
		"stripe": func(r *http.Request) bool { return r.Method == http.MethodPost && r.URL.Path == "/v1/payment_intents" },
		"nmi": func(r *http.Request) bool {
			return r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v5/customers/")
		},
	}
	cases := []struct {
		name   string
		match  map[string]func(*http.Request) bool
		commit bool
		hard   bool
	}{
		{"before_submit", beforeSubmit, false, false},
		{"after_submit_response_lost", submit, true, false},
		{"lost_before_provider", submit, false, false},
		{"hard_kill_after_submit", submit, true, true},
		{"hard_kill_lost_before_provider", submit, false, true},
	}
	for _, rail := range rails {
		for _, tc := range cases {
			if rail == "stripe" && tc.name == "before_submit" {
				continue // identical wire point to lost_before_provider on Stripe
			}
			t.Run(rail+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				w := newWorld(t)
				e := enroll(t, w, rail, embedded)
				end := e.periodEnd()
				e.toPeriodEnd()
				var g *gate
				if rail == "stripe" {
					g = w.stripe.hold(newGate(tc.match[rail], tc.commit))
				} else {
					g = w.nmi.hold(newGate(tc.match[rail], tc.commit))
				}
				_, err := w.jobs.Insert(t.Context(), dunningPass{}, &river.InsertOpts{Queue: embed.QueueBilling})
				require.NoError(t, err)
				promoteCtx, stopPromoting := context.WithCancel(t.Context())
				promoterDone := make(chan struct{})
				go func() {
					defer close(promoterDone)
					ticker := time.NewTicker(20 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-promoteCtx.Done():
							return
						case <-ticker.C:
							w.settleQuiet()
						}
					}
				}()
				defer func() { stopPromoting(); <-promoterDone }()
				select {
				case <-g.arrived:
				case <-time.After(20 * time.Second):
					t.Fatal("the renewal never reached the provider")
				}
				// Join the helper before stop replaces the fleet pointer.
				stopPromoting()
				<-promoterDone
				if tc.hard {
					w.kill() // nothing the dying process was doing is recorded
				} else {
					w.stop() // the process dies with the request in flight
				}
				w.stripe.unhold()
				w.nmi.unhold()
				w.start()
				w.advance(30 * time.Minute) // past any executor lease
				if tc.hard {
					w.rescue() // the dead process's job, silent past the rescue window
				}
				w.wake()
				w.runRenewals()
				w.until(func() bool { return w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end) }, "the renewal resolves")
				w.advance(time.Hour)
				w.wake()
				require.Len(t, e.providerLedger(), 2, "exactly one renewal charge")
				require.Len(t, completed(w.payments(embedded, e.c.id)), 2, "local payments match the provider")
				require.True(t, e.c.entitled(e.ent))
			})
		}
	}
}

// A submitted renewal the provider has no record of is decided by the
// provider's authoritative read: nothing after the settle delay re-sends the
// same operation (capped); a recorded decline or charge is adopted; an
// unavailable read never re-sends and raises an operator finding.
func TestEngineLostSubmission(t *testing.T) {
	t.Parallel()
	for _, rail := range rails {
		t.Run(rail+"/lost_in_transit", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, rail, embedded)
			end := e.periodEnd()
			e.toPeriodEnd()
			w.loseSubmissions(rail, 1)
			w.runRenewals()
			w.until(func() bool { return w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end) }, "the lost renewal is re-sent")
			require.Len(t, e.providerLedger(), 2, "exactly one renewal charge")
			require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
			require.True(t, e.c.entitled(e.ent))
			require.Empty(t, w.openFindings("life.submission.unresolved"))
		})
		t.Run(rail+"/read_unavailable", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, rail, embedded)
			end := e.periodEnd()
			e.toPeriodEnd()
			w.loseSubmissions(rail, 1)
			w.readUnavailable(rail, true)
			w.runRenewals()
			w.until(func() bool { return len(w.openFindings("life.submission.unresolved")) == 1 }, "the unreadable submission is an operator finding")
			for range 6 {
				w.advance(time.Hour)
				w.wake()
			}
			require.Equal(t, 1, e.providerAttempts(), "never re-sent while the provider cannot be read")
			require.False(t, w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end))
			w.readUnavailable(rail, false)
			w.until(func() bool { return w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end) }, "the renewal resolves once the provider answers")
			require.Len(t, e.providerLedger(), 2)
			require.Empty(t, w.openFindings("life.submission.unresolved"), "the finding closes with the operation")
		})
		t.Run(rail+"/resend_cap", func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, rail, embedded)
			end := e.periodEnd()
			e.toPeriodEnd()
			w.loseSubmissions(rail, 10)
			w.runRenewals()
			w.until(func() bool { return len(w.openFindings("life.submission.unresolved")) == 1 }, "the spent cap is an operator finding")
			for range 6 {
				w.advance(time.Hour)
				w.wake()
			}
			require.Len(t, e.providerLedger(), 1, "nothing reached the provider")
			require.Equal(t, 3, w.lostSubmissions(rail), "the original and two resends, no more")
			require.False(t, w.subscription(embedded, e.sub).CurrentPeriodEndsAt.After(end))
		})
	}
	t.Run("nmi/declined_answer_lost", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t)
		e := enroll(t, w, "nmi", embedded)
		e.toPeriodEnd()
		w.nmi.setDecline(visa.Last4, "202")
		w.nmi.dropResponses(1)
		w.runRenewals()
		w.until(func() bool { return w.subscription(embedded, e.sub).Status == "past_due" }, "the recorded decline is adopted")
		require.Equal(t, 2, e.providerAttempts(), "the declined renewal is not re-sent")
		require.Len(t, e.providerLedger(), 1)
	})
}

// One membership the due pass cannot process never fails the pass: every
// other due renewal still runs, the pass completes, and the refusal is a
// standing operator finding. (The broken row is the soak's shape: a paid
// period end moved without a matching accepted agreement.)
func TestEngineDuePassIsolatesRefusals(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	healthy := enroll(t, w, "stripe", embedded)
	broken := enroll(t, w, "nmi", embedded)
	_, err := w.pool.Exec(t.Context(), `UPDATE `+pgx.Identifier{w.schema}.Sanitize()+`.subscriptions SET lifecycle_rev = lifecycle_rev + 1, current_period_ends_at = current_period_ends_at - interval '1 day' WHERE id = $1`, strings.TrimPrefix(broken.sub.String(), "sub_"))
	require.NoError(t, err)
	end := healthy.periodEnd()
	healthy.toPeriodEnd()
	w.runRenewals() // waits for the pass to COMPLETE, not retry
	require.True(t, w.subscription(embedded, healthy.sub).CurrentPeriodEndsAt.After(end), "the healthy member renews")
	require.Len(t, broken.providerLedger(), 1, "the refused member is not charged")
	status, body := w.staff(http.MethodGet, "/v1/merchant/findings")
	require.Equal(t, http.StatusOK, status)
	require.Contains(t, body, "life.due_pass.refused")
	require.Contains(t, body, strings.TrimPrefix(broken.sub.String(), "sub_"))
}

// The same bound holds for a one-time purchase on a saved card: a buyer who
// abandons the issuer challenge is not left with a purchase that blocks them.
func TestOneTimeAbandonedAuthenticationReleases(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	client := w.client[embedded]
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "post-" + uuid.NewString()[:8], DisplayName: "Paid post", EntitlementsSpec: map[string]*int{"content:post": nil}})
	require.NoError(t, err)
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 4_990_000, Currency: "USD"})
	require.NoError(t, err)
	c := w.newCustomer()
	buy := func(method, key string) *openrails.CheckoutSession {
		session, err := client.CreateCheckoutSession(t.Context(), openrails.CreateCheckoutSessionRequest{
			OfferKind: openrails.OfferPermanent, Customer: openrails.CheckoutCustomerIdentity{ID: c.id}, Entitlement: "content:post", PriceID: price.ID,
			IdempotencyKey: key, PaymentOptions: openrails.CheckoutPaymentOptions{PSPID: w.psp["stripe"], Rail: "stripe", PaymentMethodID: method},
			SuccessURL: "https://greenfield.test/return", CancelURL: "https://greenfield.test/return?canceled=1",
		})
		require.NoError(t, err)
		return session
	}
	challenged := c.saveCard("stripe", card{Brand: "visa", Last4: "3155", Decline: "auth"})
	first := buy(challenged, "buy-3ds")
	t.Logf("first: %s", first.Status)
	w.advance(2 * time.Hour)
	w.until(func() bool {
		return unwrap(c.must(http.MethodGet, "/checkout/"+first.ID, "", nil))["status"] == "failed"
	}, "the abandoned challenge fails the purchase")
	again := buy(c.saveCard("stripe", visa), "buy-again")
	w.settle()
	require.True(t, c.entitled("content:post"), "status %s", again.Status)
	require.Len(t, w.stripe.ledger(""), 1)
}
