//go:build integration

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/permissions"
)

func TestPolicyStateIntegrity(t *testing.T) {
	f := newTreasuryWorkflow(t)
	pool := dbtest.SharedSuperuserPGXPool(t)
	t.Run("replacement_rollback", func(t *testing.T) {
		payer, token := f.actor(t, []string{permissions.CustomerAll})
		original := openrails.SpendDelegationInput{Scope: "invoker", ScopeKey: "original", Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 100, Currency: "USD"}}}
		require.NoError(t, f.client.SetCustomerSpendDelegation(t.Context(), (payer).String(), original))
		name := "policy_fail_" + uuid.NewString()[:8]
		failingKey := "failure-" + uuid.NewString()
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`CREATE SEQUENCE billing.%s;
GRANT USAGE,SELECT ON SEQUENCE billing.%s TO openrails_app;
CREATE FUNCTION billing.%s() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN PERFORM nextval('billing.%s'); RAISE EXCEPTION 'injected policy replacement failure'; END $$;
CREATE TRIGGER %s BEFORE INSERT OR UPDATE ON billing.invoker_spend_limits
FOR EACH ROW WHEN (NEW.merchant_id = '%s'::uuid AND NEW.customer_id = '%s'::uuid AND NEW.scope_key = '%s')
EXECUTE FUNCTION billing.%s()`, name, name, name, name, name, f.merchant.MerchantID, payer, failingKey, name))
		require.NoError(t, err)
		t.Cleanup(func() {
			_, err := pool.Exec(context.Background(), fmt.Sprintf("DROP TRIGGER %s ON billing.invoker_spend_limits; DROP FUNCTION billing.%s(); DROP SEQUENCE billing.%s", name, name, name))
			require.NoError(t, err)
		})
		failed := original
		failed.ScopeKey = failingKey
		failed.Windows = []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 999, Currency: "USD"}}
		failure := f.client.SetCustomerSpendDelegations(t.Context(), (payer).String(), []openrails.SpendDelegationInput{failed})
		var serverError *openrails.StatusError
		require.ErrorAs(t, failure, &serverError)
		require.Equal(t, http.StatusInternalServerError, serverError.Status)
		// Sequence advancement survives rollback and proves the injected write
		// fault ran; an auth/validation rejection cannot satisfy this witness.
		var reached bool
		require.NoError(t, pool.QueryRow(t.Context(), "SELECT is_called FROM billing."+name).Scan(&reached))
		require.True(t, reached, "the write reached the injected rollback point")
		status, raw := requestWorkflowJSON(t, http.MethodGet, f.hostURL+"/v1/customers/"+payer.String()+"/spend-delegations", token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var doc struct {
			Delegations []openrails.SpendDelegationInput `json:"delegations"`
		}
		require.NoError(t, json.Unmarshal(raw, &doc))
		require.Equal(t, []openrails.SpendDelegationInput{original}, doc.Delegations, "failed replacement rolls back both deletion and insertion")
	})
	t.Run("replace_and_upsert_serialize", func(t *testing.T) {
		ctx := t.Context()
		payer, token := f.actor(t, []string{permissions.CustomerAll})
		blocker, err := pool.Acquire(ctx)
		require.NoError(t, err)
		tx, err := blocker.Begin(ctx)
		require.NoError(t, err)
		released := false
		t.Cleanup(func() {
			if !released {
				require.NoError(t, tx.Rollback(context.Background()))
				blocker.Release()
			}
		})
		key := f.merchant.MerchantID.String() + ":" + payer.String()
		_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key)
		require.NoError(t, err)
		replacement := openrails.SpendDelegationInput{Scope: "role", ScopeKey: uuid.NewString(), Windows: []openrails.SpendLimitWindow{{Key: "week", WindowSeconds: 604800, Limit: 700, Currency: "USD"}}}
		singular := openrails.SpendDelegationInput{Scope: "invoker", ScopeKey: "concurrent-worker", Windows: []openrails.SpendLimitWindow{{Key: "day", WindowSeconds: 86400, Limit: 300, Currency: "USD"}}}
		replaceDone, upsertDone := make(chan error, 1), make(chan error, 1)
		go func() {
			replaceDone <- f.client.SetCustomerSpendDelegations(ctx, (payer).String(), []openrails.SpendDelegationInput{replacement})
		}()
		waiters := func(want int) bool {
			var count int
			err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock(hashtextextended%'`).Scan(&count)
			return err == nil && count >= want
		}
		require.Eventually(t, func() bool { return waiters(1) }, 5*time.Second, 20*time.Millisecond, "replacement must really wait on the payer lock")
		go func() { upsertDone <- f.client.SetCustomerSpendDelegation(ctx, (payer).String(), singular) }()
		require.Eventually(t, func() bool { return waiters(2) }, 5*time.Second, 20*time.Millisecond, "independent HTTP requests must contend on the same lock")
		require.NoError(t, tx.Commit(ctx))
		blocker.Release()
		released = true
		require.NoError(t, <-replaceDone)
		require.NoError(t, <-upsertDone)
		status, raw := requestWorkflowJSON(t, http.MethodGet, f.hostURL+"/v1/customers/"+payer.String()+"/spend-delegations", token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var doc struct {
			Delegations []openrails.SpendDelegationInput `json:"delegations"`
		}
		require.NoError(t, json.Unmarshal(raw, &doc))
		require.ElementsMatch(t, []openrails.SpendDelegationInput{replacement, singular}, doc.Delegations, "queued replacement then upsert both survive")
	})
	t.Run("invalid_persisted_configuration", func(t *testing.T) {
		require.NoError(t, f.client.SetMerchantSettings(t.Context(), openrails.MerchantSettings{Profile: &openrails.MerchantProfileInput{DisplayName: "before corruption"}}))
		var original []byte
		require.NoError(t, pool.QueryRow(t.Context(), `SELECT config FROM billing.merchant_configurations WHERE merchant_id=$1`, f.merchant.MerchantID.UUID()).Scan(&original))
		t.Cleanup(func() {
			_, err := pool.Exec(context.Background(), `UPDATE billing.merchant_configurations SET config=$1::jsonb WHERE merchant_id=$2`, original, f.merchant.MerchantID.UUID())
			require.NoError(t, err)
		})
		changed, err := pool.Exec(t.Context(), `UPDATE billing.merchant_configurations SET config='{"delegated_invoker_wasted_spend_windows":[{"key":"bad","window_seconds":"oops","limit":1}]}'::jsonb WHERE merchant_id=$1`, f.merchant.MerchantID.UUID())
		require.NoError(t, err)
		require.EqualValues(t, 1, changed.RowsAffected())
		payer, _ := f.actor(t, nil)
		_, err = f.embedded.ReportWastedSpend(t.Context(), openrails.WastedSpendReport{CustomerID: (payer).String(), Invoker: "bad-config", Currency: "USD", Amount: 1, Source: "test", SourceID: uuid.NewString()})
		require.Error(t, err, "malformed persisted windows must not silently fall back to a spending allowance")
	})
}
