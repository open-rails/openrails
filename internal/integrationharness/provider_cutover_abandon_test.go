//go:build integration

package integrationharness

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive/contract"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestNMIProviderCutoverAbandon(t *testing.T) {
	ctx := t.Context()
	h := New(t, ctx)
	g := newCutoverGateway(t)
	s := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeFull }))
	var prior bool
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT enabled FROM billing.destructive_action_switch`).Scan(&prior))
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.WithoutCancel(ctx), `UPDATE billing.destructive_action_switch SET enabled=$1`, prior)
		require.NoError(t, err)
	})
	_, err := h.Pool().Exec(ctx, `UPDATE billing.destructive_action_switch SET enabled=true`)
	require.NoError(t, err)
	rt := s.App().Runtime
	rt.CollectionResolver.(*money.MerchantCollectionAdapterBuilder).Endpoints.NMIV5BaseURL = g.Server.URL
	owner := s.ProvisionOwnedMerchant("cutover-abandon-" + uuid.NewString())
	client := s.Client(openrails.WithAPIKey(owner.APIKey), openrails.WithMerchantID(owner.MerchantID))
	resolve := func(id uuid.UUID) error {
		return rt.DB.RunInMerchantConn(merchant.WithID(ctx, owner.MerchantID), func(cctx context.Context) error {
			_, err := rt.IntentRunner().Resolve(cctx, id, intents.Resolution{Step: "target", Abandon: true, Actor: "fixture-operator", Reason: "retain source after interrupted cutover"})
			return err
		})
	}
	start := func(t *testing.T, mode string) (cutoverHTTPFixture, string, *openrails.ProviderCutover) {
		t.Helper()
		p := seedCutoverHTTP(t, h, s, g, mode, owner.MerchantID)
		key := uuid.NewString()
		result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
		require.NoError(t, err)
		require.Equal(t, "unknown_needs_verify", result.Status)
		g.mu.Lock()
		g.Accounts[p.SourceKey].Mode = ""
		g.mu.Unlock()
		return p, key, result
	}
	for _, mode := range []string{"source_drift", "expired_anchor", "lost_target_cancel", "target_cancel_bare404", "target_changed", "target_active", "source_cancel_submitted", "missing_target_receipt", "source_paused"} {
		t.Run(mode, func(t *testing.T) {
			initial := "source_dark"
			if mode == "source_cancel_submitted" {
				initial = "lost_cancel"
			} else if mode == "missing_target_receipt" {
				initial = "lost_create"
			}
			p, key, result := start(t, initial)
			g.mu.Lock()
			for id, sub := range g.Accounts[p.SourceKey].Subs {
				if mode == "source_paused" {
					sub.PausedSubscription = 1
					g.Accounts[p.SourceKey].Subs[id] = sub
				}
				if mode == "source_drift" {
					plan := *sub.Plan
					plan.PlanAmount, sub.Amount = "12.00", "12.00"
					sub.Plan = &plan
					g.Accounts[p.SourceKey].Subs[id] = sub
				}
			}
			target := g.Accounts[p.TargetKey].Subs[result.TargetSubscriptionID]
			if mode == "target_active" {
				target.PausedSubscription = 0
				g.Accounts[p.TargetKey].Subs[result.TargetSubscriptionID] = target
			}
			g.mu.Unlock()
			if mode == "expired_anchor" {
				priorRuntime, priorCutover := rt.Clock, rt.ProviderCutovers.Clock
				t.Cleanup(func() { rt.Clock, rt.ProviderCutovers.Clock = priorRuntime, priorCutover })
				fake := clockwork.NewFakeClockAt(p.Anchor.Add(time.Hour))
				rt.Clock, rt.ProviderCutovers.Clock = fake, fake
			}
			err := resolve(result.ID)
			if mode == "target_active" || mode == "source_cancel_submitted" || mode == "missing_target_receipt" || mode == "source_paused" {
				require.ErrorIs(t, err, intents.ErrResolutionRejected)
				g.mu.Lock()
				targetDeletes := g.Accounts[p.TargetKey].Deletes
				g.mu.Unlock()
				require.Zero(t, targetDeletes)
				return
			}
			require.NoError(t, err)
			require.NoError(t, resolve(result.ID), "approval replay retains the original decision")
			require.NoError(t, rt.DB.RunInMerchantConn(merchant.WithID(ctx, owner.MerchantID), func(cctx context.Context) error {
				row, err := intents.NewStore(rt.DB).Get(cctx, result.ID)
				if err == nil {
					rt.ProviderCutovers.Verify(cctx, row)
				}
				return err
			}))
			g.mu.Lock()
			verifyDeletes := g.Accounts[p.TargetKey].Deletes
			g.mu.Unlock()
			require.Zero(t, verifyDeletes, "verification must not cancel the paused target")
			g.mu.Lock()
			if mode == "target_changed" {
				changed := target
				changed.Amount = "99.00"
				g.Accounts[p.TargetKey].Subs[result.TargetSubscriptionID] = changed
			}
			g.Accounts[p.TargetKey].Mode = mode
			g.mu.Unlock()
			result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			if mode == "lost_target_cancel" || mode == "target_cancel_bare404" || mode == "target_changed" {
				require.Equal(t, "unknown_needs_verify", result.Status)
				_, err := h.Pool().Exec(ctx, `UPDATE billing.subscriptions SET updated_at=now() WHERE id=$1`, p.Sub)
				require.Error(t, err, "uncertain target cancellation must keep the local fence")
				g.mu.Lock()
				if mode == "target_cancel_bare404" {
					target.DelayedCondition = "inactive"
					g.Accounts[p.TargetKey].Subs[result.TargetSubscriptionID] = target
				} else if mode == "target_changed" {
					g.Accounts[p.TargetKey].Subs[result.TargetSubscriptionID] = target
				}
				g.Accounts[p.TargetKey].Mode = ""
				g.mu.Unlock()
				result, err = client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, err)
			}
			require.Equal(t, "failed_terminal", result.Status)
			require.Equal(t, "abandoned", result.Stage)
			var payload, evidence string
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT payload::text,result_evidence::text FROM billing.rail_intents WHERE id=$1`, result.ID).Scan(&payload, &evidence))
			profile := contract.Profile{Name: "rail_intents", Columns: []contract.Column{{Name: "intent_type", Type: "text"}, {Name: "status", Type: "text"}, {Name: "payload", Type: "jsonb"}, {Name: "result_evidence", Type: "jsonb"}}}
			typ := intents.TypeNMIProviderCutover
			require.NoError(t, contract.ValidateValues(profile, []*string{&typ, &result.Status, &payload, &evidence}))
			g.mu.Lock()
			deletes, sourceDeletes, activations := g.Accounts[p.TargetKey].Deletes, g.Accounts[p.SourceKey].Deletes, g.Accounts[p.TargetKey].Activations
			g.mu.Unlock()
			require.Equal(t, 1, deletes)
			require.Zero(t, sourceDeletes)
			require.Zero(t, activations)
			var psp uuid.UUID
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT psp_id FROM billing.subscriptions WHERE id=$1`, p.Sub).Scan(&psp))
			require.Equal(t, p.Source, psp)
			_, err = h.Pool().Exec(ctx, `UPDATE billing.subscriptions SET updated_at=now() WHERE id=$1`, p.Sub)
			require.NoError(t, err, "proven abandonment releases the existing mutation fence")
		})
	}
	for _, first := range []string{"abandon", "complete"} {
		t.Run("direction_cas_"+first, func(t *testing.T) {
			p, key, result := start(t, "source_dark")
			entered, release := make(chan struct{}), make(chan struct{})
			var held atomic.Bool
			g.mu.Lock()
			g.AfterSubscriptionRead = func(source bool, _ nmi.V5Subscription) {
				if source != (first == "abandon") || !held.CompareAndSwap(false, true) {
					return
				}
				close(entered)
				select {
				case <-release:
				case <-t.Context().Done():
				}
			}
			g.mu.Unlock()
			t.Cleanup(func() { g.mu.Lock(); g.AfterSubscriptionRead = nil; g.mu.Unlock() })
			execute := func() error {
				// Deliberately bypass the runner lease to simulate an old executor
				// waking after lease expiry while an operator owns the new lease.
				return rt.DB.RunInMerchantConn(merchant.WithID(ctx, owner.MerchantID), func(cctx context.Context) error {
					row, err := intents.NewStore(rt.DB).Get(cctx, result.ID)
					if err == nil {
						rt.ProviderCutovers.Execute(cctx, row)
					}
					return err
				})
			}
			done := make(chan error, 1)
			if first == "abandon" {
				go func() { done <- execute() }()
			} else {
				go func() { done <- resolve(result.ID) }()
			}
			select {
			case <-entered:
			case err := <-done:
				t.Fatalf("operation finished before contention point: %v", err)
			case <-t.Context().Done():
				t.Fatal("test canceled while waiting for provider read")
			}
			if first == "abandon" {
				require.NoError(t, resolve(result.ID))
			} else {
				require.NoError(t, execute())
			}
			close(release)
			if first == "abandon" {
				require.NoError(t, <-done)
			} else {
				require.ErrorIs(t, <-done, intents.ErrResolutionRejected)
			}
			g.mu.Lock()
			g.AfterSubscriptionRead = nil
			g.mu.Unlock()
			result, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			g.mu.Lock()
			sourceDeletes, targetDeletes, activations := g.Accounts[p.SourceKey].Deletes, g.Accounts[p.TargetKey].Deletes, g.Accounts[p.TargetKey].Activations
			g.mu.Unlock()
			if first == "abandon" {
				require.Equal(t, "abandoned", result.Stage)
				require.Zero(t, sourceDeletes)
				require.Zero(t, activations)
				require.Equal(t, 1, targetDeletes)
			} else {
				require.Equal(t, "succeeded", result.Status)
				require.Equal(t, 1, sourceDeletes)
				require.Equal(t, 1, activations)
				require.Zero(t, targetDeletes)
			}
		})
	}
}
