//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
)

// acuNotice is NMI's Account Updater notice about one vault (#1115).
func acuNotice(kind, vault string) obj {
	return nmiEvent("acu.summary."+kind, obj{"customer_vault_id": vault})
}

// methodRow is a stored card's column, by its public id.
func (w *world) methodRow(method, column string) string {
	w.t.Helper()
	var v string
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT `+column+` FROM billing.payment_methods WHERE id = $1`), strings.TrimPrefix(method, "pm_")).Scan(&v))
	return v
}

// vaultOf is the NMI vault a stored card lives in.
func (w *world) vaultOf(method string) string { return w.methodRow(method, "rail_customer_ref") }

// cardVersions is the card versions of the stored cards on one vault, oldest
// first, as source/kind.
func (w *world) cardVersions(vault string) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT v.source || '/' || v.kind FROM billing.payment_method_versions v
		JOIN billing.payment_methods pm ON pm.merchant_id = v.merchant_id AND pm.id = v.payment_method_id
		WHERE pm.rail_customer_ref = $1 ORDER BY v.created_at, v.id`), vault)
	require.NoError(w.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(w.t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(w.t, rows.Err())
	return out
}

// mandateStates is a stored card's mandates, oldest first, as
// kind/status[/end reason].
func (w *world) mandateStates(method string) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT kind || '/' || status || COALESCE('/' || end_reason, '') FROM billing.mandates
		WHERE payment_method_id = $1 ORDER BY created_at, id`), strings.TrimPrefix(method, "pm_"))
	require.NoError(w.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(w.t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(w.t, rows.Err())
	return out
}

func (c *customer) notificationCount(kind string) int {
	c.w.t.Helper()
	items, _ := c.must(http.MethodGet, "/notifications?limit=100", "", nil)["data"].([]any)
	n := 0
	for _, item := range items {
		if item.(map[string]any)["event_type"] == kind {
			n++
		}
	}
	return n
}

// An expired card sends an NMI-owned and an engine membership to wait for a
// new card. NMI's Account Updater reissues both cards: the stored cards take
// the new details from the vault, and both memberships collect at the next
// due pass (#1115).
func TestNMIAccountUpdaterRecoversBothOwners(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	l := importLegacy(t, w, "nmi", embedded, declareRecurringAnchor)
	w.converge()
	e := enroll(t, w, "nmi", embedded)
	e.refreshBeforePeriodEnd()
	w.nmi.SetDecline(visa.Last4, "223") // expired, on both vaults

	w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
	w.settle()
	e.toPeriodEnd()
	w.runRenewals()
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, l.sub).Status)
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status)

	for _, vault := range []string{l.railCust, w.vaultOf(e.method)} {
		w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Card.Decline, v.Card.Last4 = "", "1881" })
		require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("automaticallyupdated", vault)))
		require.Equal(t, []string{"customer_save/saved", "nmi_acu/updated"}, w.cardVersions(vault), "the reissue is a version of the same method")
	}
	w.runRenewals()
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, l.sub).Status, "the NMI-owned membership collects")
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status, "the engine membership collects")
	require.Equal(t, "1881", w.methodRow(e.method, "card_last4"), "the stored card takes the details NMI now holds")
}

// NMI cannot update a card and asks for the customer: the member is asked
// once per period, however often NMI says so, and the card keeps billing. A
// closed account closes the card and ends its agreements; the membership
// waits for another card and is never canceled (#1115, #1168).
func TestNMIAccountUpdaterContactCustomer(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	vault := w.vaultOf(e.method)
	notice := acuNotice("contactcustomer", vault)
	require.Equal(t, http.StatusOK, w.deliver("nmi", notice))
	require.Equal(t, http.StatusOK, w.deliver("nmi", notice), "a redelivery")
	require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("contactcustomer", vault)), "NMI says it again")
	require.Equal(t, 1, e.c.notificationCount("payment_method_update_required"))
	require.Equal(t, []string{"customer_save/saved", "nmi_acu/contact_cardholder", "nmi_acu/contact_cardholder"}, w.cardVersions(vault), "one per notice")
	require.Equal(t, "active", w.methodRow(e.method, "status"), "a Visa contact advice is a prompt: the card may still work")
	require.NotEmpty(t, w.methodRow(e.method, "contact_cardholder_at::text"))
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, e.sub).Status)

	require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("closedaccount", vault)))
	require.Equal(t, "closed", w.methodRow(e.method, "status"), "a closed account is never charged again")
	require.Equal(t, []string{"card_on_file/ended/closed", "recurring/ended/closed"}, w.mandateStates(e.method))
	require.Equal(t, 1, e.c.notificationCount("payment_method_update_required"), "the member was already asked this period")

	sent := len(w.nmi.Attempts())
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.Len(t, w.nmi.Attempts(), sent, "a closed card is not charged")
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status, "the membership waits for another card")

	require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("automaticallyupdated", vault)))
	require.Equal(t, "closed", w.methodRow(e.method, "status"), "closed is final")
}

// Mastercard's updater answers CONTACT for a closed account, so NMI's
// contact-customer notice on a Mastercard card closes it.
func TestNMIAccountUpdaterContactOnMastercardCloses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	method := c.saveCard("nmi", mastercard)
	vault := w.vaultOf(method)
	require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("contactcustomer", vault)))
	require.Equal(t, "closed", w.methodRow(method, "status"))
	require.Equal(t, []string{"customer_save/saved", "nmi_acu/closed"}, w.cardVersions(vault))
	require.Equal(t, []string{"card_on_file/ended/closed"}, w.mandateStates(method))
}
