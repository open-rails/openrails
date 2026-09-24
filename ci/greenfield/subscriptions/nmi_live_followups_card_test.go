//go:build greenfield && integration

package subscriptions_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/embed"
)

// validationsOf is every card verification NMI received for a vault.
func (f *nmiFake) validationsOf(vault string) []nmiValidation {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []nmiValidation
	for _, v := range f.validations {
		if v.Vault == vault {
			out = append(out, v)
		}
	}
	return out
}

// billingIDs is the vault's billing entries at NMI, primary first.
func (f *nmiFake) billingIDs(vault string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.vaults[vault]
	if v == nil {
		return nil
	}
	ids := []string{v.BillingID}
	for _, b := range v.Extra {
		ids = append(ids, b.ID)
	}
	return ids
}

// An in-place card replacement verifies the new card as a recurring
// credential-on-file agreement before the method uses it: the card and its
// agreement change together, the replaced card is retired at NMI, and the
// next merchant-initiated renewal cites the new card's agreement. A card the
// issuer refuses to verify is not adopted: typed decline, and the previous
// card and agreement keep billing.
func TestNMIInPlaceReplacementEstablishesAgreement(t *testing.T) {
	t.Parallel()
	for _, tp := range []topology{embedded, remote} {
		t.Run(string(tp), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			e := enroll(t, w, "nmi", tp)
			before := w.storedCard(e.method)
			require.NotEmpty(t, before.recurringRef)
			verified := len(w.nmi.validationsOf(before.vault))

			// Refused: nothing changes, at NMI or locally.
			refused := card{Brand: "mastercard", Last4: "0051", Decline: "200"}
			status, body := e.c.call(http.MethodPut, "/payment-methods/"+e.method, "", map[string]any{"provider": "nmi", "payment_token": w.nmi.tokenize(refused),
				"last_four": refused.Last4, "card_type": refused.Brand, "expiry_date": "12/35"})
			require.Equal(t, http.StatusPaymentRequired, status, "%v", body)
			require.Equal(t, "card_declined", errorCode(body))
			w.settle()
			require.Equal(t, before, w.storedCard(e.method), "a refused replacement leaves the card and its agreement")
			require.Equal(t, []string{before.billing}, w.nmi.billingIDs(before.vault), "the refused card is removed from the vault")
			require.Len(t, w.nmi.validationsOf(before.vault), verified+1, "one verification for the refused card")

			// Replaced: one new verification, adopted with the card.
			e.replaceCard(mastercard)
			after := w.storedCard(e.method)
			all := w.nmi.validationsOf(before.vault)
			require.Len(t, all, verified+2, "exactly one verification for the replacement card")
			latest := all[len(all)-1]
			require.True(t, latest.Approved)
			require.Equal(t, mastercard.Last4, latest.Card.Last4, "the verification is of the new card")
			require.Equal(t, "customer", latest.Form.Get("initiated_by"))
			require.Equal(t, "stored", latest.Form.Get("stored_credential_indicator"))
			require.Equal(t, "recurring", latest.Form.Get("billing_method"))
			require.Empty(t, latest.Form.Get("amount"), "a verification moves no funds")
			require.True(t, strings.HasPrefix(latest.Form.Get("orderid"), "pmu-"))
			require.Equal(t, before.vault, after.vault, "the method keeps its vault")
			require.Equal(t, latest.BillingID, after.billing, "the method moves onto the verified card")
			require.Equal(t, latest.TransactionID, after.recurringRef, "the agreement is the new card's verification")
			require.Equal(t, mastercard.Last4, after.lastFour)
			require.Equal(t, "mastercard", after.cardType)
			require.Equal(t, []string{after.billing}, w.nmi.billingIDs(before.vault), "the replaced card is retired at NMI")

			sales := len(w.nmi.ledger(""))
			end := e.periodEnd()
			e.toPeriodEnd()
			w.runRenewals()
			require.True(t, e.periodEnd().After(end))
			require.Len(t, w.nmi.ledger(""), sales+1, "exactly one renewal")
			renewal := w.nmi.lastSale()
			require.Equal(t, mastercard.Last4, renewal.Card.Last4)
			require.Equal(t, after.billing, renewal.BillingID)
			require.Equal(t, "merchant", renewal.InitiatedBy)
			require.Equal(t, "used", renewal.Indicator)
			require.Equal(t, latest.TransactionID, renewal.Initial, "the MIT cites the agreement established on the card it charges")
			require.Empty(t, w.nmi.unexpected())
		})
	}
}

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
	res, err := w.jobs.Insert(t.Context(), refreshMerchant{MerchantID: w.client[embedded].MerchantID().UUID()}, &river.InsertOpts{Queue: embed.QueueBilling})
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

	w.nmi.removeVault(kept.vault)
	w.pull()
	require.Contains(t, w.openFindings("pull.payment_method.mismatch"), kept.vault, "a card removed at NMI is reported")
	require.NotContains(t, w.openFindings("pull.payment_method.mismatch"), saved.vault)
	require.Zero(t, w.nmi.saleAttempts())
}
