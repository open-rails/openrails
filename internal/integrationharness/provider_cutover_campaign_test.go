//go:build integration && provider_campaign

package integrationharness

import (
	"context"
	"encoding/json"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/money"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type hostCampaignMember struct {
	SubscriptionID        string `json:"subscription_id"`
	TargetPaymentMethodID string `json:"target_payment_method_id"`
}

// runHostProviderCampaign runs the SaaS script as separate processes over the
// same public HTTP origin; callbacks only manipulate/assert fixture gateways.
func runHostProviderCampaign(t *testing.T, apiBase, token string, merchantID, sourceID, targetID uuid.UUID,
	members []hostCampaignMember, expectedApplyExit int, afterApply func()) map[string]any {
	t.Helper()
	script := os.Getenv("PROVIDER_MIGRATION_SCRIPT")
	require.NotEmpty(t, script, "set PROVIDER_MIGRATION_SCRIPT to the exact SaaS script when selecting the cross-repo proof")
	require.True(t, filepath.IsAbs(script), "fixture must name the exact SaaS script")
	private := t.TempDir()
	require.NoError(t, os.Chmod(private, 0700))
	manifestPath := filepath.Join(private, "manifest.json")
	checkpoint := filepath.Join(private, "checkpoint.json")
	now := time.Now().UTC()
	manifest := map[string]any{
		"campaign_id": "core-loopback-" + uuid.NewString(),
		"merchant_id": merchantID.String(), "api_base": apiBase,
		"source_psp_id": sourceID.String(), "target_psp_id": targetID.String(),
		"policy": map[string]string{
			"opens_at":      now.Add(-time.Hour).Format(time.RFC3339),
			"ends_at":       now.Add(24 * time.Hour).Format(time.RFC3339),
			"grace_ends_at": now.Add(24 * time.Hour).Format(time.RFC3339),
			"operator":      "core-loopback-fixture", "readiness_evidence": "two authenticated NMI fixture accounts",
			"quiescence_evidence": "isolated test Runtime", "outreach_evidence": "none; local fixture",
			"grace_evidence": "unchanged paid-through; no grant",
		},
		"subscriptions": members,
	}
	data, err := json.Marshal(manifest)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0600))
	run := func(command string, expectedExit int) map[string]any {
		t.Helper()
		args := []string{"run", "--script", script, command, "--checkpoint", checkpoint}
		if command == "dry-run" {
			args = append(args, "--manifest", manifestPath)
		}
		cmd := exec.Command("uv", args...)
		cmd.Env = append(os.Environ(), "OPENRAILS_MERCHANT_TOKEN="+token)
		output, err := cmd.CombinedOutput()
		if expectedExit == 0 {
			require.NoError(t, err, "%s: %s", command, output)
		} else {
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, "%s: %s", command, output)
			require.Equal(t, expectedExit, exit.ExitCode(), "%s: %s", command, output)
		}
		var report map[string]any
		require.NoError(t, json.Unmarshal(output, &report), "%s: %s", command, output)
		return report
	}
	run("dry-run", 0)
	run("apply", expectedApplyExit)
	if afterApply != nil {
		afterApply()
	}
	run("resume", 0)
	report := run("report", 0)
	require.Equal(t, float64(len(members)), report["counts"].(map[string]any)["succeeded"])
	return report
}

// TestNMIProviderCutoverHostCampaign is deliberately selected separately from
// core's ordinary workflow gate; it requires the real SaaS operator script.
func TestNMIProviderCutoverHostCampaign(t *testing.T) {
	ctx := context.Background()
	h := New(t, ctx)
	g := newCutoverGateway(t)
	s := h.StartStandalone("USD", WithConfig(func(c *config.Config) { c.ProviderWriteMode = config.ProviderWriteModeFull }))
	var previous bool
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT enabled FROM billing.destructive_action_switch`).Scan(&previous))
	_, err := h.Pool().Exec(ctx, `UPDATE billing.destructive_action_switch SET enabled=true`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := h.Pool().Exec(ctx, `UPDATE billing.destructive_action_switch SET enabled=$1`, previous)
		require.NoError(t, err)
	})
	builder, ok := s.App().Runtime.CollectionResolver.(*money.MerchantCollectionAdapterBuilder)
	require.True(t, ok)
	builder.Endpoints.NMIV5BaseURL = g.Server.URL
	owner := s.ProvisionOwnedMerchant("cutover-campaign-" + uuid.NewString())
	client := s.Client(openrails.WithAPIKey(owner.APIKey), openrails.WithMerchantID(owner.MerchantID))
	p := seedCutoverHTTP(t, h, s, g, "lost_cancel", owner.MerchantID)
	runHostProviderCampaign(t, s.BaseURL, owner.APIKey, owner.MerchantID.UUID(), p.Source, p.Target, []hostCampaignMember{{SubscriptionID: openrails.SubscriptionID(p.Sub).String(), TargetPaymentMethodID: openrails.PaymentMethodID(p.NewMethod).String()}}, 1, nil)
	var key string
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT idempotency_key FROM billing.rail_intents WHERE subscription_id=$1 AND intent_type=$2`, p.Sub, intents.TypeNMIProviderCutover).Scan(&key))
	result, err := client.GetProviderCutover(ctx, openrails.SubscriptionID(p.Sub), strings.TrimPrefix(key, intents.TypeNMIProviderCutover+":"))
	require.NoError(t, err)
	assertCutoverCommitted(t, h, g, p, result)
}
