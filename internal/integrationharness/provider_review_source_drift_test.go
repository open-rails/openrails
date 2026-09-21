//go:build integration

package integrationharness

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

type providerReviewTransport func(*http.Request) (*http.Response, error)

func (f providerReviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProviderCutoverRefusesSourceDriftAfterPausedTarget(t *testing.T) {
	base := http.DefaultTransport
	http.DefaultTransport = providerReviewTransport(func(r *http.Request) (*http.Response, error) {
		host := r.URL.Hostname()
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, fmt.Errorf("external HTTP disabled in provider review")
		}
		return base.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = base })
	ctx := t.Context()
	h := New(t, ctx)
	g := newCutoverGateway(t)
	s := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeFull }))
	var previousEnabled bool
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT enabled FROM billing.destructive_action_switch`).Scan(&previousEnabled))
	t.Cleanup(func() {
		_, err := h.Pool().Exec(context.WithoutCancel(ctx), `UPDATE billing.destructive_action_switch SET enabled=$1`, previousEnabled)
		require.NoError(t, err)
	})
	_, err := h.Pool().Exec(ctx, `UPDATE billing.destructive_action_switch SET enabled=true`)
	require.NoError(t, err)
	s.App().Runtime.CollectionResolver.(*money.MerchantCollectionAdapterBuilder).Endpoints.NMIV5BaseURL = g.Server.URL
	owner := s.ProvisionOwnedMerchant("cutover-drift-review-" + uuid.NewString())
	client := s.Client(openrails.WithAPIKey(owner.APIKey), openrails.WithMerchantID(owner.MerchantID))
	for _, drift := range []string{"amount", "date", "cadence", "unproven_absence", "after_cancel_absence"} {
		t.Run(drift, func(t *testing.T) {
			mode := "source_dark"
			if drift == "after_cancel_absence" {
				mode = "lost_cancel"
			}
			p := seedCutoverHTTP(t, h, s, g, mode, owner.MerchantID)
			key := uuid.NewString()
			first, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", first.Status)
			g.mu.Lock()
			source := g.Accounts[p.SourceKey]
			source.Mode = ""
			initialDeletes := source.Deletes
			original := make(map[string]nmi.V5Subscription, len(source.Subs))
			for id, sub := range source.Subs {
				original[id] = sub
				plan := *sub.Plan
				switch drift {
				case "amount":
					sub.Amount = "12.00"
					plan.PlanAmount = "12.00"
				case "date":
					sub.NextBillingDate = p.Anchor.Add(time.Hour).Format(time.RFC3339)
				case "cadence":
					plan.DayFrequency = "31"
				case "unproven_absence", "after_cancel_absence":
					delete(source.Subs, id)
					continue
				}
				sub.Plan = &plan
				source.Subs[id] = sub
			}
			g.mu.Unlock()
			resumed, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
			require.NoError(t, err)
			require.Equal(t, "unknown_needs_verify", resumed.Status)
			g.mu.Lock()
			deletes, activations := g.Accounts[p.SourceKey].Deletes, g.Accounts[p.TargetKey].Activations
			g.mu.Unlock()
			var actual uuid.UUID
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT psp_id FROM billing.subscriptions WHERE id=$1`, p.Sub).Scan(&actual))
			t.Logf("drift=%s status=%s source_deletes=%d target_activations=%d repointed=%v", drift, resumed.Status, deletes, activations, actual == p.Target)
			require.Equal(t, initialDeletes, deletes, "source with drifted or unproven terms must not be cancelled")
			require.Zero(t, activations, "target must remain paused after source drift")
			require.Equal(t, p.Source, actual, "local subscription must remain on original account")
			if drift == "after_cancel_absence" {
				rt := s.App().Runtime
				previousClock := rt.ProviderCutovers.Clock
				t.Cleanup(func() { rt.ProviderCutovers.Clock = previousClock })
				rt.ProviderCutovers.Clock = clockwork.NewFakeClockAt(p.Anchor.Add(time.Hour))
				err = rt.DB.RunInMerchantConn(merchant.WithID(ctx, owner.MerchantID), func(cctx context.Context) error {
					_, e := rt.IntentRunner().Resolve(cctx, resumed.ID, intents.Resolution{
						Step: "anchor", BillingAnchor: p.Anchor.Add(24 * time.Hour),
						Actor: "fixture-operator", Reason: "test later anchor after unknown cancellation",
					})
					return e
				})
				rt.ProviderCutovers.Clock = previousClock
				require.ErrorIs(t, err, intents.ErrResolutionRejected, "a new billing anchor needs an exact source cancellation receipt")
			}
			g.mu.Lock()
			source.Subs = original
			g.mu.Unlock()
			for range 2 {
				completed, err := client.CutoverProvider(ctx, openrails.SubscriptionID(p.Sub), key, p.Req)
				require.NoError(t, err)
				require.Equal(t, "succeeded", completed.Status)
			}
			g.mu.Lock()
			creates, deletes, activations := g.Accounts[p.TargetKey].Creates, source.Deletes, g.Accounts[p.TargetKey].Activations
			g.mu.Unlock()
			require.Equal(t, 1, creates, "resume must reuse the original paused target")
			require.Equal(t, 1, deletes)
			require.Equal(t, 1, activations)
			require.NoError(t, h.Pool().QueryRow(ctx, `SELECT psp_id FROM billing.subscriptions WHERE id=$1`, p.Sub).Scan(&actual))
			require.Equal(t, p.Target, actual)
		})
	}
}
