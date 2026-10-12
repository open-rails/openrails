//go:build e2e && integration

package ci_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth/authtest"
	"github.com/stretchr/testify/require"
)

// The in-process host's own revokes are audited but spend no allowance and
// are never held. A staff member, an application and a person the host names
// are each held at their sixth new revoke, and that incident holds no one
// else; retrying a completed revoke consumes nothing. Host rows count toward
// no limit, and a merchant incident holds every actor but the host.
func TestDestructiveCreditRevokesHoldActorsNotTheHost(t *testing.T) {
	f := newFixture(t)
	engine, host := f.runtimeWithStripe(t, "destructive-revokes", nil)
	auth := &authtest.Fake{}
	mux := http.NewServeMux()
	require.NoError(t, openrailshttp.Mount(mux, engine, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))
	server := httptest.NewServer(mux)
	defer server.Close()
	scope := authtest.MerchantScope(host.MerchantID())
	overHTTP := func(who openrails.Identity) *openrails.Client {
		key := auth.Issue(authtest.Grant{Identity: who, Scope: scope, Bound: scope, Permissions: []billing.Permission{billing.MerchantCustomerRead, billing.MerchantCustomerManage}})
		client, err := openrails.NewRemoteMerchant(server.URL, openrails.RemoteCredential{APIKey: key})
		require.NoError(t, err)
		return client
	}
	named := func(id string) context.Context {
		return openrails.WithInvoker(t.Context(), openrails.Invoker{Issuer: authtest.Issuer, ID: id})
	}

	ctx := t.Context()
	customer := billing.CustomerID(uuid.New())
	_, err := host.UpdateCustomer(ctx, customer, billing.UpdateCustomerParams{})
	require.NoError(t, err)
	params := billing.RevokeCreditGrantParams{Reason: "correction"}
	grants := 0
	revoke := func(ctx context.Context, client *openrails.Client) (billing.CreditGrantID, error) {
		grants++
		grant, err := createCreditGrant(t.Context(), host, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: fmt.Sprintf("destructive-%d", grants)})
		require.NoError(t, err)
		_, err = client.RevokeCreditGrant(ctx, grant.ID, params)
		return grant.ID, err
	}
	held := func(id billing.CreditGrantID, err error) {
		t.Helper()
		var refusal *billing.StatusError
		require.ErrorAs(t, err, &refusal)
		require.Equal(t, http.StatusTooManyRequests, refusal.Status)
		require.Equal(t, billing.CodeRateLimitExceeded, refusal.Code)
		require.Empty(t, refusal.RetryAfter, "a persistent incident has no timed unlock")
		left, err := host.GetCreditGrant(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, billing.CreditGrantActive, left.State, "a held revoke leaves its credit")
		require.EqualValues(t, 1_000_000, left.RemainingAmount)
	}
	allowance := func(ctx context.Context, client *openrails.Client, who string) {
		t.Helper()
		first, err := revoke(ctx, client)
		require.NoError(t, err, "%s's first revoke", who)
		for i := 2; i <= 5; i++ {
			_, err := revoke(ctx, client)
			require.NoError(t, err, "%s's revoke %d", who, i)
		}
		replay, err := client.RevokeCreditGrant(ctx, first, params)
		require.NoError(t, err)
		require.True(t, replay.Replayed)
		held(revoke(ctx, client))
	}
	count := func(query string, args ...any) int64 {
		t.Helper()
		var n int64
		require.NoError(t, f.pool.QueryRow(t.Context(), strings.ReplaceAll(query, "billing.", f.schema+"."), args...).Scan(&n))
		return n
	}
	openHolds := func(scope string) int64 {
		return count(`SELECT count(*) FROM billing.destructive_holds WHERE scope=$1 AND cleared_at IS NULL`, scope)
	}

	// More of the host's own revokes than a merchant may make in an hour.
	for i := range 16 {
		_, err := revoke(ctx, host)
		require.NoError(t, err, "host revoke %d", i+1)
	}
	require.Zero(t, count(`SELECT count(*) FROM billing.destructive_holds`))
	require.EqualValues(t, 16, count(`SELECT count(*) FROM billing.destructive_actions WHERE origin='host' AND kind='revoke_credit' AND state='succeeded'`), "the host's work stays in the audit")

	// None of that spent an allowance: each actor has its own five.
	allowance(ctx, overHTTP(authtest.User("bob")), "a staff member")
	allowance(ctx, overHTTP(authtest.Application("destructive-command-app")), "an application")
	allowance(named("alice"), host, "a person the host names")
	held(revoke(ctx, overHTTP(authtest.User("alice"))))
	_, err = revoke(ctx, host)
	require.NoError(t, err, "a person's incident does not hold the host")
	require.EqualValues(t, 3, openHolds("actor"))
	require.EqualValues(t, 1, count(`SELECT count(*) FROM billing.destructive_holds WHERE scope='actor' AND actor_issuer=$1 AND actor_id='alice' AND cleared_at IS NULL`, authtest.Issuer))

	// Fifteen staff revokes fill the merchant's hour; the next actor opens a
	// merchant incident, which holds a named person but not the host.
	held(revoke(ctx, overHTTP(authtest.User("carol"))))
	require.EqualValues(t, 1, openHolds("merchant"))
	_, err = revoke(ctx, host)
	require.NoError(t, err, "a merchant incident does not hold the host")
	held(revoke(named("dave"), host))
	require.EqualValues(t, 15, count(`SELECT count(*) FROM billing.destructive_actions WHERE origin='staff'`))
	require.EqualValues(t, 18, count(`SELECT count(*) FROM billing.destructive_actions WHERE origin='host'`))
}
