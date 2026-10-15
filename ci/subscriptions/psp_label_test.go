//go:build e2e && integration

package subscriptions_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/app"
	"github.com/open-rails/openrails/internal/engine"
)

// accountB is the gateway account a label moves to.
const accountB = "e2e-nmi-b"

// pspRow is the stored identity of an NMI account.
func (w *world) pspRow(account string) (id string, key string, superseded bool) {
	w.t.Helper()
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), "SELECT id::text, key, superseded_at IS NOT NULL FROM "+pgx.Identifier{w.schema, "psps"}.Sanitize()+
		" WHERE rail = 'nmi' AND account_id = $1", account).Scan(&id, &key, &superseded))
	return id, key, superseded
}

func (w *world) configurationProbe() error {
	w.t.Helper()
	for _, p := range w.rt.Probes() {
		if p.Name == "openrails_configuration" {
			return p.Check(context.Background())
		}
	}
	w.t.Fatal("no openrails_configuration probe")
	return nil
}

// declareB points the label at account B; keepA, when set, keeps account A
// declared under it.
func declareB(keepA string) func(map[string]openrails.PSPConfig) {
	return func(psps map[string]openrails.PSPConfig) {
		a := psps["nmi"]
		b := a
		b.AccountID, b.Secrets = accountB, map[string]string{"security_key": "e2e-nmi-key-b", "webhook_signing_secret": whsecNMI}
		delete(psps, "nmi")
		psps["mobius"] = b
		if keepA != "" {
			psps[keepA] = a
		}
	}
}

// undeclaredAlerts counts the alert emails naming account A.
func undeclaredAlerts(mail *mailbox) int {
	n := 0
	for _, e := range mail.all() {
		if strings.Contains(e.Text, "nmi account "+nmiAcct+" has no declared credentials") {
			n++
		}
	}
	return n
}

// A PSP is its account; its label is only how the configuration names it.
// Renaming the label keeps the PSP. Pointing the label at another account
// registers that account as a new PSP, which takes new sales, while the
// account it named keeps every record and holds no credentials: its
// operator is alerted (again while it stays so), its renewal fails on its
// own with the reason, and declaring it again re-arms it and resolves the
// alert.
func TestPSPLabelMovedToAnotherAccount(t *testing.T) {
	t.Parallel()
	logs := logtest.NewGlobal()
	w := prepareWorld(t, 12)
	mail, alertTo := &mailbox{}, "ops@e2e.test"
	w.settings = billing.MerchantSettings{AlertEmail: &alertTo}
	w.deps = func(d *openrails.Deps) { d.Email = mail }
	w.start()
	e := enroll(t, w, "nmi", embedded)
	accountA, _, _ := w.pspRow(nmiAcct)

	w.declare = func(psps map[string]openrails.PSPConfig) { psps["mobius"] = psps["nmi"]; delete(psps, "nmi") }
	w.restart()
	id, key, superseded := w.pspRow(nmiAcct)
	require.Equal(t, accountA, id, "a renamed label is the same PSP")
	require.Equal(t, "mobius", key)
	require.False(t, superseded)
	require.NoError(t, w.configurationProbe())

	w.declare = declareB("")
	w.restart()
	id, key, superseded = w.pspRow(nmiAcct)
	require.Equal(t, accountA, id, "account A keeps its PSP and records")
	require.Equal(t, "mobius", key)
	require.True(t, superseded)
	accountBID, key, superseded := w.pspRow(accountB)
	require.NotEqual(t, accountA, accountBID)
	require.Equal(t, "mobius", key)
	require.False(t, superseded)
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status, "A's subscription is intact")
	require.Equal(t, accountA, sub.PSPID.UUID().String())

	const why = "nmi account " + nmiAcct + " has no declared credentials but holds 1 cards in its vault, 0 subscriptions it bills itself and 1 subscriptions OpenRails renews on it; declare the account's credentials under any PSP key"
	require.ErrorContains(t, w.configurationProbe(), "accounts/nmi/"+nmiAcct+": "+why)
	require.Equal(t, []string{"accounts/nmi/" + nmiAcct}, w.openFindings(app.UndeclaredAccountFindingType))
	require.Eventually(t, func() bool { return undeclaredAlerts(mail) == 1 }, 10*time.Second, 50*time.Millisecond, "the operator is alerted once")

	// B takes new sales.
	c := w.newCustomer()
	method := c.saveCard("nmi", visa)
	price := w.membership("content:new", 9_990_000)
	newSub := c.subscribe(embedded, "nmi", price.ID.String(), "content:new", method)
	require.Equal(t, accountBID, w.subscription(embedded, newSub).PSPID.UUID().String())

	// A's renewal fails on its own, naming why; B's usable account keeps
	// renewing. An undeclared account never blocks the other account.
	aVault, bVault := w.vaultOf(e.method), w.vaultOf(method)
	aCharges, bCharges := len(w.nmi.ledger(aVault)), len(w.nmi.ledger(bVault))
	aAttempts := w.attemptsOn(aVault)
	bEnd := *w.subscription(embedded, newSub).CurrentPeriodEndsAt
	e.toFreshPeriodEnd()
	w.runRenewals()
	require.Len(t, w.nmi.ledger(aVault), aCharges, "A's card is not charged without A's credentials")
	require.Equal(t, aAttempts, w.attemptsOn(aVault), "A's unusable credentials never reach the provider")
	require.Len(t, w.nmi.ledger(bVault), bCharges+1, "B's valid renewal is independent of A's missing credentials")
	bRenewed := w.subscription(embedded, newSub)
	require.Equal(t, accountBID, bRenewed.PSPID.UUID().String())
	require.True(t, bRenewed.CurrentPeriodEndsAt.After(bEnd))
	require.Equal(t, billing.SubscriptionAwaitingMethod, w.subscription(embedded, e.sub).Status, "the member pays the renewal with a usable card")
	refused := false
	for _, entry := range logs.AllEntries() {
		err, _ := entry.Data[logrus.ErrorKey].(error)
		if fmt.Sprint(entry.Data["subscription_id"]) == e.sub.UUID().String() && err != nil && strings.Contains(err.Error(), "nmi account "+nmiAcct+" has no declared credentials") {
			refused = true
		}
	}
	require.True(t, refused, "the renewal names the missing credentials")

	// Unresolved, it alerts again on its cadence.
	w.advance(25 * time.Hour)
	engine.Graph(w.rt).Runtime.RealertConfiguration(t.Context())
	require.Eventually(t, func() bool { return undeclaredAlerts(mail) == 2 }, 10*time.Second, 50*time.Millisecond, "re-alerted while unresolved")

	// Declared again, under another label, account A re-arms.
	w.declare = declareB("mobius-old")
	w.restart()
	id, key, superseded = w.pspRow(nmiAcct)
	require.Equal(t, accountA, id)
	require.Equal(t, "mobius-old", key)
	require.False(t, superseded)
	require.NoError(t, w.configurationProbe())
	require.Empty(t, w.openFindings(app.UndeclaredAccountFindingType), "the alert resolves")
	archived := false
	listed, err := w.client[embedded].ListPSPs(t.Context(), billing.PSPListParams{Archived: &archived})
	require.NoError(t, err)
	var keys []string
	for _, psp := range listed.Items {
		if psp.Rail == "nmi" {
			keys = append(keys, psp.Key)
		}
	}
	require.ElementsMatch(t, []string{"mobius", "mobius-old"}, keys, "both accounts serve")
}

// Kept declared under another label, the account a label moved away from
// keeps renewing what it holds.
func TestAccountKeptUnderAnotherLabelKeepsRenewing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	w.declare = declareB("nmi-old")
	w.restart()
	_, key, superseded := w.pspRow(nmiAcct)
	require.Equal(t, "nmi-old", key)
	require.False(t, superseded)
	require.NoError(t, w.configurationProbe())

	end := e.periodEnd()
	e.toFreshPeriodEnd()
	w.runRenewals()
	sub := w.subscription(embedded, e.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status)
	require.True(t, sub.CurrentPeriodEndsAt.After(end), "renewed on account A")
	require.Len(t, completed(w.payments(embedded, e.c.id)), 2)
}
