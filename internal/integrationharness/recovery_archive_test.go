//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/merchantarchive"
	"github.com/open-rails/openrails/internal/migrate"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCustomerRetryArchiveRestoresOriginalRequestReplay(t *testing.T) {
	ctx := context.Background()
	h := &Harness{t: t, ctx: ctx, DSN: dbtest.SharedPostgresDSN(t), SuperDSN: dbtest.SharedSuperuserDSN(t)}
	gateway := NewFakeNMIGateway(t)
	source := h.StartStandalone("USD", WithConfig(func(c *config.Config) {
		c.ProviderSandbox = &config.ProviderSandboxConfig{NMIGatewayURL: gateway.URL}
	}))
	owned := source.ProvisionOwnedMerchant("recovery-archive-" + uuid.NewString()[:8])
	mid := owned.MerchantID
	client, err := openrails.NewRemote(source.BaseURL, openrails.WithAPIKey(owned.APIKey), openrails.WithMerchantID(mid), openrails.WithTimeout(0))
	require.NoError(t, err)
	f := h.SeedPastDueSubscription(source.App().Runtime, mid)
	gateway.RegisterPlan(f.RailSubscriptionID, "12.00", "USD")
	gateway.SetMode(NMISaleUncertain)
	gateway.SetVisible(false)
	request := openrails.RetrySubscriptionNowRequest{CustomerID: openrails.CustomerID(f.Customer), SubscriptionID: f.Subscription, PaymentMethodID: &f.Method, IdempotencyKey: "archive-original-" + uuid.NewString()}
	unknown, err := client.RetrySubscriptionNow(ctx, request)
	require.NoError(t, err)
	require.Equal(t, intents.StatusUnknownNeedsVerify, unknown.Operation.Status)
	var archive bytes.Buffer
	require.Error(t, merchantarchive.Export(ctx, source.App().Runtime.DB, mid, &archive))
	require.Empty(t, archive.Bytes())
	gateway.SetVisible(true)
	h.MakeOperationDue(unknown.Operation.ID)
	require.NoError(t, source.App().Runtime.DB.RunInMerchantConn(merchant.WithID(ctx, mid), func(c context.Context) error {
		_, err := source.App().Runtime.IntentRunner().RunVerifyOnce(c)
		return err
	}))
	completed, err := client.RetrySubscriptionNow(ctx, request)
	require.NoError(t, err)
	require.Equal(t, intents.StatusSucceeded, completed.Operation.Status)
	require.NotNil(t, completed.Payment)
	require.Equal(t, 1, gateway.SaleCount())
	merchantCtx, sourceRelease, err := source.App().Runtime.DB.WithMerchantConn(merchant.WithID(ctx, mid))
	require.NoError(t, err)
	defer sourceRelease()
	original, err := intents.NewStore(source.App().Runtime.DB).Get(merchantCtx, completed.Operation.ID)
	require.NoError(t, err)
	var payload intents.ManualRebillPayload
	require.NoError(t, json.Unmarshal(original.Payload, &payload))
	require.Equal(t, f.Vault, payload.Instrument.RailCustomerRef)
	require.Equal(t, f.PSP, payload.Instrument.PSPID)
	require.Equal(t, int64(12_000_000), payload.Amount)
	require.Contains(t, string(original.ResultEvidence), "qualified_rebill_receipt")
	var sourceClaimsReleased bool
	require.NoError(t, h.Pool().QueryRow(ctx, `SELECT dunning_claim_holder IS NULL AND dunning_claimed_until IS NULL FROM openrails.subscriptions WHERE id=$1`, f.Subscription).Scan(&sourceClaimsReleased))
	require.True(t, sourceClaimsReleased)

	_, err = h.Pool().Exec(ctx, `UPDATE openrails.subscriptions SET dunning_claim_holder='expired-owner', dunning_claimed_until=$1 WHERE id=$2`, time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), f.Subscription)
	require.NoError(t, err)
	require.Error(t, merchantarchive.Export(ctx, source.App().Runtime.DB, mid, &archive), "even an expired claim is not portable")
	require.Empty(t, archive.Bytes())
	_, err = h.Pool().Exec(ctx, `UPDATE openrails.subscriptions SET dunning_claim_holder=NULL, dunning_claimed_until=NULL WHERE id=$1`, f.Subscription)
	require.NoError(t, err)

	require.NoError(t, merchantarchive.Export(ctx, source.App().Runtime.DB, mid, &archive))
	adminDSN, appDSN := dbtest.SharedRLSPostgres(t)
	schema := "rebill_archive_" + uuid.NewString()[:8]
	require.NoError(t, migrate.RunPostgres(ctx, &config.Config{DB: &config.DBConfig{URL: adminDSN, Schema: schema}}))
	targetDB, err := db.NewDB(ctx, &config.DBConfig{URL: appDSN, Schema: schema})
	require.NoError(t, err)
	t.Cleanup(func() { _ = targetDB.Close() })
	_, err = targetDB.Qx(ctx).Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, mid.UUID(), owned.MerchantSlug)
	require.NoError(t, err)
	_, err = merchantarchive.Restore(ctx, targetDB, mid, bytes.NewReader(archive.Bytes()))
	require.NoError(t, err)
	targetCtx, targetRelease, err := targetDB.WithMerchantConn(merchant.WithID(ctx, mid))
	require.NoError(t, err)
	defer targetRelease()
	restored, err := intents.NewStore(targetDB).Get(targetCtx, completed.Operation.ID)
	require.NoError(t, err)
	require.JSONEq(t, string(original.Payload), string(restored.Payload))
	require.JSONEq(t, string(original.ResultEvidence), string(restored.ResultEvidence))
	var claimsReleased bool
	require.NoError(t, targetDB.Qx(targetCtx).QueryRow(targetCtx, `SELECT s.dunning_claim_holder IS NULL AND s.dunning_claimed_until IS NULL AND i.claimed_until IS NULL FROM openrails.subscriptions s JOIN openrails.rail_intents i ON i.subscription_id=s.id WHERE i.id=$1`, completed.Operation.ID).Scan(&claimsReleased))
	require.True(t, claimsReleased)
	// The restored runtime has no provider credentials or endpoint override and
	// provider writes are disabled. The ordinary public Client must replay the
	// original customer key/body entirely from the retained operation.
	target, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, MerchantSource: config.MerchantSourceAPI, SecretBackend: config.SecretBackendDB, ProviderWriteMode: config.ProviderWriteModeReadOnly, DB: &config.DBConfig{URL: appDSN, Schema: schema}}, River: embedded.RiverManagedByOpenRails()}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close(ctx) })
	replayClient, err := target.Client(openrails.WithMerchantID(mid), openrails.WithTimeout(0))
	require.NoError(t, err)
	before := recoveryArchiveCounts(t, targetDB, targetCtx, f.Subscription)
	replayed, err := replayClient.RetrySubscriptionNow(ctx, request)
	require.NoError(t, err)
	require.True(t, replayed.Replayed)
	require.Equal(t, completed.Operation, replayed.Operation)
	require.Equal(t, completed.Payment.ID, replayed.Payment.ID)
	require.Equal(t, completed.Subscription.CurrentPeriodEndsAt, replayed.Subscription.CurrentPeriodEndsAt)
	require.Equal(t, before, recoveryArchiveCounts(t, targetDB, targetCtx, f.Subscription))
	require.Equal(t, 1, gateway.SaleCount())
	changed := uuid.New()
	request.PaymentMethodID = &changed
	_, err = replayClient.RetrySubscriptionNow(ctx, request)
	requireRefusal(t, err, openrails.ErrConflict, openrails.CodeSubscriptionRetryIdempotencyConflict)
	require.Equal(t, before, recoveryArchiveCounts(t, targetDB, targetCtx, f.Subscription))
	t.Logf("restored request replay: operation=%s, provider sales=1, payments=%d, entitlements=%d, period=%s", completed.Operation.ID, before.Payments, before.Entitlements, before.PeriodEnd.Format(time.RFC3339))
}

type recoveryArchiveSnapshot struct {
	Payments, Operations, Entitlements int
	PeriodEnd                          time.Time
}

func recoveryArchiveCounts(t *testing.T, database *db.DB, ctx context.Context, subscription uuid.UUID) recoveryArchiveSnapshot {
	t.Helper()
	var out recoveryArchiveSnapshot
	require.NoError(t, database.Qx(ctx).QueryRow(ctx, `SELECT (SELECT count(*) FROM openrails.payments WHERE subscription_id=s.id AND status='completed'), (SELECT count(*) FROM openrails.rail_intents WHERE subscription_id=s.id), (SELECT count(*) FROM openrails.entitlements WHERE subscription_id=s.id), s.current_period_ends_at FROM openrails.subscriptions s WHERE s.id=$1`, subscription).Scan(&out.Payments, &out.Operations, &out.Entitlements, &out.PeriodEnd))
	return out
}
