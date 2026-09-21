//go:build integration

package integrationharness

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/authkit"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/dbtest"
	embcp "github.com/open-rails/openrails/internal/operator"
	"github.com/open-rails/openrails/pkg/merchant"
)

// This qualification uses the actual standalone server, real local AuthKit
// API-key issuance/lookup and signed, registered service JWTs. No credential
// resolver is replaced. The source and target use separate billing schemas and
// different AuthKit authority groups in the isolated per-run test database.
// It does not qualify an independently deployed external AuthKit service.
func TestMerchantArchiveRealAuthKitHTTPAndEmbeddedParity(t *testing.T) {
	ctx := t.Context()
	// Development AuthKit supports its real in-memory ephemeral store. No Redis,
	// worker, provider credential or fake payment provider is needed for this test.
	h := &Harness{t: t, ctx: ctx, DSN: dbtest.SharedPostgresDSN(t), SuperDSN: dbtest.SharedSuperuserDSN(t)}
	readonly := WithConfig(func(cfg *config.Config) { cfg.ProviderWriteMode = config.ProviderWriteModeReadOnly })
	source := h.StartStandalone("USD", readonly)
	suffix := uuid.NewString()[:8]
	sourceMerchant := source.ProvisionOwnedMerchant("archive-source-" + suffix)
	sourceOwner := source.Client(openrails.WithAPIKey(sourceMerchant.APIKey), openrails.WithMerchantID(sourceMerchant.MerchantID), openrails.WithTimeout(0))

	// Production write path first: a retained grant and exact ledger balance are
	// created by the ordinary owner Client, rather than inserted as fixture rows.
	payer := openrails.CustomerID(uuid.New())
	const amount int64 = 9_007_199_254_740_993
	depositRequest := openrails.DepositCreditsRequest{CustomerID: &payer, Invoker: "archive-qualification", Currency: "USD", Amount: amount, Source: "archive-qualification", SourceID: uuid.NewString()}
	deposit, err := sourceOwner.DepositCredits(ctx, depositRequest)
	require.NoError(t, err)
	require.False(t, deposit.Replayed)
	before, err := sourceOwner.Balance(ctx, payer)
	require.NoError(t, err)
	require.Equal(t, amount, before.BalanceAmount)

	sourceEmbedded := archiveEmbeddedClient(t, h.DSN, config.DefaultSchema, sourceMerchant.MerchantID)
	var original, local bytes.Buffer
	require.NoError(t, sourceOwner.ExportMerchantBilling(ctx, &original))
	require.NoError(t, sourceEmbedded.ExportMerchantBilling(ctx, &local))
	require.Equal(t, original.Bytes(), local.Bytes(), "real AuthKit HTTP and embedded Client must emit the same billing book")
	require.Contains(t, original.String(), "9007199254740993")
	require.NotContains(t, original.String(), sourceMerchant.APIKey)
	require.NotContains(t, original.String(), sourceMerchant.GroupID)

	// Scope-limited automation is a real AuthKit service JWT: the registered
	// issuer has stored merchant authority and the signed token narrows it to export.
	exporter := source.RegisterServiceJWTIssuer("archive-exporter-"+suffix, sourceMerchant.MerchantSlug, []string{controlplane.PermMerchantBillingExport})
	exportClient := source.Client(openrails.WithAPIKey(exporter.Token), openrails.WithMerchantID(sourceMerchant.MerchantID), openrails.WithTimeout(0))
	var automated bytes.Buffer
	require.NoError(t, exportClient.ExportMerchantBilling(ctx, &automated))
	require.Equal(t, original.Bytes(), automated.Bytes())
	assertArchiveDeniedBeforeBody(t, source, exporter.Token, sourceMerchant.MerchantID, http.StatusForbidden)

	for _, role := range []string{controlplane.MerchantRoleViewer, controlplane.MerchantRoleSupport} {
		t.Run(role+" has no archive authority", func(t *testing.T) {
			status, key, body := mintKeyHTTP(t, source.BaseURL, sourceMerchant.APIKey, "archive-"+role, role)
			require.Equal(t, http.StatusCreated, status, string(body))
			reader := source.Client(openrails.WithAPIKey(key.Secret), openrails.WithMerchantID(sourceMerchant.MerchantID))
			require.ErrorIs(t, reader.ExportMerchantBilling(ctx, io.Discard), openrails.ErrDenied)
			assertArchiveDeniedBeforeBody(t, source, key.Secret, sourceMerchant.MerchantID, http.StatusForbidden)
		})
	}
	assertArchiveDeniedBeforeBody(t, source, "not-a-valid-credential", sourceMerchant.MerchantID, http.StatusUnauthorized)
	other := source.ProvisionOwnedMerchant("archive-other-" + suffix)
	wrongBinding := source.Client(openrails.WithAPIKey(other.APIKey), openrails.WithMerchantID(sourceMerchant.MerchantID))
	require.ErrorIs(t, wrongBinding.ExportMerchantBilling(ctx, io.Discard), openrails.ErrConflict)
	assertArchiveDeniedBeforeBody(t, source, other.APIKey, sourceMerchant.MerchantID, http.StatusConflict)
	// Without a forged binding, the other merchant is authorized only for its own
	// book, so the engine still rejects this archive's immutable merchant UUID.
	otherClient := source.Client(openrails.WithAPIKey(other.APIKey), openrails.WithMerchantID(other.MerchantID))
	_, err = otherClient.ImportMerchantBilling(ctx, bytes.NewReader(original.Bytes()))
	require.ErrorIs(t, err, openrails.ErrConflict)

	// Prepare a separately provisioned destination. Its AuthKit group/owner/key
	// are chosen locally, never from the source archive.
	const targetSchema = "archive_auth_target"
	dbtest.ApplyPostgresMigrations(t, h.SuperDSN, h.DSN, targetSchema)
	targetDB, err := db.NewDB(ctx, &config.DBConfig{URL: h.DSN, Schema: targetSchema})
	require.NoError(t, err)
	t.Cleanup(func() { _ = targetDB.Close() })
	_, err = targetDB.Qx(ctx).Exec(ctx, `INSERT INTO openrails.merchants(id,slug) VALUES($1,$2)`, dbtest.TestMerchantID.UUID(), dbtest.TestMerchantSlug)
	require.NoError(t, err)
	target := h.StartStandalone("USD", readonly, WithConfig(func(cfg *config.Config) { cfg.DB.Schema = targetSchema }))
	targetCP := embcp.Get(target.App())
	targetSlug := "archive-target-" + suffix
	targetGroup := h.ensureMerchantGroup(targetCP.Core(), targetSlug)
	targetOwnerID := h.ensureAPIKeyActor(targetCP, targetSlug)
	prepared, err := embcp.ProvisionMerchantForRestore(ctx, target.App(), embcp.ProvisionMerchantForRestoreRequest{MerchantID: sourceMerchant.MerchantID, ExistingGroupID: targetGroup, OwnerUserID: targetOwnerID})
	require.NoError(t, err)
	require.Equal(t, sourceMerchant.MerchantID, prepared.MerchantID)
	require.NotEqual(t, sourceMerchant.GroupID, targetGroup)
	targetKey := target.MintAPIKey(targetSlug, "archive-target-owner", nil)
	targetOwner := target.Client(openrails.WithAPIKey(targetKey), openrails.WithMerchantID(sourceMerchant.MerchantID), openrails.WithTimeout(0))
	sourceOnTarget := target.Client(openrails.WithAPIKey(sourceMerchant.APIKey), openrails.WithMerchantID(sourceMerchant.MerchantID))
	require.ErrorIs(t, sourceOnTarget.ExportMerchantBilling(ctx, io.Discard), openrails.ErrDenied)
	assertArchiveDeniedBeforeBody(t, target, sourceMerchant.APIKey, sourceMerchant.MerchantID, http.StatusForbidden)

	importer := target.RegisterServiceJWTIssuer("archive-importer-"+suffix, targetSlug, []string{controlplane.PermMerchantBillingImport})
	importClient := target.Client(openrails.WithAPIKey(importer.Token), openrails.WithMerchantID(sourceMerchant.MerchantID), openrails.WithTimeout(0))
	require.ErrorIs(t, importClient.ExportMerchantBilling(ctx, io.Discard), openrails.ErrDenied)
	receipt, err := importClient.ImportMerchantBilling(ctx, bytes.NewReader(original.Bytes()))
	require.NoError(t, err)
	require.False(t, receipt.AlreadyImported)
	replay, err := targetOwner.ImportMerchantBilling(ctx, bytes.NewReader(original.Bytes()))
	require.NoError(t, err)
	require.True(t, replay.AlreadyImported)
	require.Equal(t, receipt.Digest, replay.Digest)

	targetEmbedded := archiveEmbeddedClient(t, h.DSN, targetSchema, sourceMerchant.MerchantID)
	embeddedReplay, err := targetEmbedded.ImportMerchantBilling(ctx, bytes.NewReader(original.Bytes()))
	require.NoError(t, err)
	require.True(t, embeddedReplay.AlreadyImported)
	require.Equal(t, receipt.Digest, embeddedReplay.Digest)
	for _, client := range []*openrails.Client{targetOwner, targetEmbedded} {
		balance, err := client.Balance(ctx, payer)
		require.NoError(t, err)
		require.Equal(t, before.BalanceAmount, balance.BalanceAmount)
		restored, err := client.GetDeposit(ctx, payer, depositRequest.SourceID)
		require.NoError(t, err)
		require.Equal(t, deposit.ID, restored.ID)
		require.Equal(t, deposit.Amount, restored.Amount)
		var roundtrip bytes.Buffer
		require.NoError(t, client.ExportMerchantBilling(ctx, &roundtrip))
		require.Equal(t, original.Bytes(), roundtrip.Bytes())
	}
	// The restored book does not recreate source authentication or replace the
	// target group. Both source and destination roles stay attached to their own
	// authority group; only the destination credential controls the restored UUID.
	var storedGroup string
	require.NoError(t, targetDB.Qx(ctx).QueryRow(ctx, `SELECT permission_group_id FROM openrails.merchants WHERE id=$1`, sourceMerchant.MerchantID.UUID()).Scan(&storedGroup))
	require.Equal(t, targetGroup, storedGroup)
	sourceCP := embcp.Get(source.App())
	sourceOwnerID := h.ensureAPIKeyActor(sourceCP, sourceMerchant.MerchantSlug)
	allowed, err := targetCP.Core().CanOnGroup(ctx, authkit.UserSubject(sourceOwnerID), targetGroup, controlplane.MerchantType.OwnerGrant())
	require.NoError(t, err)
	require.False(t, allowed)
	allowed, err = targetCP.Core().CanOnGroup(ctx, authkit.UserSubject(targetOwnerID), targetGroup, controlplane.MerchantType.OwnerGrant())
	require.NoError(t, err)
	require.True(t, allowed)
	require.ErrorIs(t, sourceOnTarget.ExportMerchantBilling(ctx, io.Discard), openrails.ErrDenied)
	assertArchiveDeniedBeforeBody(t, target, sourceMerchant.APIKey, sourceMerchant.MerchantID, http.StatusForbidden)
}

func archiveEmbeddedClient(t *testing.T, dsn, schema string, mid merchant.ID) *openrails.Client {
	t.Helper()
	rt, err := embed.New(t.Context(), embed.Options{
		Config: &config.Config{Env: "dev", TestMode: config.CredentialPostureSandbox, ProviderWriteMode: config.ProviderWriteModeReadOnly, SecretBackend: config.SecretBackendDB, DB: &config.DBConfig{URL: dsn, Schema: schema}},
		River:  embed.RiverManagedByOpenRails(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })
	client, err := rt.Client(openrails.WithMerchantID(mid), openrails.WithCurrency("USD"), openrails.WithTimeout(0))
	require.NoError(t, err)
	return client
}

type archiveUnreadBody struct{ reads int }

func (b *archiveUnreadBody) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (*archiveUnreadBody) Close() error               { return nil }

// ServeHTTP is the SAME booted standalone Handler used by the TCP assertions.
// Supplying a read-counting body here distinguishes authorization-before-parse
// from the HTTP client's independent eagerness to send its request body.
func assertArchiveDeniedBeforeBody(t *testing.T, surface *Surface, token string, mid merchant.ID, want int) {
	t.Helper()
	body := &archiveUnreadBody{}
	req := httptest.NewRequest(http.MethodPost, "/v1/merchant/billing-archive", body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(merchant.BindingHeader, mid.String())
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("X-OpenRails-Host-Principal", mid.String()) // no header can forge an in-process principal
	response := httptest.NewRecorder()
	surface.Server().Handler().ServeHTTP(response, req)
	require.Equal(t, want, response.Code, strings.TrimSpace(response.Body.String()))
	require.Zero(t, body.reads, "archive body must remain unread on authorization refusal")
}
