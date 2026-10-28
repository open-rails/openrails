//go:build integration

package critical_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/stretchr/testify/require"
)

// stripeAccounts is two independent Stripe accounts behind one wire, chosen by
// the request's secret key: what each provider account answers is its own.
type stripeAccounts struct {
	accounts map[string]*providerLedger
	mu       sync.Mutex
	reads    map[string][]string // secret -> GET paths
}

func newStripeAccounts(t *testing.T, w *world, secrets ...string) *stripeAccounts {
	t.Helper()
	s := &stripeAccounts{accounts: map[string]*providerLedger{}, reads: map[string][]string{}}
	for _, secret := range secrets {
		p := newProviderLedger(t, w)
		p.secret = secret
		s.accounts[secret] = p
	}
	server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		p := s.accounts[secret]
		if p == nil {
			http.Error(out, "fixture rejects this credential", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			s.mu.Lock()
			s.reads[secret] = append(s.reads[secret], r.URL.Path)
			s.mu.Unlock()
		}
		p.serve(out, r)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	w.deps.StripeTransport = loopbackWire{target: target, wire: transport}
	return s
}

func (s *stripeAccounts) read(secret, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.reads[secret] {
		if p == path {
			return true
		}
	}
	return false
}

func stripePSP(account, secret string, archived bool) openrails.PSPConfig {
	return openrails.StripePSP{AccountID: account, Environment: openrails.EnvironmentTest, SecretKey: secret,
		WebhookSigningSecret: "whsec_" + account, PublishableKey: "pk_test_" + account, Archived: archived}.PSPConfig()
}

// armDestructive is the documented operator SQL that enables the instance
// switch and arms one merchant for enforcing convergence.
func armDestructive(t *testing.T, a *authorityWorld, on bool) {
	t.Helper()
	_, err := a.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{a.schema, "destructive_action_switch"}.Sanitize()+" SET enabled=$1, updated_by='critical fixture operator', reason='owned disposable verification test'", on)
	require.NoError(t, err)
	_, err = a.pool.Exec(t.Context(), "INSERT INTO "+pgx.Identifier{a.schema, "merchant_destructive_policy"}.Sanitize()+
		" (merchant_id, destructive_actions_enabled, enforce_armed_at, updated_by, reason) VALUES ($1, true, now(), 'critical fixture operator', 'reviewed') ON CONFLICT (merchant_id) DO UPDATE SET enforce_armed_at = now(), destructive_actions_enabled = true",
		a.alpha.MerchantID().UUID())
	require.NoError(t, err)
}

// drainingStripeMembership imports a provider-owned Stripe subscription on
// account "old", then restarts the engine with "old" archived (draining) and
// a different active account "new" that has never heard of it.
func drainingStripeMembership(t *testing.T) (*authorityWorld, *stripeAccounts, *billing.Subscription) {
	t.Helper()
	var accounts *stripeAccounts
	a := newAuthorityWorld(t, func(w *world) {
		accounts = newStripeAccounts(t, w, "sk_test_old", "sk_test_new")
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		m := w.cfg.Merchants["alpha"]
		m.PSPs = openrails.PSPs{"old": stripePSP("acct_old", "sk_test_old", false)}
		w.cfg.Merchants["alpha"] = m
	})
	old := accounts.accounts["sk_test_old"].core
	old.SetLegacyPrice("price_legacy_monthly", 1000)
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "stripe-membership", DisplayName: "Stripe membership", Entitlements: []string{"stripe:member"}})
	require.NoError(t, err)
	price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "stripe-monthly", PSP: "old", PriceID: "price_legacy_monthly"})
	require.NoError(t, err)
	start := a.clock.Now().Truncate(time.Second)
	end := start.Add(30 * 24 * time.Hour)
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	remote := old.AddSubscription(old.AddCustomer(a.alice.Email, "Draining member", nil), "pm_draining", "price_legacy_monthly", 1000, start, end)
	book := billing.DeclaredBilling{AsOf: start, DefaultPSP: billing.PSPRef{Key: "old"}, Customers: []billing.DeclaredCustomer{{Customer: customer}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "draining-stripe-membership", Customer: customer, Price: price.ID, Rail: "stripe", RailSubscriptionID: remote, StartedAt: start, PaidThrough: &end}}}
	imported, err := a.alpha.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
	subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, subs.Items, 1)
	sub, err := a.alpha.GetSubscription(t.Context(), subs.Items[0].ID)
	require.NoError(t, err)
	require.Equal(t, billing.SubscriptionActive, sub.Status)

	require.NoError(t, a.engine.Close(context.Background()))
	m := a.cfg.Merchants["alpha"]
	m.PSPs = openrails.PSPs{"old": stripePSP("acct_old", "sk_test_old", true), "new": stripePSP("acct_new", "sk_test_new", false)}
	a.cfg.Merchants["alpha"] = m
	a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
	require.NoError(t, err)
	a.alpha, err = a.engine.MerchantBySlug("alpha")
	require.NoError(t, err)
	return a, accounts, sub
}

func TestVerificationBinding(t *testing.T) {
	t.Run("OR-J15-S05/draining-account-is-read-with-its-own-credentials", func(t *testing.T) {
		a, accounts, sub := drainingStripeMembership(t)
		armDestructive(t, a, true)
		old := accounts.accounts["sk_test_old"].core
		remote := *sub.RailSubscriptionID
		// The archived account keeps billing its member; the active account has
		// never heard of the subscription.
		old.RenewSubscription(remote, true)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) + 49*time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.converge_sweep", 1)
		require.Eventually(t, func() bool { return accounts.read("sk_test_old", "/v1/subscriptions/"+remote) }, 20*time.Second, 20*time.Millisecond,
			"the unverified row must be read from the account that owns it")
		require.Never(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err != nil || current.Status == billing.SubscriptionCanceled
		}, 2*time.Second, 50*time.Millisecond, "another account's absence is no evidence about this subscription")
		require.False(t, accounts.read("sk_test_new", "/v1/subscriptions/"+remote), "a sibling account is never asked about a subscription it does not own")
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status == billing.SubscriptionActive && current.CurrentPeriodStartsAt.Equal(*sub.CurrentPeriodEndsAt)
		}, 20*time.Second, 20*time.Millisecond, "the owning account's renewal is the evidence the row converges on")
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"stripe:member"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["stripe:member"])
	})
	t.Run("OR-J34-S05/provider-confirmed-cancel-waits-for-the-kill-switch", func(t *testing.T) {
		a, accounts, sub := drainingStripeMembership(t)
		old := accounts.accounts["sk_test_old"].core
		remote := *sub.RailSubscriptionID
		// The owning account really ended the subscription, but the deployment
		// has not enabled destructive convergence.
		old.CancelSubscription(remote)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) + 49*time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.converge_sweep", 1)
		require.Eventually(t, func() bool { return accounts.read("sk_test_old", "/v1/subscriptions/"+remote) }, 20*time.Second, 20*time.Millisecond)
		require.Never(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err != nil || current.Status == billing.SubscriptionCanceled
		}, 2*time.Second, 50*time.Millisecond, "the kill switch holds every convergence cancel")
		held, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		require.NoError(t, err)
		require.Equal(t, billing.SubscriptionStatus("unverified"), held.Status)
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"stripe:member"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["stripe:member"], "a held cancel keeps access")
		armDestructive(t, a, true)
		_, err = a.alpha.RefreshPSPs(t.Context())
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status == billing.SubscriptionCanceled
		}, 20*time.Second, 50*time.Millisecond, "once enabled, the owning account's cancel converges")
	})
}
