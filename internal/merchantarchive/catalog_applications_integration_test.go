//go:build integration

package merchantarchive

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/pkg/merchant"
	"github.com/stretchr/testify/require"
)

func TestCatalogApplicationArchivePreservesRevisionAndReceipt(t *testing.T) {
	source := archiveDB(t, "apply_archive_source")
	target := archiveDB(t, "apply_archive_target")
	mid := merchant.ID(uuid.New())
	provision(t, source, mid)
	provision(t, target, mid)
	ctx := merchant.WithID(t.Context(), mid)
	catalog, err := source.Gen(ctx).EnsureDefaultCatalog(ctx, mid.UUID())
	require.NoError(t, err)
	receipt := openrails.CatalogApplicationReceipt{ApplicationID: "deployment-one", CatalogID: openrails.CatalogID(catalog.ID).String(), BaseRevision: 1, AppliedRevision: 2, ProductsChanged: 1}
	raw, err := json.Marshal(receipt)
	require.NoError(t, err)
	digest := bytes.Repeat([]byte{0xab}, 32)
	require.NoError(t, source.Gen(ctx).InsertCatalogApplication(ctx, gen.InsertCatalogApplicationParams{MerchantID: mid.UUID(), ApplicationID: receipt.ApplicationID, CatalogID: catalog.ID, SchemaVersion: 1, RequestSha256: digest, BaseRevision: 1, AppliedRevision: 2, Result: raw}))
	_, err = source.Qx(ctx).Exec(ctx, "UPDATE openrails.merchants SET catalog_revision=2 WHERE id=$1", mid.UUID())
	require.NoError(t, err)
	var artifact bytes.Buffer
	require.NoError(t, Export(ctx, source, mid, &artifact))
	result, err := Restore(ctx, target, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.False(t, result.Replayed)
	revision, err := target.Gen(ctx).GetCatalogRevision(ctx, mid.UUID())
	require.NoError(t, err)
	require.EqualValues(t, 2, revision)
	restored, err := target.Gen(ctx).GetCatalogApplication(ctx, gen.GetCatalogApplicationParams{MerchantID: mid.UUID(), ApplicationID: receipt.ApplicationID})
	require.NoError(t, err)
	require.Equal(t, digest, restored.RequestSha256)
	require.JSONEq(t, string(raw), string(restored.Result))
	_, err = target.Gen(ctx).AdvanceCatalogRevision(ctx, mid.UUID())
	require.NoError(t, err)
	result, err = Restore(ctx, target, mid, bytes.NewReader(artifact.Bytes()))
	require.NoError(t, err)
	require.True(t, result.Replayed)
	revision, err = target.Gen(ctx).GetCatalogRevision(ctx, mid.UUID())
	require.NoError(t, err)
	require.EqualValues(t, 3, revision, "restore replay cannot reset revision")
}
