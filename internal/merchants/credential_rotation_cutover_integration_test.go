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

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
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
	_, err := n.svc.UpsertPaymentProviderConfig(context.Background(), id, "nmi", UpsertPaymentProviderConfigRequest{
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
		SELECT value FROM openrails.merchant_secrets WHERE merchant_id = $1 AND name LIKE 'psps/nmi/%'
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
		"a committed rotation must raise the version floor")

	// The rotating node is immediately correct (write-through).
	require.Equal(t, "key-v2", nodeA.resolve(t, tn.ID))

	// THE POINT: node B never saw the rotation, its cache entry is minutes old
	// with ~an hour of TTL left, and it still serves the NEW credential.
	require.Equal(t, "key-v2", nodeB.resolve(t, tn.ID),
		"a rotation on another node must cut over here at the next read, not after a cache TTL")

	// And prove the cutover was the version floor doing work rather than an
	// expiry: a fresh third node's plain unversioned read of its own warm cache
	// is exactly what would have gone stale.
	nodeC := newRotationNode(t, pool, server.URL)
	require.Equal(t, "key-v2", nodeC.resolveUnversioned(t, tn.ID), "a cold cache always reads through")

	gw.accept("key-v3")
	require.NoError(t, nodeA.rotate(t, tn.ID, accountID, "key-v3"))
	require.Equal(t, "key-v2", nodeC.resolveUnversioned(t, tn.ID),
		"without the version floor a warm cache keeps serving the retired credential — this is the bug or#812 fixes")
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
	_, err = node.svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{
		AccountID: accountID,
		Credentials: map[string]string{
			"security_key":           "nmi-key-a",
			"webhook_signing_secret": "whsec-a",
		},
	})
	require.NoError(t, err)

	// Rotate ONLY the security key.
	gw.accept("nmi-key-b")
	cfg, err := node.svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{
		AccountID:   accountID,
		Credentials: map[string]string{"security_key": "nmi-key-b"},
	})
	require.NoError(t, err)
	require.Equal(t, 2, cfg.Credentials["security_key"].RotationVersion, "the rotated credential's floor must rise")
	require.Equal(t, 1, cfg.Credentials["webhook_signing_secret"].RotationVersion,
		"an untouched credential must keep the floor it already had")

	// Re-putting the SAME value is not a rotation: the store keeps the version,
	// so the floor must not drift upward and force pointless re-reads.
	cfg, err = node.svc.UpsertPaymentProviderConfig(ctx, tn.ID, "nmi", UpsertPaymentProviderConfigRequest{
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
