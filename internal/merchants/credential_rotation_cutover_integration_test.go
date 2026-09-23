//go:build integration

package merchants

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/providerqualification"
	"github.com/open-rails/openrails/pkg/merchant"
)

// or#812 — credential rotation and its CROSS-NODE cutover.
//
// The property under test is the one a multi-node SaaS actually needs: after an
// operator rotates a PSP credential on ONE node, no OTHER node may keep
// presenting the retired credential to the gateway. Every node runs a
// TTL-bounded in-process secret cache, so before or#812 a node that had read
// the old credential kept serving it for up to DefaultSecretCacheTTL.
//
// The test simulates two nodes honestly: two independent merchants.Service
// instances, each with its OWN cache wrapper, over ONE shared DB-backed secret
// store and ONE shared Postgres. Nothing is shared in process except the
// database — exactly the sharing two app nodes have.

// rotationGateway fakes the NMI query endpoint the credential probe hits. It
// accepts exactly one security key at a time; anything else is rejected the way
// a real gateway rejects a bad key (an <error_response> body).
type rotationGateway struct {
	mu       sync.Mutex
	accepted string
	seen     []string
}

func (g *rotationGateway) accept(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.accepted = key
}

func (g *rotationGateway) keysSeen() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seen...)
}

func newRotationGateway(t *testing.T, accepted string) (*rotationGateway, *httptest.Server) {
	t.Helper()
	gw := &rotationGateway{accepted: accepted}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		key := r.Form.Get("security_key")
		gw.mu.Lock()
		gw.seen = append(gw.seen, key)
		ok := key == gw.accepted
		gw.mu.Unlock()
		if !ok {
			_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response><error_response>Invalid Security Key</error_response></nm_response>`))
			return
		}
		_, _ = w.Write([]byte(`<?xml version="1.0"?><nm_response></nm_response>`))
	}))
	t.Cleanup(server.Close)
	return gw, server
}

// rotationNode is one simulated app node: its own merchants.Service and its own
// per-process credential cache, over the shared database.
type rotationNode struct {
	svc   *Service
	cache MerchantSecretStore
}

func newRotationNode(t *testing.T, pool *pgxpool.Pool, probeURL string) *rotationNode {
	t.Helper()
	wrapped := db.WrapPool(pool, "")
	backend, err := NewDBSecretStore(wrapped)
	require.NoError(t, err)
	// A long TTL is the point: nothing in this test may pass because a cache
	// entry happened to expire.
	cache := NewCachedSecretStore(backend, time.Hour)
	svc, err := NewService(wrapped, cache, "live")
	require.NoError(t, err)
	svc.nmiCredentialProbeQueryURL = probeURL
	return &rotationNode{svc: svc, cache: cache}
}

// resolve is exactly what a checkout money path does on this node: resolve the
// active PSP's secret ref (name + rotation version floor) from the shared DB,
// then read the credential through this node's cache honouring that floor.
func (n *rotationNode) resolve(t *testing.T, id merchant.ID) string {
	t.Helper()
	ref, ok, err := n.svc.ActivePSPSecretRef(context.Background(), id, "nmi", "live", "security_key")
	require.NoError(t, err)
	require.True(t, ok, "the merchant must have an active NMI PSP")
	sec, err := ReadSecretRef(context.Background(), n.cache, id, ref)
	require.NoError(t, err)
	return strings.TrimSpace(sec.Value)
}

// resolveUnversioned is the pre-or#812 read: name only, no version floor. It
// exists so the test can PROVE the cache was genuinely warm with the old value
// and that the version floor — not luck or an expiry — did the cutover.
func (n *rotationNode) resolveUnversioned(t *testing.T, id merchant.ID) string {
	t.Helper()
	ref, ok, err := n.svc.ActivePSPSecretRef(context.Background(), id, "nmi", "live", "security_key")
	require.NoError(t, err)
	require.True(t, ok)
	sec, err := n.cache.Get(context.Background(), id, ref.Name)
	require.NoError(t, err)
	return strings.TrimSpace(sec.Value)
}

func (n *rotationNode) rotate(t *testing.T, id merchant.ID, accountID, key string) error {
	t.Helper()
	_, err := n.svc.UpsertPaymentProviderConfig(context.Background(), id, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: currentPublicationRevision(t, n.svc, id, "nmi"),
		AccountID:   accountID,
		Credentials: map[string]string{"security_key": key},
	})
	return err
}

func TestCredentialRotationCutsOverAcrossNodes(t *testing.T) {
	pool := newTestPool(t)
	gw, server := newRotationGateway(t, "key-v1")
	nodeA := newRotationNode(t, pool, server.URL)
	nodeB := newRotationNode(t, pool, server.URL)

	ctx := context.Background()
	tn, _, err := nodeA.svc.Provision(ctx, ProvisionRequest{
		Slug:              "rotation-cutover-812",
		PermissionGroupID: "group-rotation-cutover-812",
	})
	require.NoError(t, err)
	const accountID = "rotation-812-gateway"

	// Arm on node A with the credential the gateway currently accepts.
	require.NoError(t, nodeA.rotate(t, tn.ID, accountID, "key-v1"))

	// Node B reads it and now holds it in its own cache for an hour.
	require.Equal(t, "key-v1", nodeB.resolve(t, tn.ID))

	cfg, err := nodeA.svc.GetPaymentProviderConfig(ctx, tn.ID, "nmi", "live")
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Credentials["security_key"].RotationVersion,
		"arming must record the credential's rotation version on the PSP row")

	// --- A rotation whose new credential does NOT validate ------------------
	//
	// The gateway still only accepts key-v1, so probing key-v2 fails. The whole
	// rotation must fail: nothing written, no version floor moved, and BOTH
	// nodes keep serving the old credential.
	probesBefore := len(gw.keysSeen())
	err = nodeA.rotate(t, tn.ID, accountID, "key-v2")
	require.Error(t, err, "a credential the provider rejects must fail the rotation")
	require.Contains(t, err.Error(), "validate nmi credentials")
	require.Greater(t, len(gw.keysSeen()), probesBefore, "the NEW credential must actually be probed")
	require.Contains(t, gw.keysSeen(), "key-v2", "the probe must use the credential being rotated IN, not the stored one")

	require.Equal(t, "key-v1", nodeA.resolve(t, tn.ID), "a refused rotation must leave the old credential serving")
	require.Equal(t, "key-v1", nodeB.resolve(t, tn.ID), "a refused rotation must not disturb any other node")

	var stored string
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT value FROM billing.merchant_secrets WHERE merchant_id = $1 AND name LIKE 'credential_candidates/%/psps/nmi/%'
	`, tn.ID.UUID()).Scan(&stored))
	require.Equal(t, "key-v1", stored, "a refused rotation must never write the rejected credential")

	cfg, err = nodeA.svc.GetPaymentProviderConfig(ctx, tn.ID, "nmi", "live")
	require.NoError(t, err)
	require.Equal(t, 1, cfg.Credentials["security_key"].RotationVersion,
		"a refused rotation must not move the version floor")

	// --- A rotation whose new credential DOES validate ----------------------
	gw.accept("key-v2")
	require.NoError(t, nodeA.rotate(t, tn.ID, accountID, "key-v2"))

	cfg, err = nodeA.svc.GetPaymentProviderConfig(ctx, tn.ID, "nmi", "live")
	require.NoError(t, err)
	require.Equal(t, 2, cfg.Credentials["security_key"].RotationVersion,
		"logical rotation advances independently of immutable backend version one")

	// The rotating node is immediately correct (write-through).
	require.Equal(t, "key-v2", nodeA.resolve(t, tn.ID))

	// THE POINT: node B never saw the rotation, its cache entry is minutes old
	// with ~an hour of TTL left, and it still serves the NEW credential.
	require.Equal(t, "key-v2", nodeB.resolve(t, tn.ID),
		"a rotation on another node must cut over here at the next read, not after a cache TTL")

	// A third node also follows each new immutable reference immediately,
	// without waiting for its independently cached predecessor to expire.
	nodeC := newRotationNode(t, pool, server.URL)
	require.Equal(t, "key-v2", nodeC.resolveUnversioned(t, tn.ID), "a cold cache always reads through")

	gw.accept("key-v3")
	require.NoError(t, nodeA.rotate(t, tn.ID, accountID, "key-v3"))
	require.Equal(t, "key-v3", nodeC.resolveUnversioned(t, tn.ID),
		"a fresh published immutable reference selects the new candidate even through a warm cache")
	require.Equal(t, "key-v3", nodeC.resolve(t, tn.ID),
		"the same node, same warm cache, reading WITH the version floor, gets the rotated credential")
	require.Equal(t, "key-v3", nodeB.resolve(t, tn.ID))
}

// TestRotationPreservesUntouchedCredentialVersions covers the partial rotation:
// an operator rotating one credential must not drop the version floors of the
// credentials the request left alone, or those would silently lose their
// cross-node cutover guarantee.
func TestRotationPreservesUntouchedCredentialVersions(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	gw, server := newRotationGateway(t, "nmi-key-a")
	node := newRotationNode(t, pool, server.URL)

	tn, _, err := node.svc.Provision(ctx, ProvisionRequest{
		Slug:              "rotation-partial-812",
		PermissionGroupID: "group-rotation-partial-812",
	})
	require.NoError(t, err)

	const accountID = "rotation-812-partial"
	_, err = node.svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: currentPublicationRevision(t, node.svc, tn.ID, "nmi"),
		AccountID: accountID,
		Credentials: map[string]string{
			"security_key":           "nmi-key-a",
			"webhook_signing_secret": "whsec-a",
		},
	})
	require.NoError(t, err)

	// Rotate ONLY the security key.
	gw.accept("nmi-key-b")
	cfg, err := node.svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: currentPublicationRevision(t, node.svc, tn.ID, "nmi"),
		AccountID:   accountID,
		Credentials: map[string]string{"security_key": "nmi-key-b"},
	})
	require.NoError(t, err)
	require.Equal(t, 2, cfg.Credentials["security_key"].RotationVersion, "the rotated credential generation must rise")
	require.Equal(t, 1, cfg.Credentials["webhook_signing_secret"].RotationVersion,
		"an untouched credential must keep the floor it already had")

	// Re-publishing the SAME value in the same custody leaves the logical
	// generation unchanged, even though it stages a new immutable candidate.
	cfg, err = node.svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: currentPublicationRevision(t, node.svc, tn.ID, "nmi"),
		AccountID:   accountID,
		Credentials: map[string]string{"security_key": "nmi-key-b"},
	})
	require.NoError(t, err)
	require.Equal(t, 2, cfg.Credentials["security_key"].RotationVersion)
	require.Equal(t, 1, cfg.Credentials["webhook_signing_secret"].RotationVersion)
}

// TestProviderEvidenceMergePreservesForeignKeys pins the evidence-merge
// contract: the PSP row's evidence document is shared with the manifest arm
// path, so an API rotation must overlay its own fields rather than replace the
// document (which would drop a manifest-armed PSP's settings).
func TestProviderEvidenceMergePreservesForeignKeys(t *testing.T) {
	existing := []byte(`{"source":"merchant_config_manifest","settings":{"tokenization_key":"tok-abc"}}`)
	merged, err := marshalProviderEvidence(existing, map[string]string{"public": "value"}, true, map[string]int{"security_key": 3})
	require.NoError(t, err)

	require.Equal(t, map[string]int{"security_key": 3}, CredentialVersions(merged))
	require.Equal(t, "tok-abc", pspSettings(merged)["tokenization_key"], "manifest settings must survive an API rotation")

	evidence := unmarshalProviderEvidence(merged)
	require.True(t, evidence.CredentialsValidated)
	require.Equal(t, map[string]string{"public": "value"}, evidence.PublicConfig)
	require.Contains(t, string(merged), `"source":"merchant_config_manifest"`)
}

// Immutable candidates may all be KV version one. Qualification must still
// reject an old A credential after A -> B -> A, even though its value matches.
func TestCredentialPublicationGenerationFencesABA(t *testing.T) {
	pool := newTestPool(t)
	gateway, server := newRotationGateway(t, "key-a")
	node := newRotationNode(t, pool, server.URL)
	tenant, _, err := node.svc.Provision(t.Context(), ProvisionRequest{Slug: "rotation-aba-" + uuid.NewString(), PermissionGroupID: "rotation-aba-" + uuid.NewString()})
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), tenant.ID)
	database, err := db.NewWithPGXPool(pool, "")
	require.NoError(t, err)
	request := UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0), AccountID: "aba-" + uuid.NewString(), Credentials: map[string]string{"security_key": "key-a"}}
	initial, err := node.svc.UpsertPaymentProviderConfig(ctx, tenant.ID, "nmi", request)
	require.NoError(t, err)
	record := &providerqualification.Record{PSPID: initial.ID, Environment: "live", Contract: providerqualification.NMIContract, EvidenceRef: "fixture:aba"}
	fingerprint := providerqualification.Fingerprint("key-a")
	require.NoError(t, providerqualification.Set(ctx, database, initial.ID, record, fingerprint, 1))
	assertGeneration := func(expected int) {
		t.Helper()
		row, err := database.Gen(ctx).GetPSP(ctx, gen.GetPSPParams{MerchantID: tenant.ID.UUID(), ID: initial.ID})
		require.NoError(t, err)
		version, err := providerqualification.CredentialVersion(row)
		require.NoError(t, err)
		require.Equal(t, expected, version)
		ref, err := PSPSecretRef("nmi", "live", request.AccountID, row.Evidence, "security_key")
		require.NoError(t, err)
		require.Equal(t, 1, ref.MinVersion, "physical candidate version is independent")
		_, err = ReadSecretRef(ctx, node.cache, tenant.ID, ref)
		require.NoError(t, err, "logical generation must not become a backend version floor")
	}
	assertGeneration(1)
	replay, err := node.svc.UpsertPaymentProviderConfig(ctx, tenant.ID, "nmi", request)
	require.NoError(t, err)
	require.Equal(t, initial.Revision, replay.Revision)
	assertGeneration(1)
	_, err = node.svc.UpsertPaymentProviderConfig(ctx, tenant.ID, "nmi", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(initial.Revision), AccountID: request.AccountID, PublicConfig: map[string]string{"tokenization_key": "public-fixture"}})
	require.NoError(t, err)
	assertGeneration(1)
	_, err = providerqualification.Read(ctx, database, initial.ID)
	require.NoError(t, err, "metadata-only publication keeps current qualification")
	for i, key := range []string{"key-b", "key-a"} {
		gateway.accept(key)
		require.NoError(t, node.rotate(t, tenant.ID, request.AccountID, key))
		assertGeneration(i + 2)
		_, err := providerqualification.Read(ctx, database, initial.ID)
		require.ErrorIs(t, err, providerqualification.ErrUnqualified)
	}
	require.Equal(t, "key-a", node.resolve(t, tenant.ID))
	historical, err := node.svc.UpsertPaymentProviderConfig(ctx, tenant.ID, "nmi", request)
	require.NoError(t, err)
	require.Equal(t, initial.Revision, historical.Revision, "completed retries retain their original receipt")
	assertGeneration(3)
	require.ErrorIs(t, providerqualification.Set(ctx, database, initial.ID, record, fingerprint, 1), providerqualification.ErrInvalid, "an old same-value client cannot restore qualification after ABA")
	require.NoError(t, providerqualification.Set(ctx, database, initial.ID, record, fingerprint, 3))
	_, err = providerqualification.Read(ctx, database, initial.ID)
	require.NoError(t, err)
}

func TestWebhookOverlapGenerationSurvivesRetirement(t *testing.T) {
	pool := newTestPool(t)
	_, server := newRotationGateway(t, "unused")
	node := newRotationNode(t, pool, server.URL)
	tenant, _, err := node.svc.Provision(t.Context(), ProvisionRequest{Slug: "rotation-overlap-" + uuid.NewString(), PermissionGroupID: "rotation-overlap-" + uuid.NewString()})
	require.NoError(t, err)
	ctx := merchant.WithID(t.Context(), tenant.ID)
	account := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = node.svc.UpsertPaymentProviderConfig(ctx, tenant.ID, "stripe", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0), AccountID: account, Credentials: map[string]string{"webhook_signing_secret": "whsec_overlap_a"}})
	require.NoError(t, err)
	publication := node.svc.StripeWebhookPublication(tenant.ID, account)
	_, err = publication.Load(ctx)
	require.NoError(t, err)
	require.NoError(t, publication.Publish(ctx, "we_overlap_b", "whsec_overlap_b"))
	cfg, err := node.svc.GetPaymentProviderConfig(ctx, tenant.ID, "stripe", "live")
	require.NoError(t, err)
	require.Equal(t, 2, cfg.Credentials["webhook_signing_secret"].RotationVersion)
	require.Equal(t, 1, cfg.Credentials["webhook_signing_secret_previous"].RotationVersion)
	require.NoError(t, publication.RetireOverlap(ctx))
	cfg, err = node.svc.GetPaymentProviderConfig(ctx, tenant.ID, "stripe", "live")
	require.NoError(t, err)
	require.False(t, cfg.Credentials["webhook_signing_secret_previous"].Configured)
	require.Equal(t, 1, cfg.Credentials["webhook_signing_secret_previous"].RotationVersion, "retirement retains the logical generation tombstone")
	require.NoError(t, publication.Publish(ctx, "we_overlap_a", "whsec_overlap_a"))
	cfg, err = node.svc.GetPaymentProviderConfig(ctx, tenant.ID, "stripe", "live")
	require.NoError(t, err)
	require.Equal(t, 3, cfg.Credentials["webhook_signing_secret"].RotationVersion)
	require.Equal(t, 2, cfg.Credentials["webhook_signing_secret_previous"].RotationVersion, "a reused overlap slot advances rather than resetting")
	loaded, ok, err := node.svc.LoadStripeCredentialsForAccount(ctx, tenant.ID, account)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "whsec_overlap_a", loaded.WebhookSigningSecret)
	require.Equal(t, "whsec_overlap_b", loaded.WebhookSigningPrevious)
}

func TestCredentialPublicationRecoversMissingPredecessor(t *testing.T) {
	pool := newTestPool(t)
	gateway, server := newRotationGateway(t, "key-missing-old")
	original := newRotationNode(t, pool, server.URL)
	tenant, _, err := original.svc.Provision(t.Context(), ProvisionRequest{Slug: "rotation-repair-" + uuid.NewString(), PermissionGroupID: "rotation-repair-" + uuid.NewString()})
	require.NoError(t, err)
	account := "repair-" + uuid.NewString()
	require.NoError(t, original.rotate(t, tenant.ID, account, "key-missing-old"))
	ref, ok, err := original.svc.ActivePSPSecretRef(t.Context(), tenant.ID, "nmi", "live", "security_key")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, original.cache.Delete(t.Context(), tenant.ID, ref.Name))
	recovery := newRotationNode(t, pool, server.URL)
	_, err = ReadSecretRef(t.Context(), recovery.cache, tenant.ID, ref)
	require.ErrorIs(t, err, ErrSecretNotFound, "the old published material is actually missing")
	gateway.accept("key-recovered")
	require.NoError(t, recovery.rotate(t, tenant.ID, account, "key-recovered"))
	cfg, err := recovery.svc.GetPaymentProviderConfig(t.Context(), tenant.ID, "nmi", "live")
	require.NoError(t, err)
	require.Equal(t, 2, cfg.Credentials["security_key"].RotationVersion)
	require.Equal(t, "key-recovered", recovery.resolve(t, tenant.ID))
}

func TestWebhookRotationRefusesMissingOverlap(t *testing.T) {
	pool := newTestPool(t)
	_, server := newRotationGateway(t, "unused")
	node := newRotationNode(t, pool, server.URL)
	tenant, _, err := node.svc.Provision(t.Context(), ProvisionRequest{Slug: "overlap-missing-" + uuid.NewString(), PermissionGroupID: "overlap-missing-" + uuid.NewString()})
	require.NoError(t, err)
	account := "acct_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	initial, err := node.svc.UpsertPaymentProviderConfig(t.Context(), tenant.ID, "stripe", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(0), AccountID: account, Credentials: map[string]string{"webhook_signing_secret": "whsec_missing_previous"}})
	require.NoError(t, err)
	ref, ok, err := node.svc.ActivePSPSecretRef(t.Context(), tenant.ID, "stripe", "live", "webhook_signing_secret")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, node.cache.Delete(t.Context(), tenant.ID, ref.Name))
	fresh := newRotationNode(t, pool, server.URL)
	_, err = fresh.svc.UpsertPaymentProviderConfig(t.Context(), tenant.ID, "stripe", UpsertPaymentProviderConfigRequest{OperationID: uuid.New(), ExpectedRevision: publicationRevision(initial.Revision), AccountID: account, Credentials: map[string]string{"webhook_signing_secret": "whsec_new_candidate"}})
	require.ErrorIs(t, err, ErrSecretNotFound, "rotation cannot publish an unreadable required overlap reference")
	var evidence []byte
	require.NoError(t, pool.QueryRow(t.Context(), `SELECT evidence FROM billing.psps WHERE merchant_id=$1 AND id=$2`, tenant.ID.UUID(), initial.ID).Scan(&evidence))
	require.Equal(t, initial.Revision, unmarshalProviderEvidence(evidence).Revision)
	require.Equal(t, 1, CredentialVersions(evidence)["webhook_signing_secret"])
}
