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
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/nmimock"
	"github.com/stretchr/testify/require"
)

type nmiJournal struct {
	core   *nmimock.Mock
	pool   *pgxpool.Pool
	schema string
	mu     sync.Mutex
	errors []error
}

// NMI's external account and its economic journal stay independent from the
// application's merchant schema and worker lifecycle. Reads report real
// provider transactions; they never synthesize an OpenRails renewal result.
func newNMIWorld(t *testing.T, configure ...func(*world)) (*authorityWorld, *nmiJournal) {
	t.Helper()
	var p *nmiJournal
	a := newAuthorityWorld(t, func(w *world) {
		w.clock.Advance(-30 * 24 * time.Hour)
		// NMI schedules carry a calendar date, so start this weekly contract
		// at UTC midnight and keep its literal seven-day period unambiguous.
		now := w.clock.Now()
		w.clock.Advance(now.Truncate(24 * time.Hour).Sub(now))
		p = &nmiJournal{core: nmimock.NewUnstarted(nmimock.Options{Clock: w.clock.Now}), pool: w.pool, schema: "nmi_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
		_, err := p.pool.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{p.schema}.Sanitize()+"; CREATE TABLE "+pgx.Identifier{p.schema, "transactions"}.Sanitize()+" (id text PRIMARY KEY, cents bigint NOT NULL, refunded bigint NOT NULL, voided boolean NOT NULL DEFAULT false)")
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := p.pool.Exec(context.Background(), "DROP SCHEMA "+pgx.Identifier{p.schema}.Sanitize()+" CASCADE")
			require.NoError(t, err)
		})
		server := httptest.NewServer(http.HandlerFunc(func(out http.ResponseWriter, request *http.Request) {
			response := httptest.NewRecorder()
			p.core.ServeHTTP(response, request)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := p.persist(ctx)
			cancel()
			if err != nil {
				p.mu.Lock()
				p.errors = append(p.errors, err)
				p.mu.Unlock()
				http.Error(out, "provider journal failed", 503)
				return
			}
			for key, values := range response.Header() {
				out.Header()[key] = values
			}
			out.WriteHeader(response.Code)
			_, _ = out.Write(response.Body.Bytes())
		}))
		t.Cleanup(server.Close)
		t.Cleanup(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			require.Empty(t, p.errors)
			require.Empty(t, p.core.Unexpected(), "every external provider request must be explicitly modeled")
		})
		target, err := url.Parse(server.URL)
		require.NoError(t, err)
		wire := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(wire.CloseIdleConnections)
		w.deps.NMITransport = loopbackWire{target: target, wire: wire}
		w.cfg.ProviderWriteMode = openrails.ProviderWritesFull
		merchant := w.cfg.Merchants["alpha"]
		merchant.PSPs = openrails.PSPs{"nmi": openrails.NMIPSP{GatewayID: "critical-gateway", Environment: openrails.EnvironmentTest, SecurityKey: "critical-nmi-key", WebhookSigningSecret: "critical-nmi-webhook", TokenizationKey: "critical-collect-key"}.PSPConfig()}
		merchant.CardVault = "nmi"
		w.cfg.Merchants["alpha"] = merchant
		for _, change := range configure {
			change(w)
		}
	})
	return a, p
}

func (p *nmiJournal) persist(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	table := pgx.Identifier{p.schema, "transactions"}.Sanitize()
	for _, entry := range p.core.Ledger("") {
		if _, err = tx.Exec(ctx, "INSERT INTO "+table+" (id,cents,refunded) VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET refunded=EXCLUDED.refunded", entry.TransactionID, entry.Cents, entry.RefundedCents); err != nil {
			return err
		}
	}
	for _, sale := range p.core.Sales() {
		if sale.Voided {
			if _, err = tx.Exec(ctx, "UPDATE "+table+" SET voided=true WHERE id=$1", sale.TransactionID); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

func (p *nmiJournal) money(t *testing.T, transactions int, net int64) {
	t.Helper()
	var count int
	var amount int64
	err := p.pool.QueryRow(t.Context(), "SELECT count(*),COALESCE(sum(CASE WHEN voided THEN 0 ELSE cents-refunded END),0) FROM "+pgx.Identifier{p.schema, "transactions"}.Sanitize()).Scan(&count, &amount)
	require.NoError(t, err)
	require.Equal(t, transactions, count)
	require.Equal(t, net, amount)
}

func nmiMembership(t *testing.T, configure ...func(*nmimock.Schedule)) (*authorityWorld, *nmiJournal, *billing.Subscription) {
	t.Helper()
	a, p := newNMIWorld(t)
	p.core.AddPlan(nmimock.Plan{ID: "weekly", Name: "Weekly", Amount: "10.00", Days: 7})
	product, err := a.alpha.CreateProduct(t.Context(), billing.CreateProductParams{Key: "nmi-membership", DisplayName: "NMI membership", Entitlements: []string{"nmi:member"}})
	require.NoError(t, err)
	price, err := a.alpha.CreateProviderPrice(t.Context(), billing.CreateProviderPriceParams{ProductID: product.ID, Key: "nmi-weekly", PSP: "nmi", PlanID: "weekly", Currency: "USD"})
	require.NoError(t, err)
	// NMI-owned prices are retained legacy schedules, not new checkout
	// products. Construct real external receipts and import their accepted
	// terms through the public migration boundary, never private row edits.
	started, due := a.clock.Now(), a.clock.Now().Add(7*24*time.Hour)
	vaultID := p.core.AddVault(nmimock.Card{Brand: "visa", Last4: "4242"})
	vault := p.core.Vault(vaultID)
	schedule := nmimock.Schedule{Vault: vaultID, Plan: "weekly", Amount: "10.00", Days: 7, Created: started, NextBilling: due, Order: "legacy-nmi-membership"}
	for _, change := range configure {
		change(&schedule)
	}
	scheduleID := p.core.AddSchedule(schedule)
	initial := p.core.AddScheduleSale(scheduleID, started)
	require.NoError(t, p.persist(t.Context()))
	customer := billing.CustomerID(uuid.MustParse(a.alice.ID))
	book := billing.DeclaredBilling{AsOf: started, DefaultPSP: billing.PSPRef{Key: "nmi"},
		Customers:      []billing.DeclaredCustomer{{Customer: customer}},
		PaymentMethods: []billing.DeclaredPaymentMethod{{Customer: customer, Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID, RecurringTransactionID: initial.TransactionID, CreatedAt: started, Card: &billing.CardDetails{Brand: ptr("visa"), Last4: ptr("4242")}}},
		Subscriptions: []billing.DeclaredSubscription{{SourceID: "known-nmi-membership", Customer: customer, Price: price.ID, Rail: "nmi", RailSubscriptionID: scheduleID, StartedAt: started, PaidThrough: &due,
			PaymentMethod:    &billing.PaymentMethodRef{Rail: "nmi", RailCustomerRef: vaultID, RailMethodRef: vault.BillingID},
			AcceptedSchedule: &billing.DeclaredProviderSchedule{SourceReference: scheduleID, Amount: 10_000_000, Currency: "USD", IntervalHours: 168, FirstDueAt: due}}},
		Transactions: []billing.DeclaredTransaction{{SourceID: "known-nmi-initial-receipt", RailSubscriptionID: scheduleID, TransactionID: initial.TransactionID, Type: "sale", Success: true, Amount: 10_000_000, Currency: "USD", OccurredAt: started, NextBillingAt: &due}},
	}
	imported, err := a.alpha.ImportBilling(t.Context(), book)
	require.NoError(t, err)
	require.Empty(t, imported.Blocked, "%+v", imported.Reasons)
	require.Contains(t, imported.Imported, "known-nmi-membership")
	subscriptions, err := a.alpha.ListSubscriptions(t.Context(), billing.SubscriptionListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, subscriptions.Items, 1)
	sub, err := a.alpha.GetSubscription(t.Context(), subscriptions.Items[0].ID)
	require.NoError(t, err)
	require.Equal(t, "nmi_schedule", sub.CollectionPolicy)
	require.NotNil(t, sub.RailSubscriptionID)
	require.Equal(t, scheduleID, *sub.RailSubscriptionID)
	require.Equal(t, started, *sub.CurrentPeriodStartsAt)
	require.Equal(t, due, *sub.CurrentPeriodEndsAt)
	require.Equal(t, 7*24*time.Hour, sub.CurrentPeriodEndsAt.Sub(*sub.CurrentPeriodStartsAt))
	payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: customer})
	require.NoError(t, err)
	require.Len(t, payments.Items, 1)
	require.EqualValues(t, 10_000_000, payments.Items[0].Amount)
	require.Equal(t, initial.TransactionID, payments.Items[0].TransactionID)
	p.money(t, 1, 1000)
	return a, p, sub
}

func nmiRenewalReadback(t *testing.T, a *authorityWorld, p *nmiJournal, sub *billing.Subscription) {
	t.Helper()
	require.Eventually(t, func() bool {
		current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
		return err == nil && current.CurrentPeriodEndsAt.Equal(sub.CurrentPeriodEndsAt.Add(7*24*time.Hour))
	}, 20*time.Second, 20*time.Millisecond)
	payments, err := a.alpha.ListPayments(t.Context(), billing.PaymentListParams{CustomerID: sub.CustomerID})
	require.NoError(t, err)
	require.Len(t, payments.Items, 2)
	for _, payment := range payments.Items {
		require.EqualValues(t, 10_000_000, payment.Amount)
		require.Equal(t, billing.PaymentSucceeded, payment.Status)
	}
	access, err := a.alpha.CheckEntitlements(t.Context(), billing.CheckEntitlementsParams{CustomerID: sub.CustomerID, Entitlements: []string{"nmi:member"}})
	require.NoError(t, err)
	require.True(t, access.Entitlements["nmi:member"])
	events, err := a.alpha.ListEvents(t.Context(), billing.EventQuery{Types: []string{"payment.settled"}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, events.Events, 2, "imported initial receipt and exactly one renewed receipt")
	p.money(t, 2, 2000)
}

func TestNMIRecovery(t *testing.T) {
	t.Run("OR-J15-S01/missing-callback-is-repaired-from-provider-query", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		due := *sub.CurrentPeriodEndsAt
		p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
		p.core.EditSchedule(*sub.RailSubscriptionID, func(s *nmimock.Schedule) { s.NextBilling = due.Add(7 * 24 * time.Hour) })
		require.NoError(t, p.persist(t.Context()))
		a.clock.Advance(due.Sub(a.clock.Now()) + 25*time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.rebill_watch", 2)
		nmiRenewalReadback(t, a, p, sub)
		runRegistered(t, fleet, "openrails.rebill_watch", 2)
		nmiRenewalReadback(t, a, p, sub)
	})
	t.Run("OR-J15-S02", func(t *testing.T) {
		for _, profile := range []string{"unavailable-query", "lagging-index"} {
			t.Run(profile, func(t *testing.T) {
				a, p, sub := nmiMembership(t)
				due := *sub.CurrentPeriodEndsAt
				sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
				require.NoError(t, p.persist(t.Context()))
				if profile == "unavailable-query" {
					p.core.QueryUnavailable(true)
				} else {
					p.core.EditSale(sale.TransactionID, func(s *nmimock.Sale) { s.Hidden = true })
				}
				a.clock.Advance(due.Sub(a.clock.Now()) + 25*time.Hour)
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.rebill_watch", 2)
				p.money(t, 2, 2000)
				current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				require.Equal(t, due, *current.CurrentPeriodEndsAt)
				p.core.QueryUnavailable(false)
				p.core.Reveal()
				p.core.EditSchedule(*sub.RailSubscriptionID, func(s *nmimock.Schedule) { s.NextBilling = due.Add(7 * 24 * time.Hour) })
				runRegistered(t, fleet, "openrails.rebill_watch", 2)
				nmiRenewalReadback(t, a, p, sub)
			})
		}
	})
	t.Run("OR-J15-S03/provider-skipped-cycle-is-collected-after-authoritative-absence", func(t *testing.T) {
		a, p, sub := nmiMembership(t)
		due := *sub.CurrentPeriodEndsAt
		p.core.EditSchedule(*sub.RailSubscriptionID, func(s *nmimock.Schedule) { s.NextBilling = due.Add(7 * 24 * time.Hour) })
		a.clock.Advance(due.Sub(a.clock.Now()) + 25*time.Hour)
		fleet := startFleet(t, a.world)
		runRegistered(t, fleet, "openrails.rebill_watch", 2)
		runRegistered(t, fleet, "openrails.dunning", 2)
		nmiRenewalReadback(t, a, p, sub)
		runRegistered(t, fleet, "openrails.rebill_watch", 2)
		nmiRenewalReadback(t, a, p, sub)
	})
	t.Run("OR-J15-S04", func(t *testing.T) {
		for _, profile := range []string{"provider_reversed", "provider_unrecorded"} {
			t.Run(profile, func(t *testing.T) {
				a, p, sub := nmiMembership(t)
				due := *sub.CurrentPeriodEndsAt
				net := int64(2000)
				if profile == "provider_reversed" {
					sale := p.core.AddScheduleSale(*sub.RailSubscriptionID, due)
					require.NoError(t, p.persist(t.Context()))
					p.core.Void(sale.TransactionID)
					net = 1000
				} else {
					schedule := p.core.Schedule(*sub.RailSubscriptionID)
					p.core.AddSale(nmimock.Sale{Vault: schedule.Vault, OrderID: "external-unrecorded-payment", Amount: "10.00", At: due.Add(time.Hour)})
				}
				require.NoError(t, p.persist(t.Context()))
				p.core.EditSchedule(*sub.RailSubscriptionID, func(s *nmimock.Schedule) { s.NextBilling = due.Add(7 * 24 * time.Hour) })
				a.clock.Advance(due.Sub(a.clock.Now()) + 25*time.Hour)
				fleet := startFleet(t, a.world)
				runRegistered(t, fleet, "openrails.rebill_watch", 2)
				runRegistered(t, fleet, "openrails.dunning", 2)
				p.money(t, 2, net)
				current, err := a.alpha.GetSubscription(t.Context(), sub.ID)
				require.NoError(t, err)
				require.Equal(t, due, *current.CurrentPeriodEndsAt)
				cycles, err := a.alpha.ListRenewals(t.Context(), billing.RenewalListParams{SubscriptionID: sub.ID})
				require.NoError(t, err)
				require.Len(t, cycles.Items, 1)
				require.Equal(t, profile, cycles.Items[0].MissReason)
			})
		}
	})
}
