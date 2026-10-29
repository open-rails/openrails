//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/providerposture"
	"github.com/stretchr/testify/require"
)

func TestNMICollectionRefusal(t *testing.T) {
	t.Run("OR-J26-S55/a-never-sent-collection-retains-its-proof", func(t *testing.T) {
		for _, interrupted := range []bool{false, true} {
			name := "ordinary-completion"
			if interrupted {
				name = "interrupted-completion"
			}
			t.Run(name, func(t *testing.T) {
				a, p, customer := usageBillingWorld(t, true, false)
				psps, err := a.alpha.ListPSPs(t.Context(), billing.PSPListParams{})
				require.NoError(t, err)
				require.Len(t, psps.Items, 1)
				psp := psps.Items[0]
				client, err := nmi.NewAccountClient(a.alpha.MerchantID().UUID(), psp.ID.UUID(), psp.GatewayID, &config.NMIProviderSettings{SecurityKey: "critical-nmi-key", Environment: config.EnvironmentTest})
				require.NoError(t, err)
				key := client.PostureKey()
				restore := func() {
					providerposture.Process().Verify(t.Context(), key, func(context.Context) (providerposture.Verdict, error) { return providerposture.Simulated, nil })
				}
				t.Cleanup(restore)
				status := providerposture.Process().Verify(t.Context(), key, func(context.Context) (providerposture.Verdict, error) { return providerposture.Live, nil })
				require.False(t, status.Armed(), "a real-money verdict cannot arm a test account")
				if interrupted {
					_, err := a.pool.Exec(t.Context(), "CREATE FUNCTION "+pgx.Identifier{a.schema, "refuse_collection_finish"}.Sanitize()+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.intent_type='order_collection' AND NEW.status='failed_terminal' THEN RAISE EXCEPTION 'critical collection completion fault'; END IF; RETURN NEW; END $$; CREATE TRIGGER refuse_collection_finish BEFORE UPDATE ON "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" FOR EACH ROW EXECUTE FUNCTION "+pgx.Identifier{a.schema, "refuse_collection_finish"}.Sanitize()+"()")
					require.NoError(t, err)
					t.Cleanup(func() {
						_, err := a.pool.Exec(context.Background(), "DROP TRIGGER IF EXISTS refuse_collection_finish ON "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize())
						require.NoError(t, err)
					})
				}
				oneUsage(t, a.alpha, usageInput(customer, "unsent-collection", 3))
				fleet := startFleet(t, a.world)
				runUsageDue(t, fleet)
				var state string
				var evidence []byte
				require.Eventually(t, func() bool {
					err := a.pool.QueryRow(t.Context(), "SELECT status,result_evidence FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" WHERE merchant_id=$1 AND intent_type=$2 ORDER BY created_at,id LIMIT 1", a.alpha.MerchantID().UUID(), intents.TypeOrderCollection).Scan(&state, &evidence)
					return err == nil && state != intents.StatusPending && state != intents.StatusInFlight
				}, 15*time.Second, 20*time.Millisecond, "the registered worker must finish its first collection")
				p.money(t, 0, 0)
				var proof struct {
					NotExecuted bool   `json:"not_executed"`
					Code        string `json:"not_executed_code"`
				}
				require.NoError(t, json.Unmarshal(evidence, &proof))
				if interrupted {
					require.Equal(t, intents.StatusUnknownNeedsVerify, state)
					var retained struct {
						SubmittedAt string          `json:"submitted_at"`
						Qualified   json.RawMessage `json:"qualified_collection_nonexecution"`
					}
					require.NoError(t, json.Unmarshal(evidence, &retained))
					require.NotEmpty(t, retained.SubmittedAt)
					require.NotEmpty(t, retained.Qualified, "the qualified refusal must survive a failed local completion")
					_, err := a.pool.Exec(t.Context(), "DROP TRIGGER refuse_collection_finish ON "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize())
					require.NoError(t, err)
					a.clock.Advance(time.Hour)
					require.Eventually(t, func() bool {
						wakeJobs(t, fleet, "openrails.provider_operation")
						err := a.pool.QueryRow(t.Context(), "SELECT status,result_evidence FROM "+pgx.Identifier{a.schema, "provider_intents"}.Sanitize()+" WHERE merchant_id=$1 AND intent_type=$2 ORDER BY created_at,id LIMIT 1", a.alpha.MerchantID().UUID(), intents.TypeOrderCollection).Scan(&state, &evidence)
						return err == nil && state == intents.StatusFailedTerminal
					}, 15*time.Second, 20*time.Millisecond)
					p.money(t, 0, 0)
					require.NoError(t, json.Unmarshal(evidence, &proof))
				}
				require.Equal(t, intents.StatusFailedTerminal, state, "a proven unsent charge must not wait for a nonexistent provider receipt")
				require.True(t, proof.NotExecuted)
				require.Equal(t, "not_dispatched", proof.Code)
				order := oneUsageOrder(t, a, customer)
				require.Equal(t, billing.OrderOpen, order.Status)
				require.Zero(t, order.Usage.AmountPaid)
				require.EqualValues(t, 30_000_000, order.Usage.AmountDue)
				restore()
				runUsageDue(t, fleet)
				require.Eventually(t, func() bool { return oneUsageOrder(t, a, customer).Status == billing.OrderComplete }, 15*time.Second, 20*time.Millisecond)
				runUsageDue(t, fleet)
				p.money(t, 1, 3000)
			})
		}
	})
}
