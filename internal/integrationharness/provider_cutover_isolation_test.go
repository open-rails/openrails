//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/testauth"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func cutoverHTTPRequest(t *testing.T, method, url, token, key string, body any) (int, []byte) {
	t.Helper()
	var data bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&data).Encode(body))
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, &data)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	require.NoError(t, testauth.Authorize(req, token))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, raw
}

func requireCutoverError(t *testing.T, raw []byte, code string) {
	t.Helper()
	var response struct {
		Error openrails.ErrorDetails `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.Equal(t, code, response.Error.Code, string(raw))
	require.NotEmpty(t, response.Error.Message)
	require.NotEmpty(t, response.Error.RequestID)
}

func TestNMIProviderCutoverIsolation(t *testing.T) {
	ctx := t.Context()
	h := New(t, ctx)
	g := newCutoverGateway(t)
	s := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeFull }))
	_, err := h.Pool().Exec(ctx, `UPDATE billing.destructive_action_switch SET enabled=true`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.WithoutCancel(ctx), `UPDATE billing.destructive_action_switch SET enabled=false`)
		require.NoError(t, err, "restore default destructive action switch")
	})
	s.App().Runtime.CollectionResolver.(*money.MerchantCollectionAdapterBuilder).Endpoints.NMIV5BaseURL = g.Server.URL
	owner := s.ProvisionOwnedMerchant("cutover-isolation-" + uuid.NewString())
	client := s.Client(openrails.WithAPIKey(owner.APIKey), openrails.WithMerchantID(owner.MerchantID))
	seed := func(t *testing.T, mode string) cutoverHTTPFixture {
		return seedCutoverHTTP(t, h, s, g, mode, owner.MerchantID)
	}

	t.Run("owner_executes_and_replays_public_ids", func(t *testing.T) {
		p := seed(t, "success")
		issuer := s.RegisterDelegatedIssuer("cutover-owner-"+uuid.NewString(), owner.MerchantSlug)
		token := issuer.Mint(p.Customer.String(), "owner@example.com", "owner", nil)
		other := issuer.Mint(uuid.NewString(), "other@example.com", "other", nil)
		// Feed IDs obtained from the real self-service responses into the new
		// operation; this detects wire drift hidden by formatting fixture UUIDs.
		status, raw := requestJSON(t, http.MethodGet, s.BaseURL+"/v1/me/subscriptions", token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var subscriptions struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &subscriptions))
		require.Len(t, subscriptions.Data, 1, string(raw))
		subID := subscriptions.Data[0].ID
		parsedSub, err := openrails.ParseSubscriptionID(subID)
		require.NoError(t, err)
		require.Equal(t, p.Sub, parsedSub.UUID())
		status, raw = requestJSON(t, http.MethodGet, s.BaseURL+"/v1/me/payment-methods", token, nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		var methods struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		require.NoError(t, json.Unmarshal(raw, &methods))
		var methodID string
		for _, pm := range methods.Data {
			parsedMethod, err := openrails.ParsePaymentMethodID(pm.ID)
			require.NoError(t, err)
			if parsedMethod.UUID() == p.NewMethod {
				methodID = pm.ID
			}
		}
		require.NotEmpty(t, methodID, string(raw))
		p.Req.TargetPaymentMethodID, err = openrails.ParsePaymentMethodID(methodID)
		require.NoError(t, err)
		path := s.BaseURL + "/v1/me/subscriptions/" + subID + "/provider-cutover"
		key := uuid.NewString()
		status, raw = cutoverHTTPRequest(t, http.MethodPost, path, other, key, p.Req)
		require.Equal(t, http.StatusNotFound, status, string(raw))
		requireCutoverError(t, raw, "resource_missing")
		status, raw = cutoverHTTPRequest(t, http.MethodPost, path, token, key, p.Req)
		require.Equal(t, http.StatusOK, status, string(raw))
		var result openrails.ProviderCutover
		require.NoError(t, json.Unmarshal(raw, &result))
		assertCutoverCommitted(t, h, g, p, &result)
		// Only the declared pm_ spelling is accepted; the pre-v1 raw UUID
		// spelling does not create a second compatibility path.
		status, raw = cutoverHTTPRequest(t, http.MethodPost, path, token, key, map[string]any{
			"target_payment_method_id": p.NewMethod.String(),
			"expected_source_psp_id":   p.Source, "expected_target_psp_id": p.Target,
		})
		require.Equal(t, http.StatusBadRequest, status, string(raw))
		status, raw = cutoverHTTPRequest(t, http.MethodPost, path, token, key, p.Req)
		require.Equal(t, http.StatusOK, status, string(raw))
		var replay openrails.ProviderCutover
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, result, replay)
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			url := path
			if method == http.MethodGet {
				url += "?idempotency_key=" + key
			}
			status, raw = cutoverHTTPRequest(t, method, url, other, key, p.Req)
			require.Equal(t, http.StatusNotFound, status, string(raw))
			requireCutoverError(t, raw, "resource_missing")
		}
		status, raw = cutoverHTTPRequest(t, http.MethodGet, path+"?idempotency_key="+key, token, "", nil)
		require.Equal(t, http.StatusOK, status, string(raw))
		require.NoError(t, json.Unmarshal(raw, &replay))
		require.Equal(t, result, replay)
		assertCutoverCommitted(t, h, g, p, &replay)
	})

	t.Run("merchant_scope_applies_to_execution_and_replay", func(t *testing.T) {
		p := seed(t, "success")
		other := s.ProvisionOwnedMerchant("cutover-other-" + uuid.NewString())
		otherClient := s.Client(openrails.WithAPIKey(other.APIKey), openrails.WithMerchantID(other.MerchantID))
		key := uuid.NewString()
		_, err := otherClient.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		assertCutoverCommitted(t, h, g, p, result)
		_, err = otherClient.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		_, err = otherClient.GetProviderCutover(ctx, openrails.SubscriptionID(p.Sub), key)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		var count int
		require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE merchant_id=$1 AND intent_type=$2`, other.MerchantID, intents.TypeNMIProviderCutover).Scan(&count))
		require.Zero(t, count)
	})

	t.Run("different_keys_share_one_subscription_fence", func(t *testing.T) {
		p := seed(t, "lost_create")
		keys := []string{uuid.NewString(), uuid.NewString()}
		var wg sync.WaitGroup
		errs := make([]error, len(keys))
		for i, key := range keys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, errs[i] = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			}()
		}
		wg.Wait()
		require.NotEqual(t, errs[0] == nil, errs[1] == nil, "one operation must own the subscription: %v", errs)
		var count int
		require.NoError(t, h.Pool().QueryRow(ctx, `SELECT count(*) FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type=$2`, p.Sub, intents.TypeNMIProviderCutover).Scan(&count))
		require.Equal(t, 1, count)
		g.mu.Lock()
		require.Equal(t, 1, g.Accounts[p.TargetKey].Creates)
		require.Zero(t, g.Accounts[p.TargetKey].Activations)
		g.mu.Unlock()
		for _, id := range []uuid.UUID{p.OldMethod, p.NewMethod} {
			_, err := h.Pool().Exec(ctx, `UPDATE billing.payment_methods SET rail_customer_ref='changed' WHERE id=$1`, id)
			require.Error(t, err)
			_, err = h.Pool().Exec(ctx, `DELETE FROM billing.payment_methods WHERE id=$1`, id)
			require.Error(t, err)
			for _, typ := range []string{"nmi_vault_delete", "nmi_payment_method_update"} {
				_, err = h.Pool().Exec(ctx, `INSERT INTO billing.rail_intents(id,merchant_id,rail,intent_type,psp_id,payload,idempotency_key) VALUES($1,$2,'nmi',$3,$4,jsonb_build_object('payment_method_id',$5::text),$6)`, uuid.New(), owner.MerchantID.UUID(), typ, p.Source, id.String(), uuid.NewString())
				require.ErrorContains(t, err, "unresolved provider cutover")
			}
		}
		_, err := h.Pool().Exec(ctx, `DELETE FROM billing.subscriptions WHERE id=$1`, p.Sub)
		require.ErrorContains(t, err, "unresolved provider cutover")
		_, err = h.Pool().Exec(ctx, `UPDATE billing.subscriptions SET current_period_ends_at=current_period_ends_at + interval '1 day' WHERE id=$1`, p.Sub)
		require.ErrorContains(t, err, "unresolved provider cutover")
	})

	t.Run("missing_billing_id_is_ineligible", func(t *testing.T) {
		p := seed(t, "success")
		_, err := h.Pool().Exec(ctx, `UPDATE billing.payment_methods SET rail_method_ref='' WHERE id=$1`, p.NewMethod)
		require.NoError(t, err)
		path := s.BaseURL + "/v1/merchant/subscriptions/" + openrails.SubscriptionID(p.Sub).String() + "/provider-cutover"
		status, raw := cutoverHTTPRequest(t, http.MethodPost, path, owner.APIKey, uuid.NewString(), p.Req)
		require.Equal(t, http.StatusConflict, status, string(raw))
		requireCutoverError(t, raw, "provider_cutover_conflict")
		g.mu.Lock()
		require.Zero(t, g.Accounts[p.TargetKey].Creates)
		g.mu.Unlock()
	})

	for _, drift := range []string{"amount", "date", "cadence"} {
		t.Run("source_drift_"+drift, func(t *testing.T) {
			p := seed(t, "success")
			g.mu.Lock()
			for id, sub := range g.Accounts[p.SourceKey].Subs {
				switch drift {
				case "amount":
					sub.Amount = "12.00"
				case "date":
					sub.NextBillingDate = p.Anchor.Add(time.Hour).Format(time.RFC3339)
				case "cadence":
					plan := *sub.Plan
					plan.DayFrequency = "31"
					sub.Plan = &plan
				}
				g.Accounts[p.SourceKey].Subs[id] = sub
			}
			g.mu.Unlock()
			result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), uuid.NewString(), p.Req)
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", result.Status)
			g.mu.Lock()
			require.Zero(t, g.Accounts[p.TargetKey].Creates)
			require.Zero(t, g.Accounts[p.SourceKey].Deletes)
			g.mu.Unlock()
		})
	}

	for _, mode := range []string{"lost_cancel", "source_external_cancel"} {
		t.Run("operator_recovers_elapsed_anchor_"+mode, func(t *testing.T) {
			p := seed(t, mode)
			key := uuid.NewString()
			result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", result.Status)
			rt := s.App().Runtime
			originalClock := rt.ProviderCutovers.Clock
			originalRuntimeClock := rt.Clock
			fake := clockwork.NewFakeClockAt(p.Anchor.Add(time.Hour))
			rt.ProviderCutovers.Clock = fake
			rt.Clock = fake
			defer func() { rt.ProviderCutovers.Clock = originalClock; rt.Clock = originalRuntimeClock }()
			result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", result.Status)
			require.Contains(t, result.Reason, "anchor elapsed")
			anchor := fake.Now().Add(24 * time.Hour).UTC()
			resolution := intents.Resolution{Step: "anchor", BillingAnchor: anchor, Actor: "fixture-operator", Reason: "merchant authorizes delayed first charge after outage; support ticket 657"}
			resolve := func(r intents.Resolution) error {
				return rt.DB.RunInMerchantConn(merchant.WithID(ctx, owner.MerchantID), func(cctx context.Context) error {
					_, err := rt.IntentRunner().Resolve(cctx, result.ID, r)
					return err
				})
			}
			bad := resolution
			bad.ProviderReference = "8001"
			require.ErrorIs(t, resolve(bad), intents.ErrResolutionInvalid)
			bad = resolution
			bad.Step = "target"
			require.ErrorIs(t, resolve(bad), intents.ErrResolutionUnsupported)
			bad = resolution
			bad.BillingAnchor = fake.Now().Add(-time.Minute)
			require.ErrorIs(t, resolve(bad), intents.ErrResolutionRejected)
			g.mu.Lock()
			g.Accounts[p.SourceKey].Mode = "dark"
			g.mu.Unlock()
			require.ErrorIs(t, resolve(resolution), intents.ErrResolutionRejected, "dark source is never proof")
			g.mu.Lock()
			g.Accounts[p.SourceKey].Mode = ""
			target := g.Accounts[p.TargetKey].Subs["8001"]
			target.PausedSubscription = 0
			g.Accounts[p.TargetKey].Subs["8001"] = target
			g.mu.Unlock()
			require.ErrorIs(t, resolve(resolution), intents.ErrResolutionRejected, "active target cannot be retimed")
			g.mu.Lock()
			target.PausedSubscription = 1
			g.Accounts[p.TargetKey].Subs["8001"] = target
			g.mu.Unlock()
			require.NoError(t, resolve(resolution))
			g.mu.Lock()
			g.Accounts[p.TargetKey].Mode = ""
			g.mu.Unlock()
			require.NoError(t, resolve(resolution), "identical authorization is idempotent")
			var evidence struct {
				AnchorResolutions []map[string]any `json:"anchor_resolutions"`
			}
			var payloadBefore, retained []byte
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT payload,result_evidence FROM billing.rail_intents WHERE id=$1`, result.ID).Scan(&payloadBefore, &retained))
			require.NoError(t, json.Unmarshal(retained, &evidence))
			require.Len(t, evidence.AnchorResolutions, 1)
			g.mu.Lock()
			require.Equal(t, 1, g.Accounts[p.TargetKey].Creates)
			require.Equal(t, p.SourceDeletes, g.Accounts[p.SourceKey].Deletes)
			require.Zero(t, g.Accounts[p.TargetKey].Activations, "resolution only records authorization")
			g.mu.Unlock()
			result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			assertCutoverCommitted(t, h, g, p, result)
			require.True(t, result.Anchor.Equal(anchor))
			var payloadAfter []byte
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT payload FROM billing.rail_intents WHERE id=$1`, result.ID).Scan(&payloadAfter))
			require.JSONEq(t, string(payloadBefore), string(payloadAfter))
			require.NoError(t, resolve(resolution), "completed authorization replays without retiming")
			g.mu.Lock()
			require.Equal(t, anchor.Format(time.RFC3339), g.Accounts[p.TargetKey].Subs["8001"].NextBillingDate)
			require.Equal(t, 1, g.Accounts[p.TargetKey].Activations)
			g.mu.Unlock()
		})

	}
	t.Run("lost_activation_finishes_after_anchor_elapsed", func(t *testing.T) {
		p := seed(t, "lost_activation")
		key := uuid.NewString()
		result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "unknown_needs_verify", result.Status)
		rt := s.App().Runtime
		oldClock, oldAdmissionClock := rt.Clock, rt.ProviderCutovers.Clock
		fake := clockwork.NewFakeClockAt(p.Anchor.Add(time.Hour))
		rt.Clock, rt.ProviderCutovers.Clock = fake, fake
		defer func() { rt.Clock, rt.ProviderCutovers.Clock = oldClock, oldAdmissionClock }()
		result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		assertCutoverCommitted(t, h, g, p, result)
	})

	t.Run("unsent_expired_anchor_releases_fences", func(t *testing.T) {
		p := seed(t, "success")
		rt := s.App().Runtime
		oldMode, oldClock, oldAdmissionClock := rt.Config.ProviderWriteMode, rt.Clock, rt.ProviderCutovers.Clock
		defer func() {
			rt.Config.ProviderWriteMode, rt.Clock, rt.ProviderCutovers.Clock = oldMode, oldClock, oldAdmissionClock
		}()
		rt.Config.ProviderWriteMode = config.ProviderWriteModeReadOnly
		key := uuid.NewString()
		result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Contains(t, []string{"pending", "failed_retryable"}, result.Status)
		fake := clockwork.NewFakeClockAt(p.Anchor.Add(time.Hour))
		rt.Clock, rt.ProviderCutovers.Clock = fake, fake
		rt.Config.ProviderWriteMode = config.ProviderWriteModeFull
		result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "failed_terminal", result.Status)
		require.Equal(t, "not_executed", result.Stage)
		_, err = h.Pool().Exec(ctx, `UPDATE billing.subscriptions SET current_period_ends_at=current_period_ends_at + interval '30 days' WHERE id=$1`, p.Sub)
		require.NoError(t, err, "unsent expiry must release the renewal fence")
		replay, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, result, replay)
		g.mu.Lock()
		require.Zero(t, g.Accounts[p.SourceKey].Requests)
		require.Zero(t, g.Accounts[p.TargetKey].Requests)
		g.mu.Unlock()
	})

	t.Run("rotated_source_key_cannot_turn_foreign_404_into_cancellation", func(t *testing.T) {
		p := seed(t, "source_dark")
		key := uuid.NewString()
		result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "unknown_needs_verify", result.Status)
		require.NotEmpty(t, result.TargetSubscriptionID)
		wrongKey := "foreign-" + uuid.NewString()
		g.mu.Lock()
		g.Accounts[wrongKey] = &cutoverAccount{Source: true, Subs: map[string]nmi.V5Subscription{}}
		g.mu.Unlock()
		name, err := merchants.PSPSecretName("nmi", "test", p.SourceKey, "security_key")
		require.NoError(t, err)
		store := s.App().Runtime.Merchants.Secrets()
		_, err = store.Put(ctx, owner.MerchantID, name, wrongKey)
		require.NoError(t, err)
		result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "unknown_needs_verify", result.Status)
		g.mu.Lock()
		require.Zero(t, g.Accounts[wrongKey].Requests, "foreign account must not be consulted for cancellation proof")
		require.Zero(t, g.Accounts[p.SourceKey].Deletes)
		require.Zero(t, g.Accounts[p.TargetKey].Activations)
		g.Accounts[p.SourceKey].Mode = ""
		g.mu.Unlock()
		_, err = store.Put(ctx, owner.MerchantID, name, p.SourceKey)
		require.NoError(t, err)
		result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		assertCutoverCommitted(t, h, g, p, result)
	})
}
