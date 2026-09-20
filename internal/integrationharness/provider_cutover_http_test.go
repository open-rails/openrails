//go:build integration

package integrationharness

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

// This server implements the documented v5 requests, keeps independent state
// per authenticated account, and injects loss AFTER committing provider state.
// It deliberately has no sale endpoint: any accidental initial charge fails.
type cutoverAccount struct {
	Plan                          nmi.V5Plan
	Subs                          map[string]nmi.V5Subscription
	Mode                          string
	Creates, Deletes, Activations int
	Requests                      int
	Source                        bool
}
type cutoverGateway struct {
	mu                    sync.Mutex
	Accounts              map[string]*cutoverAccount
	Server                *httptest.Server
	AfterSubscriptionRead func(source bool, sub nmi.V5Subscription)
	AfterCreate           func()
}

func newCutoverGateway(t *testing.T) *cutoverGateway {
	g := &cutoverGateway{Accounts: map[string]*cutoverAccount{}}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		acct := g.Accounts[r.Header.Get("Authorization")]
		if acct == nil {
			http.Error(w, "wrong account credential", 401)
			return
		}
		acct.Requests++
		w.Header().Set("Content-Type", "application/json")
		if acct.Mode == "dark" {
			http.Error(w, "provider unavailable", 503)
			return
		}
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/customers/"):
			id := strings.TrimPrefix(path, "/customers/")
			_ = json.NewEncoder(w).Encode(nmi.V5Customer{Object: "customer", ID: id, Billing: []nmi.V5CustomerBilling{{ID: "only-card"}}})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/plans/"):
			if strings.TrimPrefix(path, "/plans/") != acct.Plan.ID {
				w.WriteHeader(404)
				return
			}
			_ = json.NewEncoder(w).Encode(acct.Plan)
		case r.Method == http.MethodPost && path == "/subscriptions":
			var req struct {
				PlanID string `json:"plan_id"`
				Vault  struct {
					ID        string `json:"id"`
					BillingID string `json:"billing_id"`
				} `json:"customer_vault"`
				Paused    bool   `json:"paused_subscription"`
				StartDate string `json:"start_date"`
			}
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			if e := dec.Decode(&req); e != nil || !req.Paused || req.PlanID != acct.Plan.ID || req.Vault.ID == "" || req.Vault.BillingID != "only-card" || acct.Source {
				http.Error(w, "invalid enrollment", 400)
				return
			}
			start, e := time.Parse("20060102150405", req.StartDate)
			if e != nil || !start.After(time.Now()) {
				http.Error(w, "invalid future anchor", 400)
				return
			}
			acct.Creates++
			id := fmt.Sprint(8000 + acct.Creates)
			sub := nmi.V5Subscription{Object: "subscription", ID: id, StartDate: start.Format(time.RFC3339), NextBillingDate: start.Format(time.RFC3339), Amount: acct.Plan.PlanAmount, CustomerVaultID: req.Vault.ID, DelayedCondition: "active", PausedSubscription: 1, Plan: &acct.Plan}
			acct.Subs[id] = sub
			if hook := g.AfterCreate; hook != nil {
				g.mu.Unlock()
				hook()
				g.mu.Lock()
			}
			if acct.Mode == "source_dark" {
				g.Accounts[strings.Replace(r.Header.Get("Authorization"), "target-", "source-", 1)].Mode = "dark"
			}
			if acct.Mode == "source_external_cancel" {
				source := g.Accounts[strings.Replace(r.Header.Get("Authorization"), "target-", "source-", 1)]
				for id, sub := range source.Subs {
					sub.DelayedCondition = "inactive"
					source.Subs[id] = sub
				}
			}
			if acct.Mode == "lost_create" {
				w.WriteHeader(502)
				return
			}
			if acct.Mode == "wrong_receipt" {
				sub.CustomerVaultID = "wrong-vault"
				acct.Subs[id] = sub
			}
			_ = json.NewEncoder(w).Encode(sub)
		case strings.HasPrefix(path, "/subscriptions/"):
			id := strings.TrimPrefix(path, "/subscriptions/")
			sub, ok := acct.Subs[id]
			if !ok {
				w.WriteHeader(404)
				return
			}
			switch r.Method {
			case http.MethodGet:
				if acct.Mode == "wrong_tombstone" && sub.DelayedCondition == "inactive" {
					sub.ID = "different-subscription"
				}
				if acct.Mode == "wrong_tombstone_vault" && sub.DelayedCondition == "inactive" {
					sub.CustomerVaultID = "different-vault"
				}
				if hook := g.AfterSubscriptionRead; hook != nil {
					g.mu.Unlock()
					hook(acct.Source, sub)
					g.mu.Lock()
				}
				_ = json.NewEncoder(w).Encode(sub)
			case http.MethodDelete:
				if !acct.Source && sub.PausedSubscription != 1 && sub.PausedSubscription != true {
					http.Error(w, "active target cancellation is forbidden", 400)
					return
				}
				acct.Deletes++
				sub.DelayedCondition = "inactive"
				acct.Subs[id] = sub
				if acct.Mode == "target_cancel_bare404" {
					delete(acct.Subs, id)
				}
				if acct.Mode == "lost_cancel" || acct.Mode == "lost_target_cancel" {
					acct.Mode = ""
					w.WriteHeader(502)
					return
				}
				_ = json.NewEncoder(w).Encode(sub)
			case http.MethodPut:
				var req struct {
					Paused    bool   `json:"paused_subscription"`
					StartDate string `json:"start_date"`
				}
				dec := json.NewDecoder(r.Body)
				dec.DisallowUnknownFields()
				if e := dec.Decode(&req); e != nil || req.Paused || acct.Source {
					http.Error(w, "invalid activation", 400)
					return
				}
				start, e := time.Parse("20060102150405", req.StartDate)
				if e != nil || !start.After(time.Now()) {
					http.Error(w, "elapsed anchor", 400)
					return
				}
				if acct.Mode == "source_external_cancel" {
					w.WriteHeader(502)
					return
				}
				acct.Activations++
				sub.PausedSubscription = 0
				sub.NextBillingDate = start.Format(time.RFC3339)
				acct.Subs[id] = sub
				if acct.Mode == "lost_activation" {
					acct.Mode = ""
					w.WriteHeader(502)
					return
				}
				_ = json.NewEncoder(w).Encode(sub)
			default:
				http.Error(w, "unsupported subscription verb", 405)
			}
		default:
			http.Error(w, "unexpected endpoint; no initial sale allowed", 400)
		}
	}))
	t.Cleanup(g.Server.Close)
	return g
}

type cutoverHTTPFixture struct {
	Sub, Source, Target, Customer, OldMethod, NewMethod, Price uuid.UUID
	Anchor, Start                                              time.Time
	SourceKey, TargetKey                                       string
	SourceDeletes                                              int
	Req                                                        openrails.ProviderCutoverRequest
}

func seedCutoverHTTP(t *testing.T, h *Harness, s *Surface, g *cutoverGateway, mode string, merchantIDs ...merchant.ID) cutoverHTTPFixture {
	t.Helper()
	ctx := context.Background()
	rt := s.App().Runtime
	merchantID := dbtest.TestMerchantID
	if len(merchantIDs) > 0 {
		merchantID = merchantIDs[0]
	}
	suffix := uuid.NewString()
	sourceKey := "source-" + suffix
	targetKey := "target-" + suffix
	set := config.PSPSet{sourceKey: {Rail: models.RailNMI, AccountID: sourceKey, Archived: true, NMI: &config.NMIRailConfig{SecurityKey: sourceKey}}, targetKey: {Rail: models.RailNMI, AccountID: targetKey, NMI: &config.NMIRailConfig{SecurityKey: targetKey}}}
	SeedPSPs(ctx, t, rt, merchantID, set)
	source, _, _, _ := merchants.PSPNaturalKey("nmi", "test", sourceKey)
	target, _, _, _ := merchants.PSPNaturalKey("nmi", "test", targetKey)
	for _, pspID := range []uuid.UUID{source, target} {
		require.NoError(t, operator.SetProviderCutoverQualification(ctx, s.App(), merchantID, pspID, &providerqualification.Record{
			PSPID: pspID, Environment: "test", Contract: providerqualification.NMIContract, EvidenceRef: "loopback-fixture-only",
		}))
	}
	p := cutoverHTTPFixture{Sub: uuid.New(), Source: source, Target: target, Customer: uuid.New(), OldMethod: uuid.New(), NewMethod: uuid.New(), Price: uuid.New(), Start: time.Now().UTC().Truncate(time.Second).Add(-time.Hour), Anchor: time.Now().UTC().Truncate(time.Second).Add(7 * 24 * time.Hour), SourceKey: sourceKey, TargetKey: targetKey}
	p.SourceDeletes = 1
	if mode == "source_external_cancel" {
		p.SourceDeletes = 0
	}
	p.Req = openrails.ProviderCutoverRequest{TargetPaymentMethodID: openrails.PaymentMethodID(p.NewMethod), ExpectedSourcePSPID: source, ExpectedTargetPSPID: target}
	pool := h.Pool()
	mid := merchantID.UUID()
	product := uuid.New()
	oldVault := "old-" + suffix
	newVault := "new-" + suffix
	providerSub := "old-sub-" + suffix
	planID := "plan-" + suffix
	exec := func(q string, args ...any) { t.Helper(); _, e := pool.Exec(ctx, q, args...); require.NoError(t, e) }
	dbtest.EnsureCustomerIDPgxFor(ctx, t, pool, mid, p.Customer.String())
	exec(`INSERT INTO openrails.products(id,key,display_name,merchant_id) VALUES($1,$2,$2,$3)`, product, suffix, mid)
	exec(`INSERT INTO openrails.prices(id,product_id,key,amount,currency,access_duration_hours,auto_renew,merchant_id) VALUES($1,$2,$3,10000000,'USD',720,true,$4)`, p.Price, product, suffix, mid)
	exec(`INSERT INTO openrails.price_psp_bindings(merchant_id,price_id,psp_id,plan_id,configuration) VALUES($1,$2,$3,$4,'{}')`, mid, p.Price, target, planID)
	for _, pm := range []struct {
		ID, PSP uuid.UUID
		Vault   string
	}{{p.OldMethod, source, oldVault}, {p.NewMethod, target, newVault}} {
		exec(`INSERT INTO openrails.payment_methods(id,merchant_id,customer_id,rail,psp_id,rail_customer_ref,rail_method_ref,initial_transaction_id,custodian,rebill_driver) VALUES($1,$2,$3,'nmi',$4,$5,'only-card','','psp','provider')`, pm.ID, mid, p.Customer, pm.PSP, pm.Vault)
	}
	exec(`INSERT INTO openrails.subscriptions(id,merchant_id,customer_id,product_id,price_id,status,started_at,current_period_starts_at,current_period_ends_at,rail,rail_subscription_id,psp_id,payment_method_id) VALUES($1,$2,$3,$4,$5,'active',$6,$6,$7,'nmi',$8,$9,$10)`, p.Sub, mid, p.Customer, product, p.Price, p.Start, p.Anchor, providerSub, source, p.OldMethod)
	plan := nmi.V5Plan{Object: "plan", ID: planID, PlanAmount: "10.00", PlanPayments: "0", DayFrequency: "30", MonthFrequency: "0"}
	sourceAcct := &cutoverAccount{Source: true, Plan: plan, Subs: map[string]nmi.V5Subscription{providerSub: {Object: "subscription", ID: providerSub, CustomerVaultID: oldVault, Amount: "10.00", Plan: &plan, PausedSubscription: 0, DelayedCondition: "active", NextBillingDate: p.Anchor.Format(time.RFC3339)}}}
	targetAcct := &cutoverAccount{Plan: plan, Subs: map[string]nmi.V5Subscription{}}
	if mode == "lost_cancel" || strings.HasPrefix(mode, "wrong_tombstone") {
		sourceAcct.Mode = mode
	} else {
		targetAcct.Mode = mode
	}
	g.mu.Lock()
	g.Accounts[sourceKey] = sourceAcct
	g.Accounts[targetKey] = targetAcct
	g.mu.Unlock()
	return p
}
func assertCutoverCommitted(t *testing.T, h *Harness, g *cutoverGateway, p cutoverHTTPFixture, result *openrails.ProviderCutover) {
	t.Helper()
	require.Equal(t, "succeeded", result.Status)
	require.Equal(t, "completed", result.Stage)
	require.Equal(t, openrails.SubscriptionID(p.Sub), result.SubscriptionID)
	require.NotEmpty(t, result.TargetSubscriptionID)
	var account, method, customer, price uuid.UUID
	var providerID string
	var start, end time.Time
	require.NoError(t, h.Pool().QueryRow(context.Background(), `SELECT psp_id,payment_method_id,customer_id,price_id,rail_subscription_id,current_period_starts_at,current_period_ends_at FROM openrails.subscriptions WHERE id=$1`, p.Sub).Scan(&account, &method, &customer, &price, &providerID, &start, &end))
	require.Equal(t, p.Target, account)
	require.Equal(t, p.NewMethod, method)
	require.Equal(t, p.Customer, customer)
	require.Equal(t, p.Price, price)
	require.Equal(t, result.TargetSubscriptionID, providerID)
	require.True(t, p.Start.Equal(start))
	require.True(t, p.Anchor.Equal(end))
	var payload, evidence string
	require.NoError(t, h.Pool().QueryRow(context.Background(), `SELECT payload::text,result_evidence::text FROM openrails.rail_intents WHERE id=$1`, result.ID).Scan(&payload, &evidence))
	profile := contract.Profile{Name: "rail_intents", Columns: []contract.Column{{Name: "intent_type", Type: "text"}, {Name: "status", Type: "text"}, {Name: "payload", Type: "jsonb"}, {Name: "result_evidence", Type: "jsonb"}}}
	typ, status := intents.TypeNMIProviderCutover, "succeeded"
	require.NoError(t, contract.ValidateValues(profile, []*string{&typ, &status, &payload, &evidence}), "real cutover receipts must survive merchant archive")
	g.mu.Lock()
	defer g.mu.Unlock()
	source := g.Accounts[p.SourceKey]
	target := g.Accounts[p.TargetKey]
	require.Equal(t, 1, target.Creates)
	require.Equal(t, p.SourceDeletes, source.Deletes)
	require.Equal(t, 1, target.Activations)
	for _, sub := range source.Subs {
		require.Equal(t, "inactive", sub.DelayedCondition)
	}
	require.Equal(t, 0, target.Subs[result.TargetSubscriptionID].PausedSubscription)
}
func TestNMIProviderCutoverHTTP(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	g := newCutoverGateway(t)
	s := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeFull }))
	_, err := h.Pool().Exec(ctx, `UPDATE openrails.destructive_action_switch SET enabled=true`)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = h.Pool().Exec(ctx, `UPDATE openrails.destructive_action_switch SET enabled=false`) })
	builder, ok := s.App().Runtime.CollectionResolver.(*money.MerchantCollectionAdapterBuilder)
	require.True(t, ok)
	builder.Endpoints.NMIV5BaseURL = g.Server.URL
	owner := s.ProvisionOwnedMerchant("cutover-http-" + uuid.NewString())
	client := s.Client(openrails.WithAPIKey(owner.APIKey), openrails.WithMerchantID(owner.MerchantID))
	seed := func(t *testing.T, mode string) cutoverHTTPFixture {
		return seedCutoverHTTP(t, h, s, g, mode, owner.MerchantID)
	}
	for _, mode := range []string{"success", "lost_cancel", "lost_activation", "source_dark", "lost_create", "wrong_receipt", "wrong_tombstone", "wrong_tombstone_vault"} {
		t.Run(mode, func(t *testing.T) {
			p := seed(t, mode)
			key := uuid.NewString()
			preview, e := client.PreviewProviderCutover(ctx, openrails.SubscriptionID(p.Sub), p.Req)
			require.NoError(t, e)
			require.Equal(t, "ready", preview.Status)
			result, e := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, e)
			if mode == "lost_create" || mode == "wrong_receipt" || strings.HasPrefix(mode, "wrong_tombstone") {
				require.Equal(t, "unknown_needs_verify", result.Status)
				for i := 0; i < 2; i++ {
					result, e = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
					require.NoError(t, e)
					require.Equal(t, "unknown_needs_verify", result.Status)
				}
				g.mu.Lock()
				require.Equal(t, 1, g.Accounts[p.TargetKey].Creates)
				require.Equal(t, 0, g.Accounts[p.TargetKey].Activations)
				if !strings.HasPrefix(mode, "wrong_tombstone") {
					require.Equal(t, 0, g.Accounts[p.SourceKey].Deletes)
				}
				g.mu.Unlock()
				var actual uuid.UUID
				require.NoError(t, h.Pool().QueryRow(ctx, `SELECT psp_id FROM openrails.subscriptions WHERE id=$1`, p.Sub).Scan(&actual))
				require.Equal(t, p.Source, actual)
				_, e = h.Pool().Exec(ctx, `UPDATE openrails.subscriptions SET status='cancelled' WHERE id=$1`, p.Sub)
				require.Error(t, e, "unresolved cutover fences lifecycle")
				_, e = h.Pool().Exec(ctx, `UPDATE openrails.payment_methods SET rail_customer_ref='changed' WHERE id=$1`, p.NewMethod)
				require.Error(t, e, "unresolved cutover fences card remap")
			} else {
				if mode != "success" {
					require.Equal(t, "unknown_needs_verify", result.Status)
				}
				if mode == "source_dark" {
					g.mu.Lock()
					require.Equal(t, 0, g.Accounts[p.SourceKey].Deletes)
					require.Equal(t, 0, g.Accounts[p.TargetKey].Activations)
					g.Accounts[p.SourceKey].Mode = ""
					g.mu.Unlock()
				}
				result, e = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, e)
				assertCutoverCommitted(t, h, g, p, result)
				again, e := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, e)
				require.Equal(t, result, again)
			}
			changed := p.Req
			changed.ExpectedSourcePSPID = uuid.New()
			_, e = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, changed)
			require.Error(t, e)
			got, e := client.GetProviderCutover(ctx, openrails.SubscriptionID(p.Sub), key)
			require.NoError(t, e)
			require.Equal(t, result.ID, got.ID)
		})
	}
	t.Run("operator_resolves_lost_create", func(t *testing.T) {
		p := seed(t, "lost_create")
		key := uuid.NewString()
		result, e := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, e)
		require.Equal(t, "unknown_needs_verify", result.Status)
		rt := s.App().Runtime
		e = rt.DB.RunInMerchantConn(merchant.WithID(ctx, owner.MerchantID), func(cctx context.Context) error {
			_, err := rt.IntentRunner().Resolve(cctx, result.ID, intents.Resolution{Step: "target", ProviderReference: "8001", Actor: "fixture-operator", Reason: "provider support supplied exact enrollment receipt"})
			return err
		})
		require.NoError(t, e)
		result, e = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, e)
		assertCutoverCommitted(t, h, g, p, result)
	})
	t.Run("concurrent_replay", func(t *testing.T) {
		p := seed(t, "success")
		key := uuid.NewString()
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, e := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				errs <- e
			}()
		}
		wg.Wait()
		close(errs)
		for e := range errs {
			require.NoError(t, e)
		}
		result, e := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, e)
		assertCutoverCommitted(t, h, g, p, result)
	})
	t.Run("customer_ownership", func(t *testing.T) {
		p := seed(t, "success")
		issuer := s.RegisterDelegatedIssuer("cutover-"+uuid.NewString(), owner.MerchantSlug)
		token := issuer.Mint(p.Customer.String(), "owner@example.com", "owner", nil)
		path := s.BaseURL + "/v1/me/subscriptions/" + openrails.SubscriptionID(p.Sub).String() + "/provider-cutover/preview"
		status, body := requestJSON(t, http.MethodPost, path, token, p.Req)
		require.Equal(t, http.StatusOK, status, string(body))
		other := issuer.Mint(uuid.NewString(), "other@example.com", "other", nil)
		status, body = requestJSON(t, http.MethodPost, path, other, p.Req)
		require.Equal(t, http.StatusNotFound, status, string(body))
		bad := p.Req
		bad.ExpectedTargetPSPID = uuid.Nil
		_, e := client.PreviewProviderCutover(ctx, openrails.SubscriptionID(p.Sub), bad)
		require.Error(t, e)
	})
	t.Run("host_campaign", func(t *testing.T) {
		p := seed(t, "lost_cancel")
		runHostProviderCampaign(t, s.BaseURL, owner.APIKey, owner.MerchantID.UUID(), p.Source, p.Target, []hostCampaignMember{{SubscriptionID: openrails.SubscriptionID(p.Sub).String(), TargetPaymentMethodID: openrails.PaymentMethodID(p.NewMethod).String()}}, 1, nil)
		// A real host script writes exactly one intent with the frozen request.
		var key string
		require.NoError(t, h.Pool().QueryRow(ctx, `SELECT idempotency_key FROM openrails.rail_intents WHERE subscription_id=$1 AND intent_type=$2`, p.Sub, intents.TypeNMIProviderCutover).Scan(&key))
		result, e := client.GetProviderCutover(ctx, openrails.SubscriptionID(p.Sub), strings.TrimPrefix(key, intents.TypeNMIProviderCutover+":"))
		require.NoError(t, e)
		assertCutoverCommitted(t, h, g, p, result)
	})
}
