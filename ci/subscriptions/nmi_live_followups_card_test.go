//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/openrails"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

// A provider refresh reads the vault roster before the local one. A card
// saved in between is present at NMI and must not be reported missing; a
// card genuinely removed at NMI still is.
func TestNMIRefreshRacingCardSave(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.armDestructive()
	c := w.newCustomer()
	kept := w.storedCard(c.saveCard("nmi", visa))

	// Hold the refresh right after its vault roster read.
	g := w.nmi.hold(newGate(func(r *http.Request) bool {
		return r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v5/subscriptions")
	}, false))
	res, err := w.jobs.Insert(t.Context(), refreshMerchant{MerchantID: w.client[embedded].MerchantID().UUID()}, &river.InsertOpts{Queue: openrails.QueueBilling})
	require.NoError(t, err)
	select {
	case <-g.arrived:
	case <-time.After(20 * time.Second):
		t.Fatal("the refresh never read the NMI roster")
	}
	saved := w.storedCard(c.saveCard("nmi", mastercard))
	w.nmi.unhold()
	close(g.release)
	w.waitJob(res.Job.ID)
	mismatched := w.openFindings("pull.payment_method.mismatch")
	require.NotContains(t, mismatched, saved.vault, "a card saved during the refresh is held by NMI")
	require.NotContains(t, mismatched, kept.vault)

	w.nmi.RemoveVault(kept.vault)
	w.pull()
	require.Contains(t, w.openFindings("pull.payment_method.mismatch"), kept.vault, "a card removed at NMI is reported")
	require.NotContains(t, w.openFindings("pull.payment_method.mismatch"), saved.vault)
	require.Zero(t, len(w.nmi.Attempts()))
}
