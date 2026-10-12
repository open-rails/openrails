//go:build e2e && integration

package ci_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
)

// Inspection shares the existing facts across embedded and remote clients,
// needs only usage:read, and never reserves or charges money.
func TestOperationalReadsAreBoundedAndReadOnly(t *testing.T) {
	f := newFixture(t)
	clock := clockwork.NewFakeClockAt(time.Now().UTC().Truncate(time.Microsecond))
	cfg := f.config()
	cfg.Merchants = map[string]openrails.MerchantDeclaration{"inspect-a": {DisplayName: "A"}, "inspect-b": {DisplayName: "B"}}
	engine, err := openrails.New(t.Context(), cfg, openrails.Deps{FXTransport: testFX.Transport(), Postgres: f.pool, Clock: clock})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close(context.Background())) })
	a, err := engine.MerchantBySlug("inspect-a")
	require.NoError(t, err)
	declareDynamicMeter(t, a, "capture", "units")
	declareDynamicMeter(t, a, "tokens", "tokens")
	b, err := engine.MerchantBySlug("inspect-b")
	require.NoError(t, err)
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, engine, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))
	server := httptest.NewServer(mux)
	defer server.Close()
	scope := authtest.MerchantScope(a.MerchantID())
	key := auth.Issue(authtest.Grant{Identity: authtest.Application("inspection"), Scope: scope, Bound: scope, Permissions: []billing.Permission{billing.MerchantUsageRead}})
	reader, err := openrails.NewRemoteMerchant(server.URL, openrails.RemoteCredential{APIKey: key})
	require.NoError(t, err)
	customer, second := billing.CustomerID(uuid.New()), billing.CustomerID(uuid.New())
	for _, shop := range []*openrails.Client{a, b} {
		for _, id := range []billing.CustomerID{customer, second} {
			_, err := shop.CreateCreditGrants(t.Context(), []billing.CreateCreditGrantParams{{CustomerID: id, Currency: "USD", Amount: 10_000_000, Source: "seed", SourceID: id.String()}})
			require.NoError(t, err)
		}
	}

	admittedAt := clock.Now()
	var capturedUsage billing.UsageEventID
	states := []billing.AdmissionState{billing.AdmissionOpen, billing.AdmissionExpired, billing.AdmissionReleased, billing.AdmissionCaptured}
	for _, state := range states {
		deadline := admittedAt.Add(time.Hour)
		if state == billing.AdmissionExpired {
			deadline = admittedAt.Add(time.Minute)
		}
		verdicts, err := a.Admit(t.Context(), []billing.AdmitParams{{RequestID: string(state), CustomerID: customer, Invoker: customer.String(), InvokerType: billing.InvokerTypeCustomer, Currency: "USD", EstimatedAmount: 100_000, ExpiresAt: &deadline}})
		require.NoError(t, err)
		require.True(t, verdicts[0].Allowed())
		if state == billing.AdmissionReleased {
			_, err = a.ReleaseAdmissions(t.Context(), []string{string(state)})
		}
		if state == billing.AdmissionCaptured {
			var receipt *billing.CaptureReceipt
			receipt, err = captureAdmission(t.Context(), a, string(state), 50_000, billing.CaptureUsage{EventType: "capture", Dimensions: map[string]int64{"units": 1}})
			require.NoError(t, err)
			require.NotNil(t, receipt.UsageEventID)
			capturedUsage = *receipt.UsageEventID
		}
		require.NoError(t, err)
	}
	clock.Advance(2 * time.Minute)
	for _, client := range []*openrails.Client{a, reader} {
		for _, state := range states {
			got, err := client.GetAdmission(t.Context(), string(state))
			require.NoError(t, err)
			require.Equal(t, state, *got.State)
			require.False(t, got.Replayed, "inspection does not replay admission")
			if state == billing.AdmissionCaptured {
				require.EqualValues(t, 50_000, *got.CapturedAmount)
			}
		}
		page, err := client.ListAdmissions(t.Context(), billing.AdmissionListParams{CustomerID: customer, State: billing.AdmissionExpired})
		require.NoError(t, err)
		require.Len(t, page.Items, 1, "expiry is derived without a worker")
		require.Equal(t, "expired", page.Items[0].RequestID)
		var names []string
		params := billing.AdmissionListParams{PageRequest: billing.PageRequest{Limit: 2}}
		for {
			page, err := client.ListAdmissions(t.Context(), params)
			require.NoError(t, err)
			for _, item := range page.Items {
				names = append(names, item.RequestID)
			}
			if page.Next == "" {
				break
			}
			params.Cursor = page.Next
		}
		require.Equal(t, []string{"released", "open", "expired", "captured"}, names, "equal-time positions use request IDs without duplicates")
		page, err = client.ListAdmissions(t.Context(), billing.AdmissionListParams{From: admittedAt.Add(time.Second)})
		require.NoError(t, err)
		require.Empty(t, page.Items)
	}
	_, err = b.GetAdmission(t.Context(), "open")
	require.ErrorIs(t, err, billing.ErrNotFound)
	_, err = reader.ReleaseAdmissions(t.Context(), []string{"open"})
	require.ErrorIs(t, err, billing.ErrDenied, "read permission grants no mutation")

	var events []*billing.UsageEvent
	for i, id := range []billing.CustomerID{customer, customer, second} {
		items, err := a.RecordUsage(t.Context(), []billing.RecordUsageParams{{CustomerID: id, Invoker: id.String(), Currency: "USD", EventType: "tokens", Source: "jobs", SourceID: []string{"first", "second", "third"}[i], Amount: new(int64(1_000)), Dimensions: map[string]int64{"tokens": 3}, Metadata: map[string]any{"trace": "kept"}}})
		require.NoError(t, err)
		require.NoError(t, items[0].Err())
		events = append(events, items[0].Event)
	}
	failed, err := a.RecordUsage(t.Context(), []billing.RecordUsageParams{{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "tokens", Source: "jobs", SourceID: "failed", Amount: new(int64(1_000)), Outcome: billing.UsageFailed}})
	require.NoError(t, err)
	require.NoError(t, failed[0].Err())
	events = append(events, failed[0].Event)
	oldAt := clock.Now().Add(-32 * 24 * time.Hour)
	items, err := a.RecordUsage(t.Context(), []billing.RecordUsageParams{{CustomerID: customer, Invoker: customer.String(), Currency: "USD", EventType: "tokens", Source: "jobs", SourceID: "older", Dimensions: map[string]int64{"tokens": 1}, Amount: new(int64(1_000)), OccurredAt: &oldAt}})
	require.NoError(t, err)
	require.NoError(t, items[0].Err())
	older := items[0].Event
	before := balanceOf(t, a, customer)
	for _, client := range []*openrails.Client{a, reader} {
		got, err := client.GetUsageEvent(t.Context(), events[0].ID)
		require.NoError(t, err)
		require.Equal(t, events[0], got, "the original event's money, dimensions, source and ledger identity survive inspection")
		params := billing.UsageEventListParams{PageRequest: billing.PageRequest{Limit: 1}}
		var ids []billing.UsageEventID
		for {
			page, err := client.ListUsageEvents(t.Context(), params)
			require.NoError(t, err)
			for _, event := range page.Items {
				ids = append(ids, event.ID)
			}
			if page.Next == "" {
				break
			}
			params.Cursor = page.Next
		}
		require.ElementsMatch(t, []billing.UsageEventID{capturedUsage, events[0].ID, events[1].ID, events[2].ID, events[3].ID}, ids, "captured usage is inspectable alongside recorded usage")
		page, err := client.ListUsageEvents(t.Context(), billing.UsageEventListParams{CustomerID: customer, Currency: "usd", Source: "jobs", SourceID: "first", EventType: "tokens", Outcome: billing.UsageSucceeded})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, events[0].ID, page.Items[0].ID)
		page, err = client.ListUsageEvents(t.Context(), billing.UsageEventListParams{Outcome: billing.UsageFailed})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.EqualValues(t, 1_000, page.Items[0].Amount+page.Items[0].ForgivenAmount)
		page, err = client.ListUsageEvents(t.Context(), billing.UsageEventListParams{From: oldAt.Add(-time.Hour), To: oldAt.Add(time.Hour)})
		require.NoError(t, err)
		require.Len(t, page.Items, 1)
		require.Equal(t, older.ID, page.Items[0].ID)
		_, err = client.ListUsageEvents(t.Context(), billing.UsageEventListParams{From: clock.Now().Add(-32 * 24 * time.Hour), To: clock.Now()})
		require.ErrorIs(t, err, billing.ErrInvalid)
		_, err = client.ListUsageEvents(t.Context(), billing.UsageEventListParams{SourceID: "first"})
		require.ErrorIs(t, err, billing.ErrInvalid)
	}
	require.Equal(t, before, balanceOf(t, a, customer), "all inspection leaves the ledger unchanged")
	_, err = b.GetUsageEvent(t.Context(), events[0].ID)
	require.ErrorIs(t, err, billing.ErrNotFound)
	page, err := b.ListUsageEvents(t.Context(), billing.UsageEventListParams{})
	require.NoError(t, err)
	require.Empty(t, page.Items, "a different merchant sees none of the events")
	_, err = reader.RecordUsage(t.Context(), []billing.RecordUsageParams{{CustomerID: customer, Currency: "USD", Source: "denied", SourceID: "denied"}})
	require.ErrorIs(t, err, billing.ErrDenied)
	clock.Advance(62 * 24 * time.Hour)
	_, err = a.GetAdmission(t.Context(), "open")
	require.ErrorIs(t, err, billing.ErrNotFound, "physical rows past retention are not inspected")
	admissions, err := a.ListAdmissions(t.Context(), billing.AdmissionListParams{From: admittedAt.Add(-time.Hour)})
	require.NoError(t, err)
	require.Empty(t, admissions.Items)
}
