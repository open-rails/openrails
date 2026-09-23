//go:build integration

package merchantarchive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestInitialMembershipCutoverArchiveRoundTrip(t *testing.T) {
	testPurchaseWorkflowArchive(t, true, "cutover")
}

type initialArchiveCutoverResolver struct {
	mid     uuid.UUID
	clients map[uuid.UUID]*nmi.NMIClient
}

func (r initialArchiveCutoverResolver) ResolveNMIClient(_ context.Context, mid uuid.UUID, psp *uuid.UUID) (*nmi.NMIClient, bool, error) {
	if mid != r.mid || psp == nil {
		return nil, false, nil
	}
	client, ok := r.clients[*psp]
	return client, ok, nil
}

// Compose the real accepted initial writer and real forward cutover producer;
// only the provider HTTP edges are local fixtures. Qualification is fixture-only.
func qualifiedInitialArchiveCutover(t *testing.T, ctx context.Context, d *db.DB, mid merchant.ID, customer, price, sub, source uuid.UUID, sourceClient *nmi.NMIClient, clock clockwork.Clock, anchor time.Time) {
	t.Helper()
	target, method := uuid.New(), uuid.New()
	targetRef := "archive-cutover-" + uuid.NewString()
	plan := nmi.V5Plan{Object: "plan", ID: "writer-target-plan", PlanAmount: "2.50", DayFrequency: "2", PlanPayments: "0"}
	var mu sync.Mutex
	exists, paused := false, true
	creates, activates := 0, 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/customers/target-vault":
			fmt.Fprint(w, `{"object":"customer","id":"target-vault","billing":[{"id":"target-card","priority":1}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/plans/writer-target-plan":
			require.NoError(t, json.NewEncoder(w).Encode(plan))
		case r.Method == http.MethodPost && r.URL.Path == "/subscriptions":
			var request struct {
				PlanID string `json:"plan_id"`
				Paused bool   `json:"paused_subscription"`
				Start  string `json:"start_date"`
				Vault  struct {
					ID        string `json:"id"`
					BillingID string `json:"billing_id"`
				} `json:"customer_vault"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Equal(t, plan.ID, request.PlanID)
			require.Equal(t, "target-vault", request.Vault.ID)
			require.Equal(t, "target-card", request.Vault.BillingID)
			require.True(t, request.Paused)
			require.Equal(t, anchor.UTC().Format("20060102150405"), request.Start)
			exists = true
			paused = true
			creates++
			require.NoError(t, json.NewEncoder(w).Encode(nmi.V5Subscription{Object: "subscription", ID: targetRef, CustomerVaultID: "target-vault", DelayedCondition: "active", PausedSubscription: paused, Amount: "2.50", NextBillingDate: anchor.UTC().Format(time.RFC3339), Plan: &plan}))
		case r.Method == http.MethodPut && r.URL.Path == "/subscriptions/"+targetRef:
			var request struct {
				Paused bool   `json:"paused_subscription"`
				Start  string `json:"start_date"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.False(t, request.Paused)
			require.Equal(t, anchor.UTC().Format("20060102150405"), request.Start)
			paused = false
			activates++
			fmt.Fprint(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/subscriptions/"+targetRef && exists:
			require.NoError(t, json.NewEncoder(w).Encode(nmi.V5Subscription{Object: "subscription", ID: targetRef, CustomerVaultID: "target-vault", DelayedCondition: "active", PausedSubscription: paused, Amount: "2.50", NextBillingDate: anchor.UTC().Format(time.RFC3339), Plan: &plan}))
		default:
			t.Errorf("unexpected cutover provider request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected fixture request", 400)
		}
	}))
	defer gateway.Close()
	_, err := d.Qx(ctx).Exec(ctx, `INSERT INTO openrails.psps(merchant_id,id,rail,environment,account_id,key,evidence) VALUES($1,$2,'nmi','test','archive-target-account','archive-target','{"settings":{}}')`, mid.UUID(), target)
	require.NoError(t, err)
	require.NoError(t, paymentmethods.NewPaymentMethodRepo(d).Create(ctx, &models.PaymentMethod{ID: method, CustomerID: customer, PspID: target, Rail: models.RailNMI, RailCustomerRef: "target-vault", RailMethodRef: "target-card", CreatedAt: clock.Now(), UpdatedAt: clock.Now()}))
	_, err = d.Qx(ctx).Exec(ctx, `INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,plan_id) VALUES($1,$2,$3,$4)`, mid.UUID(), price, target, plan.ID)
	require.NoError(t, err)
	targetClient, err := nmi.NewAccountClient(mid.UUID(), target, "archive-target", &config.NMIProviderSettings{SecurityKey: "archive-target-key", WebhookSecret: "fixture-webhook"}, true)
	require.NoError(t, err)
	targetClient.LoopbackFixture = true
	targetClient.DirectPostURL, targetClient.QueryURL, targetClient.V5BaseURL = gateway.URL, gateway.URL, gateway.URL
	for _, entry := range []struct {
		psp    uuid.UUID
		client *nmi.NMIClient
	}{{source, sourceClient}, {target, targetClient}} {
		version := 0
		qualification := providerqualification.BoundRecord{Record: providerqualification.Record{PSPID: entry.psp, Environment: "test", Contract: providerqualification.NMIContract, EvidenceRef: "loopback-fixture-only"}, CredentialFingerprint: providerqualification.Fingerprint(entry.client.SecurityKey), CredentialVersion: &version}
		evidence, err := json.Marshal(map[string]any{"settings": map[string]any{"nmi_cutover_qualification": qualification}})
		require.NoError(t, err)
		_, err = d.Qx(ctx).Exec(ctx, `UPDATE openrails.psps SET evidence=$3 WHERE merchant_id=$1 AND id=$2`, mid.UUID(), entry.psp, evidence)
		require.NoError(t, err)
	}
	_, err = d.Qx(ctx).Exec(ctx, `UPDATE openrails.psps SET archived=true WHERE merchant_id=$1 AND id=$2`, mid.UUID(), source)
	require.NoError(t, err)
	_, err = d.Qx(ctx).Exec(ctx, `UPDATE openrails.destructive_action_switch SET enabled=true`)
	require.NoError(t, err)
	handler := &intents.NMIProviderCutover{DB: d, Resolver: initialArchiveCutoverResolver{mid.UUID(), map[uuid.UUID]*nmi.NMIClient{source: sourceClient, target: targetClient}}, Clock: clock}
	runner := &intents.Runner{Store: intents.NewStore(d), Registry: intents.NewRegistry(handler), Config: &config.Config{TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeFull}}
	result, err := handler.Submit(ctx, runner, sub, "archive-cutover-"+uuid.NewString(), openrails.ProviderCutoverRequest{TargetPaymentMethodID: openrails.PaymentMethodID(method), ExpectedSourcePSPID: source, ExpectedTargetPSPID: target}, intents.OriginAdmin)
	require.NoError(t, err)
	canonical, err := intents.NewStore(d).Get(ctx, result.ID)
	require.NoError(t, err)
	if canonical.LastFailureReason != nil {
		t.Logf("cutover outcome: %s", *canonical.LastFailureReason)
	}
	require.Equal(t, intents.StatusSucceeded, result.Status)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, creates)
	require.Equal(t, 1, activates)
}
