//go:build greenfield && integration

package subscriptions_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// nmiDupWindow is the duplicate window the fake NMI applies in these rows.
const nmiDupWindow = 20 * time.Second

// seedRecent is another customer's charge of amount on the same card, just
// processed at NMI.
func (f *nmiFake) seedRecent(c card, amount string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.remember(c, amount)
}

func wireAmount(micros int64) string {
	cents := micros / 10_000
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}

// A legacy (NMI-billed) upgrade whose proration matches another customer's
// charge of the same card and amount inside NMI's duplicate window is refused
// unprocessed: typed 409, nothing charged, NMI's schedule untouched, the
// operation terminal. The same change after the window charges exactly once.
func TestLegacyNMITierUpgradeDuplicateRefused(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			group := "g" + uuid.NewString()[:8]
			old := w.tierPrice(group, 1, 999, monthHours, true)
			next := w.tierPrice(group, 2, 1999, monthHours, false)
			l := w.legacyOnTier(tp, old, 999, monthHours, 10*day)
			w.nmi.customSchedule(l.railSub)
			preview, err := w.client[tp].PreviewTierChange(t.Context(), l.sub, openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			w.nmi.duplicateWindow(w.clock.Now, nmiDupWindow)
			w.nmi.seedRecent(visa, wireAmount(preview.AmountDueNow))
			sales := len(l.tierSales())

			key := "up-" + uuid.NewString()
			_, err = w.client[tp].ChangeTier(t.Context(), l.sub, key, openrails.ChangeTierRequest{PriceID: next.ID})
			requireCode(t, err, http.StatusConflict, openrails.CodePaymentDuplicateRefused)
			w.settle()
			_, err = w.client[tp].ChangeTier(t.Context(), l.sub, key, openrails.ChangeTierRequest{PriceID: next.ID})
			requireCode(t, err, http.StatusConflict, openrails.CodePaymentDuplicateRefused)
			require.Len(t, l.tierSales(), sales, "nothing was charged")
			require.Empty(t, w.nmi.scheduleUpdates(l.railSub), "NMI's schedule is untouched")
			require.Equal(t, old.ID, w.subscription(tp, l.sub).PriceID)
			require.True(t, l.c.entitled(old.ent))

			w.advance(nmiDupWindow + time.Second)
			done, err := w.client[tp].ChangeTier(t.Context(), l.sub, "up-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: next.ID})
			require.NoError(t, err)
			w.settle()
			require.Equal(t, "succeeded", done.Status, "%+v", done)
			require.Len(t, l.tierSales(), sales+1, "exactly one charge")
			require.Len(t, w.nmi.scheduleUpdates(l.railSub), 1)
			require.Equal(t, next.ID, w.subscription(tp, l.sub).PriceID)
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

// The duplicate refusal's answer is lost in transit: the operation cannot
// read a receipt, and after the settle delay NMI's empty record under its
// order settles it as not executed on its own. No finding stays open and
// nothing was charged.
func TestLegacyNMITierUpgradeLostDuplicateSettles(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	old := w.tierPrice(group, 1, 999, monthHours, true)
	next := w.tierPrice(group, 2, 1999, monthHours, false)
	l := w.legacyOnTier(embedded, old, 999, monthHours, 10*day)
	w.nmi.customSchedule(l.railSub)
	preview, err := w.client[embedded].PreviewTierChange(t.Context(), l.sub, openrails.ChangeTierRequest{PriceID: next.ID})
	require.NoError(t, err)
	w.nmi.duplicateWindow(w.clock.Now, nmiDupWindow)
	w.nmi.seedRecent(visa, wireAmount(preview.AmountDueNow))
	w.nmi.dropResponses(1)
	sales := len(l.tierSales())

	key := "up-" + uuid.NewString()
	done, err := w.client[embedded].ChangeTier(t.Context(), l.sub, key, openrails.ChangeTierRequest{PriceID: next.ID})
	require.NoError(t, err)
	require.Equal(t, "processing", done.Status, "%+v", done)
	w.until(func() bool {
		_, err := w.client[embedded].ChangeTier(t.Context(), l.sub, key, openrails.ChangeTierRequest{PriceID: next.ID})
		return err != nil
	}, "the unsettled proration resolves from NMI's record")
	_, err = w.client[embedded].ChangeTier(t.Context(), l.sub, key, openrails.ChangeTierRequest{PriceID: next.ID})
	requireCode(t, err, http.StatusConflict, openrails.CodeTierChangeRefused)
	require.Len(t, l.tierSales(), sales, "nothing was charged")
	require.Empty(t, w.nmi.scheduleUpdates(l.railSub))
	require.Empty(t, w.openFindings("life.tier_change.proration_unresolved"))
	require.Equal(t, old.ID, w.subscription(embedded, l.sub).PriceID)
}

// An engine tier upgrade refused as a duplicate is not executed, not left
// unresolved; after the window the same upgrade charges once.
func TestEngineTierUpgradeDuplicateRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	group := "g" + uuid.NewString()[:8]
	from := w.tierPrice(group, 1, 1000, 720, false)
	to := w.tierPrice(group, 2, 2000, 720, false)
	_, sub := w.engineMember("nmi", embedded, from)
	w.advance(w.subscription(embedded, sub).CurrentPeriodEndsAt.Sub(w.clock.Now()) - 360*time.Hour)
	preview, err := w.client[embedded].PreviewTierChange(t.Context(), sub, openrails.ChangeTierRequest{PriceID: to.ID})
	require.NoError(t, err)
	w.nmi.duplicateWindow(w.clock.Now, nmiDupWindow)
	w.nmi.seedRecent(visa, wireAmount(preview.AmountDueNow))
	charges := len(w.nmi.ledger(""))

	_, err = w.client[embedded].ChangeTier(t.Context(), sub, "up-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: to.ID})
	var statusErr *openrails.StatusError
	require.ErrorAs(t, err, &statusErr)
	require.Equal(t, http.StatusConflict, statusErr.Status, "%v", err)
	w.settle()
	require.Len(t, w.nmi.ledger(""), charges, "nothing was charged")
	require.Equal(t, from.ID, w.subscription(embedded, sub).PriceID)

	w.advance(nmiDupWindow + time.Second)
	done, err := w.client[embedded].ChangeTier(t.Context(), sub, "up-"+uuid.NewString(), openrails.ChangeTierRequest{PriceID: to.ID})
	require.NoError(t, err)
	require.Equal(t, "succeeded", done.Status, "%+v", done)
	require.Len(t, w.nmi.ledger(""), charges+1)
}

// Saving a card whose recurring verification NMI refuses as a duplicate is a
// typed 409 and leaves no vault behind; the same save after the window works.
func TestNMICardSaveDuplicateRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	c := w.newCustomer()
	w.nmi.duplicateWindow(w.clock.Now, nmiDupWindow)
	w.nmi.seedRecent(visa, "0.00")
	w.nmi.mu.Lock()
	before := len(w.nmi.vaults)
	w.nmi.mu.Unlock()
	status, body := c.call(http.MethodPost, "/payment-methods", "", map[string]any{"provider": "nmi", "psp_id": w.psp["nmi"], "payment_token": w.nmi.tokenize(visa), "name_on_card": "Greenfield Payer"})
	require.Equal(t, http.StatusConflict, status, "%v", body)
	require.Equal(t, openrails.CodePaymentDuplicateRefused, errorCode(body), "%v", body)
	w.nmi.mu.Lock()
	after := len(w.nmi.vaults)
	w.nmi.mu.Unlock()
	require.Equal(t, before, after, "the refused card's vault is removed")
	w.advance(nmiDupWindow + time.Second)
	require.NotEmpty(t, c.saveCard("nmi", visa))
}

// A tiny book whose schedules NMI ended converges in one armed pull: the
// cancellation cap never holds a whole book of at most five.
func TestTinyBookCancellationConverges(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	book := w.mirrorBook(embedded, w.bookTier("monthly", 999, 30), 4)
	// Another schedule stays live at NMI, so the roster is non-empty.
	vault := w.nmi.legacyVault(mastercard)
	w.nmi.legacySchedule(vault, "legacy_plan_other", "9.99", w.clock.Now().Add(20*day))
	for _, l := range book {
		w.nmi.providerCancel(l.railSub)
	}
	w.advance(time.Hour)
	w.pull()
	for _, l := range book {
		require.Equal(t, "cancelled", w.subscription(embedded, l.sub).Status)
		require.Zero(t, w.nmi.deletesOf(l.railSub))
	}
	require.Empty(t, w.openFindings("pull.cancellation.capped"))
	require.Zero(t, w.nmi.saleAttempts())
}
