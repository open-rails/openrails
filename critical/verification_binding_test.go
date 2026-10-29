//go:build integration

package critical_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
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

// stripeMembership imports a provider-owned Stripe subscription on account
// "old", the merchant's only Stripe PSP.
func stripeMembership(t *testing.T) (*authorityWorld, *stripeAccounts, *billing.Subscription) {
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
	old.AddProduct("prod_legacy", "Legacy membership")
	old.AddPrice(stripemock.Price{ID: "price_legacy_monthly", Product: "prod_legacy", Amount: 1000, Interval: "month"})
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
	return a, accounts, sub
}

// drainingStripeMembership is stripeMembership restarted with "old" archived
// (draining) and a different active account "new" that has never heard of it.
func drainingStripeMembership(t *testing.T) (*authorityWorld, *stripeAccounts, *billing.Subscription) {
	t.Helper()
	a, accounts, sub := stripeMembership(t)
	require.NoError(t, a.engine.Close(context.Background()))
	m := a.cfg.Merchants["alpha"]
	m.PSPs = openrails.PSPs{"old": stripePSP("acct_old", "sk_test_old", true), "new": stripePSP("acct_new", "sk_test_new", false)}
	a.cfg.Merchants["alpha"] = m
	var err error
	a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
	require.NoError(t, err)
	a.alpha, err = a.engine.MerchantBySlug("alpha")
	require.NoError(t, err)
	return a, accounts, sub
}

func TestVerificationBinding(t *testing.T) {
	t.Run("OR-J15-S06/draining-account-is-read-with-its-own-credentials", func(t *testing.T) {
		a, accounts, sub := drainingStripeMembership(t)
		armDestructive(t, a, true)
		old := accounts.accounts["sk_test_old"].core
		remote := *sub.RailSubscriptionID
		// The archived account keeps billing its member; the active account has
		// never heard of the subscription.
		old.RenewSubscription(remote, true)
		a.clock.Advance(sub.CurrentPeriodEndsAt.Sub(a.clock.Now()) + 49*time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
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
		runRegistered(t, fleet, "openrails.rebill_watch", 1)
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
		refreshMerchant(t, fleet, a.alpha.MerchantID().UUID())
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status == billing.SubscriptionCanceled
		}, 20*time.Second, 50*time.Millisecond, "once enabled, the owning account's cancel converges")
	})
	t.Run("OR-J34-S06/held-webhook-cancel-keeps-access-until-the-switch-allows-it", func(t *testing.T) {
		a, accounts, sub := stripeMembership(t)
		old := accounts.accounts["sk_test_old"].core
		remote := *sub.RailSubscriptionID
		fleet := startFleet(t, a.world)
		old.CancelSubscription(remote)
		body := stripeNotice(t, "evt_held_cancel", "customer.subscription.deleted", old.Subscription(remote))
		status, raw := callback(t, a, "acct_old", body, stripeSignature(body, "whsec_acct_old"))
		require.Equal(t, http.StatusOK, status, raw)
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status == billing.SubscriptionStatus("unverified")
		}, 30*time.Second, 50*time.Millisecond, "a cancel the switch holds parks the row for another read")
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"stripe:member"}})
		require.NoError(t, err)
		require.True(t, access.Entitlements["stripe:member"], "a held cancel keeps access")
		armDestructive(t, a, true)
		refreshMerchant(t, fleet, a.alpha.MerchantID().UUID())
		require.Eventually(t, func() bool {
			current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
			return err == nil && current.Status == billing.SubscriptionCanceled
		}, 30*time.Second, 50*time.Millisecond, "once allowed, the held cancel converges without another webhook")
	})
	t.Run("OR-J15-S07/bulk-read-binds-each-nmi-psp", func(t *testing.T) {
		const members = 201 // one past the bulk threshold
		a, accounts := nmiAccountsWorld(t)
		old := accounts.cores["nmi-old-key"]
		old.AddPlan(nmimock.Plan{ID: "weekly", Name: "Weekly", Amount: "10.00", Days: 7})
		product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-membership", DisplayName: "NMI membership", Entitlements: []string{"nmi:member"}})
		require.NoError(t, err)
		price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-weekly", PSP: "old", PlanID: "weekly", Currency: "USD"})
		require.NoError(t, err)
		require.NoError(t, a.engine.Close(context.Background()))
		m := a.cfg.Merchants["alpha"]
		m.PSPs = openrails.PSPs{"old": nmiPSP("old", true), "new": nmiPSP("new", false)}
		a.cfg.Merchants["alpha"] = m
		a.engine, err = openrails.New(t.Context(), a.cfg, a.deps)
		require.NoError(t, err)
		a.alpha, err = a.engine.MerchantBySlug("alpha")
		require.NoError(t, err)
		// The first boot queued the old account's onboarding pull; it would
		// read the 201 members below beside the verifier under test.
		fleet := hostFleet(t, a.world, 2)
		cancelOnboarding(t, fleet, 2)
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))

		// A lapsed book on the archived account: every row is imported with no
		// evidence of the renewal the account has since charged, so all of
		// them are unverified at once.
		now := a.clock.Now()
		started, paid := now.Add(-10*24*time.Hour), now.Add(-3*24*time.Hour)
		book := billing.DeclaredBilling{AsOf: now, DefaultPSP: billing.PSPRef{Key: "old"}}
		for i := range members {
			customer := billing.CustomerID(uuid.New())
			vault := old.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
			schedule := old.AddSchedule(nmimock.Schedule{Vault: vault, Plan: "weekly", Amount: "10.00", Days: 7, Created: started, NextBilling: paid.Add(7 * 24 * time.Hour), Order: fmt.Sprintf("lapsed-%d", i)})
			old.AddScheduleSale(schedule, paid)
			ref := billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vault, RailMethodRef: old.Vault(vault).BillingID}
			book.Customers = append(book.Customers, billing.DeclaredCustomer{Customer: customer})
			book.PaymentMethods = append(book.PaymentMethods, billing.DeclaredPaymentMethod{Customer: customer, Rail: "nmi", RailCustomerRef: vault, RailMethodRef: ref.RailMethodRef, CreatedAt: started})
			book.Subscriptions = append(book.Subscriptions, billing.DeclaredSubscription{SourceID: fmt.Sprintf("lapsed-%d", i), Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: schedule,
				StartedAt: started, PaidThrough: &paid, PaymentMethod: &ref,
				AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: schedule, Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: paid}})
		}
		imported, err := a.alpha.ImportBilling(t.Context(), book)
		require.NoError(t, err)
		require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
		require.Len(t, imported.Imported, members)
		// The archived account still bills every schedule: its roster resolves
		// the rows. No refresh is armed, so only the verifier can.
		for _, i := range []int{0, members - 1} {
			customer := book.Customers[i].Customer
			require.Eventually(t, func() bool {
				subs, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
				return err == nil && len(subs.Items) == 1 && subs.Items[0].Status == billing.SubscriptionActive && subs.Items[0].CurrentPeriodEndsAt.After(now)
			}, 30*time.Second, 50*time.Millisecond, "the archived account's renewals are read in bulk with its own credentials")
		}
	})
	t.Run("OR-J34-S30/switch-turned-off-during-a-pull-holds-the-pulls-retractions", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		armDestructive(t, a, true)
		customer, key := unretractedGrant(t, a)
		// NMI charged the renewal: the pull changes the book and converges it.
		due := *sub.CurrentPeriodEndsAt
		p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
		a.clock.Advance(due.Sub(a.clock.Now()) + time.Hour)
		fleet := hostFleet(t, a.world)
		fleet.PeriodicJobs().Clear()
		// The PSP's onboarding pull would be the first provider read held.
		cancelOnboarding(t, fleet, 1)
		require.NoError(t, a.engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
		// The pull reads the switch as on, then the operator turns it off
		// while the pull is reading the provider.
		hold := p.core.Hold(func(*http.Request) bool { return true }, nmimock.HoldRequest)
		events, unsubscribe := fleet.Subscribe(river.EventKindJobCompleted, river.EventKindJobFailed)
		defer unsubscribe()
		job, err := fleet.Insert(t.Context(), refreshMerchantJob{MerchantID: a.alpha.MerchantID().UUID()}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		select {
		case <-hold.Arrived():
		case <-time.After(30 * time.Second):
			t.Fatal("the pull never read the provider")
		}
		armDestructive(t, a, false)
		hold.Release()
		deadline := time.After(3 * time.Minute)
		for done := false; !done; {
			select {
			case event := <-events:
				done = event != nil && event.Job.ID == job.Job.ID
			case <-deadline:
				t.Fatal("provider refresh did not finish")
			}
		}
		access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{key}})
		require.NoError(t, err)
		require.True(t, access.Entitlements[key], "a switch turned off before the pull's convergence holds its retractions")
		findings, err := a.alpha.ListFindings(t.Context(), billing.FindingListParams{Type: "derive.grant_effect.excess"})
		require.NoError(t, err)
		require.Len(t, findings.Items, 1, "the pull's convergence saw the drift")
		require.Equal(t, billing.FindingStatus("reconcile_required"), findings.Items[0].Status)
	})
}

// unretractedGrant is drift for convergence to retract: a free grant revoked
// in the ledger whose access window was left live.
func unretractedGrant(t *testing.T, a *authorityWorld) (billing.CustomerID, string) {
	t.Helper()
	const key = "comp:reader"
	customer := billing.CustomerID(uuid.New())
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "comp-reader", DisplayName: "Comp reader", Entitlements: []string{key}})
	require.NoError(t, err)
	granted, err := a.alpha.CreateProductAccess(t.Context(), billing.CreateProductAccessBatchParams{IdempotencyKey: "comp-reader",
		Items: []billing.CreateProductAccessParams{{CustomerID: customer, ProductID: product.ID, Hours: ptr(24 * 365), Reason: billing.GrantReasonComp}}})
	require.NoError(t, err)
	require.NoError(t, a.alpha.RevokeProductAccess(t.Context(), granted[0].ID, billing.RevokeProductAccessParams{Reason: "critical drift"}))
	_, err = a.pool.Exec(t.Context(), "UPDATE "+pgx.Identifier{a.schema, "product_access"}.Sanitize()+" SET revoked_at=NULL, revoke_reason=NULL WHERE id=$1", uuid.UUID(granted[0].ID))
	require.NoError(t, err)
	// Drift no writer made: the revoke's own convergence is not left queued.
	_, err = a.pool.Exec(t.Context(), "DELETE FROM "+pgx.Identifier{a.schema, "river_job"}.Sanitize()+" WHERE kind='openrails.customer_converge'")
	require.NoError(t, err)
	access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: customer, Entitlements: []string{key}})
	require.NoError(t, err)
	require.True(t, access.Entitlements[key])
	return customer, key
}

// nmiAccounts is two independent NMI gateways behind one wire, chosen by the
// request's security key.
type nmiAccounts struct {
	cores map[string]*nmimock.Mock
}

func nmiPSP(name string, archived bool) openrails.PSPConfig {
	return openrails.NMIPSP{GatewayID: name + "-gateway", Environment: openrails.EnvironmentTest, SecurityKey: "nmi-" + name + "-key",
		WebhookSigningSecret: "nmi-" + name + "-webhook", TokenizationKey: "nmi-" + name + "-collect", Archived: archived}.PSPConfig()
}

// nmiAccountsWorld boots with only account "old" declared.
func nmiAccountsWorld(t *testing.T) (*authorityWorld, *nmiAccounts) {
	t.Helper()
	accounts := &nmiAccounts{cores: map[string]*nmimock.Mock{}}
	a := newAuthorityWorld(t, func(w *world) {
		w.clock.Advance(-30 * 24 * time.Hour)
		now := w.clock.Now()
		w.clock.Advance(now.Truncate(24 * time.Hour).Sub(now))
		for _, key := range []string{"nmi-old-key", "nmi-new-key"} {
			accounts.cores[key] = nmimock.NewUnstarted(nmimock.Options{Clock: w.clock.Now})
		}
		server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(out, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			key := r.Header.Get("Authorization")
			if key == "" {
				form, _ := url.ParseQuery(string(body))
				key = form.Get("security_key")
			}
			if key == "" {
				key = r.URL.Query().Get("security_key")
			}
			core := accounts.cores[key]
			if core == nil {
				http.Error(out, "fixture rejects this credential", http.StatusUnauthorized)
				return
			}
			core.ServeHTTP(out, r)
		}))
		t.Cleanup(server.Close)
		target, err := url.Parse(server.URL)
		require.NoError(t, err)
		wire := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(wire.CloseIdleConnections)
		w.deps.NMITransport = loopbackWire{target: target, wire: wire}
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		m := w.cfg.Merchants["alpha"]
		m.PSPs = openrails.PSPs{"old": nmiPSP("old", false)}
		w.cfg.Merchants["alpha"] = m
	})
	return a, accounts
}
