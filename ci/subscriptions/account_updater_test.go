//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT `+column+` FROM openrails.payment_methods WHERE id = $1`), strings.TrimPrefix(method, "pm_")).Scan(&v))
	return v
}

// vaultOf is the NMI vault a stored card lives in.
func (w *world) vaultOf(method string) string { return w.methodRow(method, "rail_customer_ref") }

// cardUpdates is the recorded card updates of a stored card, as source/kind.
func (w *world) cardUpdates(vault string) []string {
	w.t.Helper()
	rows, err := w.pool.Query(w.t.Context(), w.q(`SELECT u.source || '/' || u.kind FROM openrails.payment_method_updates u
		JOIN openrails.payment_methods pm ON pm.merchant_id = u.merchant_id AND pm.id = u.payment_method_id
		WHERE pm.rail_customer_ref = $1 ORDER BY u.at, u.id`), vault)
	require.NoError(w.t, err)
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
	w.nmi.SetDecline(visa.Last4, "223") // expired, on both vaults

	w.advance(l.periodEnd().Sub(w.clock.Now()) + time.Hour)
	require.Equal(t, http.StatusOK, w.deliver("nmi", l.providerRenewal(false)))
	w.settle()
	e.toPeriodEnd()
	w.runRenewals()
	require.Equal(t, "awaiting_method", w.subscription(embedded, l.sub).Status)
	require.Equal(t, "awaiting_method", w.subscription(embedded, e.sub).Status)

	for _, vault := range []string{l.railCust, w.vaultOf(e.method)} {
		w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Card.Decline, v.Card.Last4 = "", "1881" })
		require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("automaticallyupdated", vault)))
		require.Equal(t, []string{"nmi_acu/updated"}, w.cardUpdates(vault))
	}
	w.runRenewals()
	require.Equal(t, "active", w.subscription(embedded, l.sub).Status, "the NMI-owned membership collects")
	require.Equal(t, "active", w.subscription(embedded, e.sub).Status, "the engine membership collects")
	require.Equal(t, "1881", w.methodRow(e.method, "last_four"), "the stored card takes the details NMI now holds")
}

// NMI cannot update a card and asks for the customer: the member is asked
// for a new card once per period, however often NMI says so. A closed
// account parks the card (#1115).
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
	require.Equal(t, []string{"nmi_acu/contact_customer", "nmi_acu/contact_customer"}, w.cardUpdates(vault), "one per notice")
	require.Equal(t, "active", w.subscription(embedded, e.sub).Status, "the card is not parked: it may still work")

	require.Equal(t, http.StatusOK, w.deliver("nmi", acuNotice("closedaccount", vault)))
	require.Equal(t, "nmi_acu_closed_account", w.methodRow(e.method, "park_reason"), "a closed account is never charged again")
	require.Equal(t, 1, e.c.notificationCount("payment_method_update_required"), "the member was already asked this period")
}
