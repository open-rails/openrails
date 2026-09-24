//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// Member actions on legacy NMI-billed (provider-owned) subscriptions through
// the OpenRails Client (embedded and remote) and the member's /v1/me routes.
// NMI owns the schedule: every action either changes NMI exactly once or is
// refused with a typed error and no NMI write.

const providerCancelHeld = "life.provider_cancel.held"

// meCancel is the member cancelling one subscription on /v1/me.
func (l *legacy) meCancel(sub openrails.SubscriptionID) (int, map[string]any) {
	return l.c.call(http.MethodPost, "/subscriptions/"+sub.String()+"/cancel", "", map[string]any{"feedback": "too expensive"})
}

func errorCode(body map[string]any) string {
	if e, ok := body["error"].(map[string]any); ok {
		code, _ := e["code"].(string)
		return code
	}
	return ""
}

// Cancel at period end or immediately, by the merchant (either Client) or
// the member (/v1/me), with the destructive switch armed or not. Armed: the
// NMI schedule is deleted exactly once. Disarmed: a typed refusal changes
// nothing and raises one operator finding; after arming the same cancel works.
// An account deletion is irrevocable: it cancels locally while disarmed and
// its held delete runs once armed.
func TestLegacyNMICancel(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name                   string
		by                     string // merchant | member
		armed, revoke, account bool
	}{
		{"merchant at period end", "merchant", true, false, false},
		{"merchant immediate", "merchant", true, true, false},
		{"merchant disarmed", "merchant", false, false, false},
		{"account deletion disarmed", "merchant", false, false, true},
		{"member at period end", "member", true, false, false},
		{"member disarmed", "member", false, false, false},
	}
	for _, row := range rows {
		for _, tp := range []topology{embedded, remote} {
			if row.by == "member" && tp == remote {
				continue // the member has one surface: /v1/me
			}
			t.Run(fmt.Sprintf("%s/%s", row.name, tp), func(t *testing.T) {
				t.Parallel()
				w := newWorld(t)
				if row.armed {
					w.armDestructive()
				}
				l := importLegacy(t, w, "nmi", tp)
				w.converge()
				end := l.periodEnd()
				charges := l.engineCharges()

				cancel := func() (int, string) {
					if row.by == "member" {
						status, body := l.meCancel(l.sub)
						w.settle()
						return status, errorCode(body)
					}
					err := w.client[tp].CancelSubscription(t.Context(), l.sub, openrails.CancelSubscriptionRequest{Reason: "member asked", RevokeAccess: row.revoke, AccountDeletion: row.account})
					w.settle()
					if err != nil {
						requireCode(t, err, http.StatusConflict, openrails.CodeProviderCancelHeld)
						return http.StatusConflict, openrails.CodeProviderCancelHeld
					}
					return http.StatusOK, ""
				}

				status, code := cancel()
				if !row.armed && !row.account {
					require.Equal(t, http.StatusConflict, status)
					require.Equal(t, openrails.CodeProviderCancelHeld, code)
					require.Equal(t, "active", w.subscription(tp, l.sub).Status, "a refused cancel changes nothing")
					require.Contains(t, w.openFindings(providerCancelHeld), l.sub.UUID().String())
					w.advance(time.Hour)
					w.wake()
					require.Zero(t, w.nmi.deletesOf(l.railSub))
					require.True(t, w.nmi.scheduleLive(l.railSub))
					w.armDestructive()
					status, _ = cancel()
					require.Less(t, status, 300, "the same cancel works once armed")
					require.NotContains(t, w.openFindings(providerCancelHeld), l.sub.UUID().String(), "the accepted cancel closes the finding")
				}
				require.Less(t, status, 300)
				sub := w.subscription(tp, l.sub)
				require.Equal(t, "cancelled", sub.Status)
				require.True(t, sub.CurrentPeriodEndsAt.Equal(end), "the paid period is not rewritten")

				if row.account && !row.armed {
					require.Contains(t, w.openFindings(providerCancelHeld), l.sub.UUID().String())
					w.advance(time.Hour)
					w.wake()
					require.Zero(t, w.nmi.deletesOf(l.railSub), "the delete waits for the operator's switch")
					w.armDestructive()
				}
				if row.by == "member" {
					// At period end: the schedule survives the undo window and
					// is deleted before NMI's next billing date.
					w.advance(time.Hour)
					w.wake()
					require.Zero(t, w.nmi.deletesOf(l.railSub), "the member's undo window keeps the schedule")
					w.advance(end.Sub(w.clock.Now()) - 47*time.Hour)
				}
				w.until(func() bool { return w.nmi.deletesOf(l.railSub) > 0 }, "the NMI schedule delete")
				w.advance(time.Hour)
				w.wake()
				require.Equal(t, 1, w.nmi.deletesOf(l.railSub), "exactly one NMI delete")
				require.False(t, w.nmi.scheduleLive(l.railSub))
				require.Equal(t, !row.revoke, l.c.entitled(l.ent), "access follows at-period-end vs immediate")
				w.advance(end.Sub(w.clock.Now()) + time.Hour)
				w.runRenewals()
				require.False(t, l.c.entitled(l.ent), "access ends with the paid period")
				require.Equal(t, charges, l.engineCharges(), "OpenRails never charges a legacy subscription")
				require.Empty(t, w.nmi.unexpected())
			})
		}
	}
}

// importAnother lands a second legacy NMI subscription, on another product,
// for the same customer and vault.
func (l *legacy) importAnother(t *testing.T) (openrails.SubscriptionID, string, string) {
	t.Helper()
	w, client := l.w, l.w.client[l.tp]
	ent := "content:legacy-other"
	product, err := client.Products.Create(t.Context(), &openrails.ProductCreateParams{Key: "legacy-other-" + uuid.NewString()[:8], DisplayName: "Legacy extra", EntitlementsSpec: map[string]*int{ent: nil}})
	require.NoError(t, err)
	hours := monthHours
	plan := "legacy_plan_" + uuid.NewString()[:8]
	price, err := client.Prices.Create(t.Context(), &openrails.PriceCreateParams{ProductID: product.ID, Key: product.Key + "-usd", UnitAmount: 9_990_000, Currency: "USD", AutoRenew: true, AccessDurationHours: &hours,
		PSPLinks: map[string]map[string]string{"nmi": {"plan_id": plan}}})
	require.NoError(t, err)
	start := w.clock.Now().Add(-5 * day)
	end := start.Add(monthHours * time.Hour)
	railSub := w.nmi.legacySchedule(l.railCust, plan, "9.99", end)
	customerID, err := openrails.ParseCustomerID(l.c.id)
	require.NoError(t, err)
	priceID, err := openrails.ParsePriceID(price.ID)
	require.NoError(t, err)
	method := &openrails.PaymentMethodRef{Rail: "nmi", RailCustomerRef: l.railCust, RailMethodRef: w.nmi.billingOf(l.railCust)}
	result, err := client.ImportBilling(t.Context(), openrails.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: openrails.PSPRef{Key: "nmi"},
		Customers: []openrails.DeclaredCustomer{{Customer: customerID}},
		Subscriptions: []openrails.DeclaredSubscription{{SourceID: "legacy-" + railSub, Customer: customerID, Price: priceID, Rail: "nmi", RailSubscriptionID: railSub,
			StartedAt: start, PaidThrough: &end, PaymentMethod: method, CollectionPolicy: "provider"}},
		Transactions: []openrails.DeclaredTransaction{{RailSubscriptionID: railSub, TransactionID: w.nmi.legacySale(l.railCust, "9.99", start), Success: true, AmountCents: 999, Currency: "USD", OccurredAt: start}},
	})
	require.NoError(t, err)
	require.Len(t, result.Imported, 1, "%+v", result)
	w.settle()
	subs, err := client.ListSubscriptions(t.Context(), openrails.SubscriptionFilter{CustomerID: l.c.id})
	require.NoError(t, err)
	require.Len(t, subs.Data, 2)
	for _, s := range subs.Data {
		if s.RailSubscriptionID == railSub {
			return s.ID, railSub, ent
		}
	}
	t.Fatal("second legacy subscription not listed")
	return openrails.SubscriptionID{}, "", ""
}

// A member with two legacy subscriptions cancels exactly the one they named
// on /v1/me; the other keeps billing at NMI.
func TestLegacyNMIMemberCancelsTheNamedSubscription(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	l := importLegacy(t, w, "nmi", embedded)
	second, secondRail, secondEnt := l.importAnother(t)
	w.converge()

	status, body := l.meCancel(second)
	require.Equal(t, http.StatusAccepted, status, "%v", body)
	w.settle()
	require.Equal(t, "cancelled", w.subscription(embedded, second).Status)
	require.Equal(t, "active", w.subscription(embedded, l.sub).Status, "the other membership is untouched")

	w.advance(w.subscription(embedded, second).CurrentPeriodEndsAt.Sub(w.clock.Now()) - 47*time.Hour)
	w.until(func() bool { return w.nmi.deletesOf(secondRail) > 0 }, "the named schedule's delete")
	require.Equal(t, 1, w.nmi.deletesOf(secondRail))
	require.Zero(t, w.nmi.deletesOf(l.railSub), "the other schedule is never deleted")
	require.True(t, w.nmi.scheduleLive(l.railSub))
	require.True(t, l.c.entitled(l.ent))
	require.True(t, l.c.entitled(secondEnt), "the cancelled membership keeps its paid period")
	require.Empty(t, w.nmi.unexpected())
}

// A card update repoints the NMI schedule at the new vault exactly once, and
// NMI's next renewal on that card is mirrored. Another card of the vault NMI
// already bills is refused: NMI charges a vault's primary card.
func TestLegacyNMICardUpdate(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"merchant-embedded", "merchant-remote", "member"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			tp := embedded
			if by == "merchant-remote" {
				tp = remote
			}
			l := importLegacy(t, w, "nmi", tp)
			w.converge()
			update := func(method string) error {
				if by == "member" {
					status, body := l.c.call(http.MethodPut, "/subscriptions/"+l.sub.String()+"/payment-method", "", map[string]any{"payment_method_id": method})
					if status >= 300 {
						return fmt.Errorf("%d %s", status, errorCode(body))
					}
					return nil
				}
				id, err := openrails.ParsePaymentMethodID(method)
				require.NoError(t, err)
				return w.client[tp].UpdateSubscriptionPaymentMethod(t.Context(), l.sub, openrails.UpdateSubscriptionPaymentMethodRequest{PaymentMethodID: id})
			}
			updates := func() []providerCall {
				return w.nmi.callsTo(http.MethodPost, "transact.php", func(f url.Values) bool { return f.Get("recurring") == "update_subscription" })
			}

			// Another card of the same vault: refused, no NMI write.
			other := w.nmi.addCard(l.railCust, mastercard)
			customerID, err := openrails.ParseCustomerID(l.c.id)
			require.NoError(t, err)
			_, err = w.client[tp].ImportBilling(t.Context(), openrails.DeclaredBilling{AsOf: w.clock.Now(), DefaultPSP: openrails.PSPRef{Key: "nmi"},
				PaymentMethods: []openrails.DeclaredPaymentMethod{{Customer: customerID, Rail: "nmi", RailCustomerRef: l.railCust, RailMethodRef: other, LastFour: mastercard.Last4, CardType: "mastercard", ExpiryDate: "12/35"}}})
			require.NoError(t, err)
			methods, err := w.client[tp].ListPaymentMethods(t.Context(), l.c.id, openrails.PageOptions{Limit: 20})
			require.NoError(t, err)
			sameVault := ""
			for _, m := range methods.Data {
				if m.Card != nil && m.Card.Last4 != nil && *m.Card.Last4 == mastercard.Last4 {
					sameVault = m.ID
				}
			}
			require.NotEmpty(t, sameVault)
			err = update(sameVault)
			require.Error(t, err)
			if by == "member" {
				require.Equal(t, "409 "+openrails.CodePaymentMethodSameVault, err.Error())
			} else {
				requireCode(t, err, http.StatusConflict, openrails.CodePaymentMethodSameVault)
			}
			require.Empty(t, updates(), "a refused swap never reaches NMI")

			// A new card in its own vault.
			method := l.c.saveCard("nmi", mastercard)
			require.NoError(t, update(method))
			w.settle()
			calls := updates()
			require.Len(t, calls, 1, "exactly one NMI payment-source update")
			require.Equal(t, l.railSub, calls[0].Form.Get("subscription_id"))
			newVault := calls[0].Form.Get("customer_vault_id")
			require.NotEqual(t, l.railCust, newVault)
			require.Equal(t, newVault, w.nmi.scheduleState(l.railSub).Vault)
			sub := w.subscription(tp, l.sub)
			require.NotNil(t, sub.PaymentMethodID)
			require.Equal(t, method, sub.PaymentMethodID.String())

			// NMI's next renewal bills the new card and is mirrored once.
			charges := l.engineCharges()
			end := l.periodEnd()
			w.advance(end.Sub(w.clock.Now()) + time.Hour)
			sale, _ := w.nmi.providerRenew(l.railSub, true)
			require.Equal(t, mastercard.Last4, sale.Card.Last4)
			notice := nmiEvent("transaction.sale.success", obj{"transaction_id": sale.TransactionID, "transaction_type": "cc", "condition": "pendingsettlement", "amount": sale.Amount, "currency": "USD", "order_id": sale.OrderID, "customer_vault_id": sale.Vault,
				"subscription": obj{"subscription_id": l.railSub}, "action": obj{"action_type": "sale", "amount": sale.Amount, "success": "1", "response_code": "100"}})
			require.Equal(t, http.StatusOK, w.deliver("nmi", notice))
			require.Equal(t, http.StatusOK, w.deliver("nmi", notice))
			mirrored := 0
			for _, p := range completed(w.payments(tp, l.c.id)) {
				if p.TransactionID == sale.TransactionID {
					mirrored++
				}
			}
			require.Equal(t, 1, mirrored, "one local payment per NMI transaction")
			require.True(t, l.periodEnd().After(end))
			require.Len(t, updates(), 1)
			require.Equal(t, charges, l.engineCharges(), "OpenRails never charges a legacy subscription")
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

// Refunding an NMI-captured legacy payment sends exactly one NMI refund.
// Access follows revoke_access; the NMI schedule is untouched either way —
// stopping future billing is a cancel, not a refund.
func TestLegacyNMIRefund(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name   string
		amount int64 // micros; 0 = full
		revoke bool
	}{
		{"full", 0, false},
		{"full revoke", 0, true},
		{"partial", 5_000_000, false},
		{"partial revoke", 5_000_000, true},
	}
	for i, row := range rows {
		tp := []topology{embedded, remote}[i%2]
		t.Run(fmt.Sprintf("%s/%s", row.name, tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importLegacy(t, w, "nmi", tp)
			w.converge()
			paid := completed(w.payments(tp, l.c.id))
			require.Len(t, paid, 1, "the imported legacy charge")
			legacy := paid[0]
			params := openrails.RefundPaymentParams{Amount: row.amount, Full: row.amount == 0, Reason: "requested_by_customer", RevokeAccess: row.revoke, IdempotencyKey: "refund-" + legacy.ID.String()}
			refund, err := w.client[tp].RefundPayment(t.Context(), legacy.ID, params)
			require.NoError(t, err)
			w.settle()
			again, err := w.client[tp].RefundPayment(t.Context(), legacy.ID, params)
			require.NoError(t, err)
			require.Equal(t, refund.ID, again.ID, "a replayed refund is the same refund")
			w.settle()
			require.Len(t, w.nmi.callsTo(http.MethodPost, "/payments/"+legacy.TransactionID+"/refund", nil), 1, "exactly one NMI refund")
			want := row.amount
			if want == 0 {
				want = legacy.Amount
			}
			got, err := w.client[tp].GetPayment(t.Context(), legacy.ID)
			require.NoError(t, err)
			require.Equal(t, want, got.AmountRefunded)
			require.Equal(t, http.StatusOK, w.deliver("nmi", w.refundNotice("nmi")), "NMI's own refund notice does not re-decide it")
			require.Equal(t, !row.revoke, l.c.entitled(l.ent), "access follows revoke_access")
			if row.revoke {
				// Revoking access ends the membership: NMI stops billing it,
				// exactly once, and nothing re-grants the access.
				require.Equal(t, "cancelled", w.subscription(tp, l.sub).Status)
				w.until(func() bool { return w.nmi.deletesOf(l.railSub) > 0 }, "the NMI schedule delete")
				w.converge()
				w.pull()
				w.advance(time.Hour)
				w.wake()
				require.Equal(t, 1, w.nmi.deletesOf(l.railSub), "exactly one NMI delete")
				require.False(t, l.c.entitled(l.ent), "revoked access stays revoked through converge and pull")
			} else {
				require.True(t, w.nmi.scheduleLive(l.railSub), "a refund without revoke leaves NMI billing")
				require.Zero(t, w.nmi.deletesOf(l.railSub))
				require.Equal(t, "active", w.subscription(tp, l.sub).Status)
			}
			require.Zero(t, l.engineCharges())
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

// Tier change on a legacy NMI subscription is refused, typed, on both Client
// topologies and the preview: NMI keeps billing its own schedule, so the
// supported path is the engine takeover, then an engine tier change.
func TestLegacyNMITierChangeRefused(t *testing.T) {
	t.Parallel()
	for _, policy := range []string{"provider", "provider_dunning"} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.armDestructive()
			l := importLegacy(t, w, "nmi", embedded, func(book *openrails.DeclaredBilling) {
				book.Subscriptions[0].CollectionPolicy = policy
			})
			up := w.tierPrice("g"+uuid.NewString()[:8], 2, 1999, monthHours, true)
			down := w.tierPrice("g"+uuid.NewString()[:8], 0, 499, monthHours, true)
			for _, tp := range []topology{embedded, remote} {
				for _, target := range []tier{up, down} {
					_, err := w.client[tp].PreviewTierChange(t.Context(), l.sub, openrails.ChangeTierRequest{PriceID: target.ID})
					requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeRequiresEngineBilling)
					_, err = w.client[tp].ChangeTier(t.Context(), l.sub, "tier-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: target.ID})
					requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeRequiresEngineBilling)
				}
			}
			w.settle()
			sub := w.subscription(embedded, l.sub)
			require.Equal(t, l.price.ID, sub.PriceID)
			require.Nil(t, sub.ScheduledPriceID, "nothing scheduled")
			require.Zero(t, l.engineCharges(), "nothing charged")
			require.Empty(t, w.nmi.callsTo(http.MethodPost, "transact.php", nil), "no NMI schedule or sale")
			require.Zero(t, w.nmi.deletesOf(l.railSub))
			require.Empty(t, w.nmi.unexpected())
		})
	}
}
