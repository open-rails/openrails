//go:build e2e && integration

package ci_test

import (
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

// A self-acting application and the credential-free host both consume real
// logical command admissions. Retrying a completed revoke consumes nothing;
// the sixth new revoke leaves its credit and the customer's balance intact.
func TestDestructiveCreditRevokesCoverHostAndApplication(t *testing.T) {
	for _, transport := range []string{"host", "application"} {
		t.Run(transport, func(t *testing.T) {
			f := newFixture(t)
			engine, host := f.runtimeWithStripe(t, "destructive-"+transport, nil)
			client := host
			if transport == "application" {
				auth := &authtest.Fake{}
				mux := http.NewServeMux()
				require.NoError(t, openrailshttp.Mount(mux, engine, openrails.Routes{Auth: auth, RouteGroups: everyGroup}))
				server := httptest.NewServer(mux)
				defer server.Close()
				scope := authtest.MerchantScope(host.MerchantID())
				key := auth.Issue(authtest.Grant{Identity: authtest.Application("destructive-command-app"), Scope: scope, Bound: scope, Permissions: []billing.Permission{billing.MerchantCustomerRead, billing.MerchantCustomerManage}})
				var err error
				client, err = openrails.NewRemoteMerchant(server.URL, openrails.RemoteCredential{APIKey: key})
				require.NoError(t, err)
			}
			customer := billing.CustomerID(uuid.New())
			_, err := host.UpdateCustomer(t.Context(), customer, billing.UpdateCustomerParams{})
			require.NoError(t, err)
			var grants []billing.CreditGrantID
			for i := range 6 {
				grant, err := createCreditGrant(t.Context(), host, customer, billing.CreateCreditGrantParams{Currency: "USD", Amount: 1_000_000, Source: "support", SourceID: fmt.Sprintf("destructive-%d", i)})
				require.NoError(t, err)
				grants = append(grants, grant.ID)
			}
			params := billing.RevokeCreditGrantParams{Reason: "correction"}
			for _, id := range grants[:5] {
				_, err := client.RevokeCreditGrant(t.Context(), id, params)
				require.NoError(t, err)
			}
			replay, err := client.RevokeCreditGrant(t.Context(), grants[0], params)
			require.NoError(t, err)
			require.True(t, replay.Replayed)
			_, err = client.RevokeCreditGrant(t.Context(), grants[5], params)
			var refusal *billing.StatusError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, http.StatusTooManyRequests, refusal.Status)
			require.Equal(t, billing.CodeRateLimitExceeded, refusal.Code)
			require.Empty(t, refusal.RetryAfter, "a persistent incident has no timed unlock")
			remaining, err := host.GetCreditGrant(t.Context(), grants[5])
			require.NoError(t, err)
			require.Equal(t, billing.CreditGrantActive, remaining.State)
			require.EqualValues(t, 1_000_000, remaining.RemainingAmount)
			balance, err := host.GetBalance(t.Context(), customer, "USD")
			require.NoError(t, err)
			require.EqualValues(t, 1_000_000, balance.AvailableAmount)
			var actions, holds int64
			require.NoError(t, f.pool.QueryRow(t.Context(), strings.ReplaceAll(`SELECT count(*) FROM billing.destructive_actions WHERE merchant_id=$1 AND kind='revoke_credit'`, "billing.", f.schema+"."), host.MerchantID().UUID()).Scan(&actions))
			require.NoError(t, f.pool.QueryRow(t.Context(), strings.ReplaceAll(`SELECT count(*) FROM billing.destructive_holds WHERE merchant_id=$1 AND cleared_at IS NULL`, "billing.", f.schema+"."), host.MerchantID().UUID()).Scan(&holds))
			require.EqualValues(t, 5, actions)
			require.EqualValues(t, 1, holds)
		})
	}
}
